// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/database/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testRoot = "byok integration root"

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if host := parsed.Hostname(); host != "localhost" && host != "127.0.0.1" && host != "::1" {
		t.Fatal("integration tests require a loopback PostgreSQL host")
	}
	if strings.Trim(parsed.Path, "/") != "notes_test" {
		t.Fatal("integration tests require a disposable database named notes_test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "byok_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
	})
	if err := migrations.Apply(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool
}

type fixture struct {
	pool      *pgxpool.Pool
	store     *ProfileStore
	workspace string
	owner     string
	member    string
}

func newFixture(t *testing.T, deployment string) fixture {
	t.Helper()
	pool := integrationPool(t)
	ctx := context.Background()
	f := fixture{pool: pool, workspace: "ws-" + uuid.NewString(), owner: "owner-" + uuid.NewString(), member: "member-" + uuid.NewString()}
	for _, user := range []string{f.owner, f.member} {
		if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,email) VALUES($1,$1,$2)`, user, user+"@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES($1)`, f.workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_members(workspace_id,user_id,role,state) VALUES($1,$2,'owner','active'),($1,$3,'member','active')`, f.workspace, f.owner, f.member); err != nil {
		t.Fatal(err)
	}
	f.store = NewProfileStore(pool, "", deployment, func(context.Context) (string, error) { return testRoot, nil })
	return f
}

func (f fixture) grant(t *testing.T, targetType, id, plan string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO entitlements(id,target_type,target_id,source,plan,status) VALUES($1,$2,$3,'admin_grant',$4,'active')`,
		uuid.NewString(), targetType, id, plan); err != nil {
		t.Fatal(err)
	}
}

func chatDefinition(enabled bool) ProfileDefinitionInput {
	return defaultDef(ModelDeclaration{ModelID: "gpt-6.1-sol", Enabled: enabled, Capabilities: []Capability{textCap()}})
}

func (f fixture) create(t *testing.T, name, credential string) Profile {
	t.Helper()
	p, err := f.store.Create(context.Background(), CreateProfileInput{WorkspaceID: f.workspace, Provider: "openai", Name: name,
		Credential: credential, Definition: chatDefinition(true), Enabled: true, ActorUserID: f.owner})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func decryptStored(t *testing.T, p Profile, identity string) string {
	t.Helper()
	key := mustKey(t, testRoot)
	credential, err := key.Decrypt(p.EncryptedCredential, ServerAAD(p.WorkspaceID, p.ProfileID, p.Provider, identity))
	if err != nil {
		t.Fatalf("decrypt stored: %v", err)
	}
	return string(credential.Bytes())
}

func TestEntitlementRules(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "cloud")
	if e, err := f.store.Entitlement(ctx, f.workspace, f.member); err != nil || e.Server || e.Local {
		t.Fatalf("none = %+v, %v", e, err)
	}
	f.grant(t, "user", f.member, "pro")
	if e, _ := f.store.Entitlement(ctx, f.workspace, f.member); e.Server || !e.Local {
		t.Fatalf("actor only = %+v", e)
	}
	f.grant(t, "user", f.owner, "pro")
	if e, _ := f.store.Entitlement(ctx, f.workspace, ""); !e.Server || !e.Local {
		t.Fatalf("owner = %+v", e)
	}
	if e, err := f.store.Entitlement(ctx, "missing", f.owner); err != nil || e.Server || e.Local {
		t.Fatalf("missing = %+v, %v", e, err)
	}
	g := newFixture(t, "cloud")
	g.grant(t, "workspace", g.workspace, "team")
	if e, _ := g.store.Entitlement(ctx, g.workspace, g.member); !e.Server || !e.Local {
		t.Fatalf("workspace = %+v", e)
	}
	if _, err := g.pool.Exec(ctx, `UPDATE workspace_members SET state='left' WHERE role='owner'`); err != nil {
		t.Fatal(err)
	}
	if _, err := g.store.Entitlement(ctx, g.workspace, g.member); !errors.Is(err, ErrWorkspaceOwnerNotFound) {
		t.Fatalf("no owner err = %v", err)
	}
	h := newFixture(t, "selfhosted")
	if e, _ := h.store.Entitlement(ctx, h.workspace, h.member); !e.Server || !e.Local {
		t.Fatalf("selfhosted = %+v", e)
	}
}

func TestProfileLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "selfhosted")
	var wg sync.WaitGroup
	orders := make(chan int64, 3)
	for _, name := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			orders <- f.create(t, name, "secret-"+name).SortOrder
		}()
	}
	wg.Wait()
	close(orders)
	seen := map[int64]bool{}
	for order := range orders {
		seen[order] = true
	}
	if !seen[0] || !seen[1] || !seen[2] {
		t.Fatalf("sort orders = %v", seen)
	}
	if _, err := f.store.Create(ctx, CreateProfileInput{WorkspaceID: f.workspace, Provider: "openai", Name: "a", Credential: "x", Definition: chatDefinition(true), ActorUserID: f.owner}); !errors.Is(err, ErrProfileNameConflict) {
		t.Fatalf("duplicate = %v", err)
	}
	profiles, err := f.store.List(ctx, f.workspace)
	if err != nil || len(profiles) != 3 {
		t.Fatal(profiles, err)
	}
	first := profiles[0]
	if first.Revision != 1 || first.CredentialGeneration != 1 || first.Validation != nil {
		t.Fatalf("new profile = %+v", first)
	}
	decryptStored(t, first, "default")

	// Replace errors.
	replace := ReplaceProfileInput{WorkspaceID: f.workspace, ProfileID: first.ProfileID, ExpectedRevision: 2, Name: first.Name, Definition: chatDefinition(true), Enabled: true, ActorUserID: f.owner}
	if _, err := f.store.Replace(ctx, replace); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale = %v", err)
	}
	missing := replace
	missing.ProfileID, missing.ExpectedRevision = uuid.NewString(), 1
	if _, err := f.store.Replace(ctx, missing); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("missing = %v", err)
	}
	rename := replace
	rename.ExpectedRevision, rename.Name = 1, profiles[1].Name
	if _, err := f.store.Replace(ctx, rename); !errors.Is(err, ErrProfileNameConflict) {
		t.Fatalf("rename = %v", err)
	}

	// Validation reconciliation for an unchanged endpoint.
	two := defaultDef(ModelDeclaration{ModelID: "gpt-6.1-sol", Enabled: true, Capabilities: []Capability{textCap()}},
		ModelDeclaration{ModelID: "gpt-6-luna", Enabled: true, Capabilities: []Capability{textCap()}})
	replace.ExpectedRevision, replace.Definition = 1, two
	updated, err := f.store.Replace(ctx, replace)
	if err != nil {
		t.Fatal(err)
	}
	evidence := Validation{DefinitionFingerprint: "x", CredentialGeneration: 1, Connection: ProbeStatus{Kind: "verified"},
		Models: []ModelProbe{{ModelID: "gpt-6.1-sol", Checks: []ModelProbeCheck{}}, {ModelID: "gpt-6-luna", Checks: []ModelProbeCheck{}}}}
	encoded, _ := json.Marshal(evidence)
	if _, err := f.pool.Exec(ctx, `UPDATE ai_workspace_byok_configs SET validation=$2 WHERE id=$1`, first.ProfileID, encoded); err != nil {
		t.Fatal(err)
	}
	changed := defaultDef(ModelDeclaration{ModelID: "gpt-6.1-sol", Enabled: true, Capabilities: []Capability{textCap()}},
		ModelDeclaration{ModelID: "gpt-6-luna", Enabled: false, Capabilities: []Capability{textCap()}})
	replace.ExpectedRevision, replace.Definition = updated.Revision, changed
	updated, err = f.store.Replace(ctx, replace)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Validation == nil || len(updated.Validation.Models) != 1 || updated.Validation.Models[0].ModelID != "gpt-6.1-sol" ||
		updated.Validation.DefinitionFingerprint != definitionFingerprint(updated.Definition) {
		t.Fatalf("reconciled = %+v", updated.Validation)
	}

	// Endpoint change re-encrypts without a new credential and clears evidence.
	f.store.runtimePath = writeRuntimeConfig(t, `{"copilot":{"byok.allowCustomEndpoint":true}}`)
	f.store.SetEndpointResolver(staticResolver("93.184.216.34"))
	replace.ExpectedRevision, replace.Definition = updated.Revision, customDef(model("custom", textCap()))
	updated, err = f.store.Replace(ctx, replace)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CredentialGeneration != 1 || updated.Validation != nil || decryptStored(t, updated, "https://llm.example.com/v1") != "secret-"+first.Name {
		t.Fatalf("endpoint change = %+v", updated)
	}

	rotated, err := f.store.Rotate(ctx, RotateCredentialInput{WorkspaceID: f.workspace, ProfileID: first.ProfileID, ExpectedRevision: updated.Revision, Credential: "rotated", ActorUserID: f.owner})
	if err != nil || rotated.CredentialGeneration != 2 || rotated.Revision != updated.Revision+1 || rotated.Validation != nil {
		t.Fatalf("rotate = %+v, %v", rotated, err)
	}
	if decryptStored(t, rotated, "https://llm.example.com/v1") != "rotated" {
		t.Fatal("rotated credential")
	}
	if _, err := f.store.Rotate(ctx, RotateCredentialInput{WorkspaceID: f.workspace, ProfileID: first.ProfileID, ExpectedRevision: 1, Credential: "x", ActorUserID: f.owner}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale rotate = %v", err)
	}

	// Reorder.
	profiles, _ = f.store.List(ctx, f.workspace)
	order := []ProfileOrderInput{{profiles[2].ProfileID, profiles[2].Revision}, {profiles[1].ProfileID, profiles[1].Revision}}
	if _, err := f.store.Reorder(ctx, f.workspace, f.owner, order); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("partial reorder = %v", err)
	}
	stale := append(order, ProfileOrderInput{profiles[0].ProfileID, profiles[0].Revision + 5})
	if _, err := f.store.Reorder(ctx, f.workspace, f.owner, stale); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale reorder = %v", err)
	}
	if after, _ := f.store.List(ctx, f.workspace); after[0].Revision != profiles[0].Revision || after[0].ProfileID != profiles[0].ProfileID {
		t.Fatal("stale reorder changed rows")
	}
	full := append(order, ProfileOrderInput{profiles[0].ProfileID, profiles[0].Revision})
	reordered, err := f.store.Reorder(ctx, f.workspace, f.owner, full)
	if err != nil || reordered[0].ProfileID != profiles[2].ProfileID || reordered[0].SortOrder != 0 || reordered[2].Revision != profiles[0].Revision+1 {
		t.Fatalf("reorder = %+v, %v", reordered, err)
	}

	if ok, err := f.store.Delete(ctx, f.workspace, first.ProfileID); err != nil || !ok {
		t.Fatal("delete")
	}
	if ok, err := f.store.Delete(ctx, f.workspace, first.ProfileID); err != nil || ok {
		t.Fatal("delete again")
	}
}

func TestProbesPersistOnlyCurrentEvidence(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "selfhosted")
	p := f.create(t, "probe", "secret")
	f.store.SetProbeRunner(probeFunc(func(_ context.Context, target ProbeTarget) ProbeStatus {
		if string(target.Credential) != "secret" {
			t.Errorf("credential = %q", target.Credential)
		}
		return ProbeStatus{Kind: "verified", TestedAtMS: nowMS()}
	}))
	checks := []ProbeCheckInput{{"gpt-6.1-sol", "chat"}}
	result, err := f.store.ProbeProfile(ctx, ProbeProfileInput{WorkspaceID: f.workspace, ProfileID: p.ProfileID, Checks: checks})
	if err != nil || result.Stale || result.Connection.Kind != "verified" {
		t.Fatalf("probe = %+v, %v", result, err)
	}
	stored, _ := f.store.List(ctx, f.workspace)
	if stored[0].Validation == nil || stored[0].Validation.DefinitionFingerprint != result.DefinitionFingerprint || stored[0].Revision != 1 {
		t.Fatalf("persisted = %+v", stored[0])
	}
	f.store.SetProbeRunner(probeFunc(func(ctx context.Context, _ ProbeTarget) ProbeStatus {
		if _, err := f.pool.Exec(ctx, `UPDATE ai_workspace_byok_configs SET revision=revision+1 WHERE id=$1`, p.ProfileID); err != nil {
			t.Error(err)
		}
		return ProbeStatus{Kind: "verified", TestedAtMS: nowMS()}
	}))
	if result, err := f.store.ProbeProfile(ctx, ProbeProfileInput{WorkspaceID: f.workspace, ProfileID: p.ProfileID, Checks: checks}); err != nil || !result.Stale {
		t.Fatalf("stale probe = %+v, %v", result, err)
	}
	if _, err := f.store.ProbeProfile(ctx, ProbeProfileInput{WorkspaceID: f.workspace, ProfileID: uuid.NewString(), Checks: checks}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing profile = %v", err)
	}
	both := ProbeDraftInput{WorkspaceID: f.workspace, Provider: "openai", Credential: ptr("x"), ProfileID: &p.ProfileID, ExpectedRevision: ptr(int64(2)), Definition: chatDefinition(true), Checks: checks}
	if _, err := f.store.ProbeDraft(ctx, both); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("both sources = %v", err)
	}
	wrongProvider := ProbeDraftInput{WorkspaceID: f.workspace, Provider: "gemini", ProfileID: &p.ProfileID, ExpectedRevision: ptr(int64(2)),
		Definition: defaultDef(model("gemini-3.8-flash", textCap())), Checks: []ProbeCheckInput{{"gemini-3.8-flash", "chat"}}}
	if _, err := f.store.ProbeDraft(ctx, wrongProvider); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("wrong provider = %v", err)
	}
	f.store.SetProbeRunner(probeFunc(func(context.Context, ProbeTarget) ProbeStatus {
		return ProbeStatus{Kind: "verified", TestedAtMS: nowMS()}
	}))
	stored_ := ProbeDraftInput{WorkspaceID: f.workspace, Provider: "openai", ProfileID: &p.ProfileID, ExpectedRevision: ptr(int64(2)), Definition: chatDefinition(true), Checks: checks}
	if result, err := f.store.ProbeDraft(ctx, stored_); err != nil || result.Connection.Kind != "verified" {
		t.Fatalf("stored draft = %+v, %v", result, err)
	}
}

func TestResolveTargets(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "cloud")
	p := f.create(t, "chat", "chat-secret")
	if _, err := f.store.ResolveChatTarget(ctx, f.workspace, f.owner, p.ProfileID, "gpt-6.1-sol"); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("unentitled = %v", err)
	}
	if _, err := f.store.ResolveServerTarget(ctx, f.workspace, f.owner, "embedding"); !errors.Is(err, ErrNoCompatibleServerTarget) {
		t.Fatalf("unentitled server = %v", err)
	}
	f.grant(t, "user", f.owner, "pro")
	target, err := f.store.ResolveChatTarget(ctx, f.workspace, f.owner, p.ProfileID, "gpt-6.1-sol")
	if err != nil || target.BaseURL != "https://api.openai.com/v1" || target.Source != "byok_server" || string(target.Credential.Bytes()) != "chat-secret" || target.Transport != nil {
		t.Fatalf("target = %+v, %v", target, err)
	}
	target.Credential.Destroy()
	for _, id := range []string{"", uuid.NewString()} {
		if _, err := f.store.ResolveChatTarget(ctx, f.workspace, f.owner, id, "gpt-6.1-sol"); !errors.Is(err, ErrTargetUnavailable) {
			t.Fatalf("bad id = %v", err)
		}
	}
	if _, err := f.store.ResolveImageTarget(ctx, f.workspace, f.owner, p.ProfileID, "gpt-6.1-sol", "image_edit"); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("image on chat model = %v", err)
	}
	broken := NewProfileStore(f.pool, "", "cloud", func(context.Context) (string, error) { return "other root", nil })
	if _, err := broken.ResolveChatTarget(ctx, f.workspace, f.owner, p.ProfileID, "gpt-6.1-sol"); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("bad root = %v", err)
	}
	noKey := NewProfileStore(f.pool, "", "cloud", nil)
	if _, err := noKey.ResolveChatTarget(ctx, f.workspace, f.owner, p.ProfileID, "gpt-6.1-sol"); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("nil loader = %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO app_configs(id,value,updated_at) VALUES('copilot.byok.allowedProviders','["gemini"]'::jsonb,now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResolveChatTarget(ctx, f.workspace, f.owner, p.ProfileID, "gpt-6.1-sol"); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("policy disallowed = %v", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM app_configs WHERE id='copilot.byok.allowedProviders'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE ai_workspace_byok_configs SET enabled=false WHERE id=$1`, p.ProfileID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResolveChatTarget(ctx, f.workspace, f.owner, p.ProfileID, "gpt-6.1-sol"); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("disabled profile = %v", err)
	}

	embedDef := defaultDef(model("gpt-6.1-sol", textCap()), model("text-embedding-3-small", Capability{Input: []string{"text"}, Output: []string{"embedding"}}))
	first, err := f.store.Create(ctx, CreateProfileInput{WorkspaceID: f.workspace, Provider: "openai", Name: "embed-1", Credential: "e1", Definition: embedDef, Enabled: true, ActorUserID: f.owner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Create(ctx, CreateProfileInput{WorkspaceID: f.workspace, Provider: "openai", Name: "embed-2", Credential: "e2", Definition: embedDef, Enabled: true, ActorUserID: f.owner}); err != nil {
		t.Fatal(err)
	}
	target, err = f.store.ResolveServerTarget(ctx, f.workspace, f.owner, "embedding")
	if err != nil || target.ProfileID != first.ProfileID || target.ModelID != "text-embedding-3-small" || string(target.Credential.Bytes()) != "e1" {
		t.Fatalf("server target = %+v, %v", target, err)
	}
	if _, err := f.store.ResolveServerTarget(ctx, f.workspace, f.owner, "transcript"); !errors.Is(err, ErrNoCompatibleServerTarget) || !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("no transcript = %v", err)
	}
	if _, err := f.store.ResolveServerTarget(ctx, f.workspace, f.owner, "dance"); !errors.Is(err, ErrTargetUnavailable) || errors.Is(err, ErrNoCompatibleServerTarget) {
		t.Fatalf("unknown op = %v", err)
	}
}

func TestCustomEndpointTargetTransport(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "selfhosted")
	f.store.runtimePath = writeRuntimeConfig(t, `{"copilot":{"byok.allowCustomEndpoint":true}}`)
	addr := netip.MustParseAddr("93.184.216.34")
	f.store.SetEndpointResolver(func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{addr}, nil })
	p, err := f.store.Create(ctx, CreateProfileInput{WorkspaceID: f.workspace, Provider: "openai", Name: "custom", Credential: "c",
		Definition: customDef(model("own", textCap())), Enabled: true, ActorUserID: f.owner})
	if err != nil {
		t.Fatal(err)
	}
	target, err := f.store.ResolveChatTarget(ctx, f.workspace, f.owner, p.ProfileID, "own")
	if err != nil || target.BaseURL != "https://llm.example.com/v1" || target.Transport == nil {
		t.Fatalf("custom target = %+v, %v", target, err)
	}
	target.Credential.Destroy()
	if _, err := newPinnedTransport("https", false, nil, "llm.example.com", []netip.Addr{netip.MustParseAddr("10.0.0.1")}).DialContext(ctx, "tcp", "llm.example.com:443"); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("private dial = %v", err)
	}
}

func TestLocalLeases(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "cloud")
	f.grant(t, "user", f.member, "pro")
	input := CreateLocalLeaseInput{WorkspaceID: f.workspace, UserID: f.member, Providers: []LocalLeaseProviderInput{{
		Provider: "openai", Name: "Local", Credential: "local-secret", Definition: chatDefinition(true), Enabled: true,
	}}}
	lease, err := f.store.CreateLocalLease(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(time.UnixMilli(lease.ExpiresAtMS)); remaining < 9*time.Minute || remaining > 10*time.Minute+time.Second {
		t.Fatalf("remaining = %s", remaining)
	}
	again, err := f.store.CreateLocalLease(ctx, input)
	if err != nil || again != lease {
		t.Fatalf("again = %+v, %v", again, err)
	}
	var raw []byte
	if err := f.pool.QueryRow(ctx, `SELECT payload FROM runtime_states WHERE purpose='copilot_byok_local_lease' AND token_hash=$1`, sha256Hex([]byte(lease.LeaseID))).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		WorkspaceID string `json:"workspaceId"`
		UserID      string `json:"userId"`
		Providers   []struct {
			Provider            string            `json:"provider"`
			Name                string            `json:"name"`
			EncryptedCredential string            `json:"encryptedCredential"`
			Definition          ProfileDefinition `json:"definition"`
			Enabled             bool              `json:"enabled"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || strings.Contains(string(raw), "local-secret") || payload.UserID != f.member || payload.Providers[0].Name != "Local" {
		t.Fatalf("payload = %s, %v", raw, err)
	}
	credential, err := mustKey(t, testRoot).Decrypt(payload.Providers[0].EncryptedCredential, LocalAAD(f.workspace, f.member, lease.LeaseID, 0, "openai", "default"))
	if err != nil || string(credential.Bytes()) != "local-secret" {
		t.Fatalf("decrypt = %v", err)
	}

	target, err := f.store.ResolveLocalChatTarget(ctx, f.workspace, f.member, lease.LeaseID, lease.LeaseID+":0", "gpt-6.1-sol")
	if err != nil || target.Source != "byok_local" || target.ProfileID != lease.LeaseID+":0" || string(target.Credential.Bytes()) != "local-secret" {
		t.Fatalf("local target = %+v, %v", target, err)
	}
	other := newFixture(t, "cloud")
	for name, attempt := range map[string][5]string{
		"wrong user":      {f.workspace, f.owner, lease.LeaseID, lease.LeaseID + ":0", "gpt-6.1-sol"},
		"wrong workspace": {other.workspace, f.member, lease.LeaseID, lease.LeaseID + ":0", "gpt-6.1-sol"},
		"out of range":    {f.workspace, f.member, lease.LeaseID, lease.LeaseID + ":1", "gpt-6.1-sol"},
		"malformed":       {f.workspace, f.member, lease.LeaseID, lease.LeaseID + ":-1", "gpt-6.1-sol"},
		"other lease":     {f.workspace, f.member, "other", lease.LeaseID + ":0", "gpt-6.1-sol"},
		"model":           {f.workspace, f.member, lease.LeaseID, lease.LeaseID + ":0", "missing"},
	} {
		if _, err := f.store.ResolveLocalChatTarget(ctx, attempt[0], attempt[1], attempt[2], attempt[3], attempt[4]); !errors.Is(err, ErrTargetUnavailable) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	changed := input
	changed.Providers = []LocalLeaseProviderInput{input.Providers[0]}
	changed.Providers[0].Credential = "changed-secret"
	fresh, err := f.store.CreateLocalLease(ctx, changed)
	if err != nil || fresh.LeaseID == lease.LeaseID {
		t.Fatalf("changed = %+v, %v", fresh, err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE runtime_states SET expires_at=now()-interval '1 second' WHERE token_hash=$1`, sha256Hex([]byte(lease.LeaseID))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResolveLocalChatTarget(ctx, f.workspace, f.member, lease.LeaseID, lease.LeaseID+":0", "gpt-6.1-sol"); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("expired = %v", err)
	}
	if renewed, err := f.store.CreateLocalLease(ctx, input); err != nil || renewed.LeaseID == lease.LeaseID {
		t.Fatalf("after expiry = %+v, %v", renewed, err)
	}
	failing := NewProfileStore(f.pool, "", "cloud", func(context.Context) (string, error) { return "", errors.New("boom") })
	if _, err := failing.CreateLocalLease(ctx, input); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("key failure = %v", err)
	}
}

func TestAggregateUsageByDay(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "cloud")
	if _, err := f.pool.Exec(ctx, `INSERT INTO ai_usage_events(id,workspace_id,provider,provider_source,feature_kind,total_tokens,created_at) VALUES
		('a',$1,'openai','byok_server','chat',9,'2026-01-01T23:30:00Z'),
		('b',$1,'openai','byok_local','chat',3,'2026-01-01T10:00:00Z'),
		('c',$1,'openai','byok_server','summarize',7,'2026-01-02T02:00:00Z'),
		('d',$1,'openai','managed','chat',100,'2026-01-01T12:00:00Z'),
		('e',$1,'openai','byok_server','chat',100,'2026-01-03T00:00:00Z')`, f.workspace); err != nil {
		t.Fatal(err)
	}
	from, to := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	points, err := f.store.AggregateUsageByDay(ctx, f.workspace, from, to)
	if err != nil || len(points) != 2 || !points[0].Date.Equal(from) || points[0].TotalTokens != 12 || points[1].FeatureKind != "summarize" {
		t.Fatalf("points = %+v, %v", points, err)
	}
	if empty, err := f.store.AggregateUsageByDay(ctx, "none", from, to); err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty = %#v, %v", empty, err)
	}
}
