// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package calendar

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const maxCalDAVResponseBytes = 50 << 20

type Database interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
}

type PrivateKeyLoader func(context.Context) (string, error)
type Resolver func(context.Context, string) ([]netip.Addr, error)

type LinkInput struct {
	ProviderPresetID string
	Username         string
	Password         string
	DisplayName      *string
}

type Account struct{ ID string }

type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string { return e.Message }

type ProviderConfig struct {
	Enabled             bool             `json:"enabled"`
	AllowCustomProvider bool             `json:"allowCustomProvider"`
	Providers           []ProviderPreset `json:"providers"`
	AllowInsecureHTTP   bool             `json:"allowInsecureHttp"`
	AllowedHosts        []string         `json:"allowedHosts"`
	BlockPrivateNetwork *bool            `json:"blockPrivateNetwork"`
	RequestTimeoutMS    int              `json:"requestTimeoutMs"`
	MaxRedirects        *int             `json:"maxRedirects"`
	maxRedirects        int
}

type ProviderPreset struct {
	ID                  string `json:"id"`
	Label               string `json:"label"`
	ServerURL           string `json:"serverUrl"`
	AuthType            string `json:"authType"`
	RequiresAppPassword bool   `json:"requiresAppPassword"`
	DocsURL             string `json:"docsUrl"`
}

type Service struct {
	db          Database
	runtimePath string
	loadKey     PrivateKeyLoader
	resolve     Resolver
}

func NewService(db Database, runtimePath string, loadKey PrivateKeyLoader) *Service {
	return &Service{db: db, runtimePath: runtimePath, loadKey: loadKey, resolve: defaultResolver}
}

func (s *Service) SetResolver(resolve Resolver) {
	if resolve != nil {
		s.resolve = resolve
	}
}

func (s *Service) LinkCalDAVAccount(ctx context.Context, userID string, input LinkInput) (Account, error) {
	if s.db == nil || s.loadKey == nil {
		return Account{}, errors.New("CalDAV service is unavailable")
	}
	providerConfig, err := s.providerConfig(ctx)
	if err != nil {
		return Account{}, err
	}
	if !providerConfig.Enabled {
		return Account{}, &Error{Code: "caldav_disabled", Message: "CalDAV integration is not enabled."}
	}
	var preset *ProviderPreset
	for i := range providerConfig.Providers {
		if providerConfig.Providers[i].ID == input.ProviderPresetID {
			preset = &providerConfig.Providers[i]
			break
		}
	}
	if preset == nil {
		return Account{}, &Error{Code: "caldav_provider_not_found", Message: "CalDAV provider is not available."}
	}
	if strings.TrimSpace(input.Username) == "" || input.Password == "" {
		return Account{}, &Error{Code: "caldav_invalid_credentials", Message: "CalDAV username and password are required."}
	}
	client := newClient(providerConfig, *preset, input.Username, input.Password, s.resolve)
	defer client.client.CloseIdleConnections()
	discoveryURL, err := client.resolveDiscoveryURL(ctx)
	if err != nil {
		return Account{}, err
	}
	principalURL, err := client.currentPrincipal(ctx, discoveryURL)
	if err != nil {
		return Account{}, err
	}
	homeURL, err := client.calendarHome(ctx, principalURL, discoveryURL)
	if err != nil {
		return Account{}, err
	}
	calendars, err := client.listCalendars(ctx, homeURL)
	if err != nil {
		return Account{}, err
	}
	key, err := s.loadKey(ctx)
	if err != nil || strings.TrimSpace(key) == "" {
		return Account{}, &Error{Code: "caldav_encryption_unavailable", Message: "Calendar credential encryption is unavailable."}
	}
	encryptedPassword, err := encryptCredential(key, input.Password)
	if err != nil {
		return Account{}, err
	}
	providerAccountID := principalURL
	if providerAccountID == "" {
		parsed, _ := url.Parse(discoveryURL)
		providerAccountID = input.Username + "@" + parsed.Hostname()
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback(ctx)
	accountID := uuid.NewString()
	var displayName any
	if input.DisplayName != nil {
		displayName = *input.DisplayName
	}
	err = tx.QueryRow(ctx, `INSERT INTO calendar_accounts(
		id,user_id,provider,provider_account_id,display_name,email,provider_preset_id,
		server_url,principal_url,calendar_home_url,username,auth_type,access_token,
		refresh_token,expires_at,scope,status,last_error,created_at,updated_at)
		VALUES($1,$2,'CalDAV',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULL,NULL,NULL,'active',NULL,now(),now())
		ON CONFLICT(user_id,provider,provider_account_id) DO UPDATE SET
		display_name=excluded.display_name,email=excluded.email,provider_preset_id=excluded.provider_preset_id,
		server_url=excluded.server_url,principal_url=excluded.principal_url,calendar_home_url=excluded.calendar_home_url,
		username=excluded.username,auth_type=excluded.auth_type,access_token=excluded.access_token,
		status='active',last_error=NULL,updated_at=now() RETURNING id`, accountID, userID,
		providerAccountID, displayName, input.Username, input.ProviderPresetID, discoveryURL,
		principalURL, homeURL, input.Username, client.authType, encryptedPassword).Scan(&accountID)
	if err != nil {
		return Account{}, err
	}
	for _, calendar := range calendars {
		_, err = tx.Exec(ctx, `INSERT INTO calendar_subscriptions(
			id,account_id,provider,external_calendar_id,display_name,timezone,color,enabled,next_sync_at,created_at,updated_at)
			VALUES($1,$2,'CalDAV',$3,$4,$5,$6,true,now(),now(),now())
			ON CONFLICT(account_id,external_calendar_id) DO UPDATE SET display_name=excluded.display_name,
			timezone=excluded.timezone,color=excluded.color,enabled=true,
			next_sync_at=LEAST(calendar_subscriptions.next_sync_at,now()),updated_at=now()`,
			uuid.NewString(), accountID, calendar.ID, calendar.DisplayName, calendar.Timezone, calendar.Color)
		if err != nil {
			return Account{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, err
	}
	if err := s.SyncAccountNow(ctx, accountID); err != nil {
		return Account{ID: accountID}, err
	}
	return Account{ID: accountID}, nil
}

func (s *Service) providerConfig(ctx context.Context) (ProviderConfig, error) {
	raw, found, err := config.ReadNodeRuntimeValue(ctx, s.db, s.runtimePath, "calendar.caldav")
	if err != nil {
		return ProviderConfig{}, err
	}
	if !found {
		return ProviderConfig{}, nil
	}
	var value ProviderConfig
	if err := json.Unmarshal(raw, &value); err != nil {
		return ProviderConfig{}, &Error{Code: "caldav_config_invalid", Message: "CalDAV configuration is invalid."}
	}
	if value.RequestTimeoutMS <= 0 {
		value.RequestTimeoutMS = 10_000
	}
	maxRedirects := 5
	if value.MaxRedirects != nil {
		maxRedirects = *value.MaxRedirects
	}
	value.maxRedirects = maxRedirects
	return value, nil
}

type caldavClient struct {
	config     ProviderConfig
	preset     ProviderPreset
	user       string
	pass       string
	resolve    Resolver
	client     *http.Client
	authType   string
	nonceCount uint32
}

func newClient(cfg ProviderConfig, preset ProviderPreset, user, pass string, resolver Resolver) *caldavClient {
	authType := preset.AuthType
	if authType == "" {
		authType = "auto"
	}
	c := &caldavClient{config: cfg, preset: preset, user: user, pass: pass, resolve: resolver, authType: authType, nonceCount: 1}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = c.dial
	c.client = &http.Client{
		Transport: transport,
		Timeout:   time.Duration(cfg.RequestTimeoutMS) * time.Millisecond,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > cfg.maxRedirects {
				return &Error{Code: "caldav_max_redirects", Message: "CalDAV request exceeded redirect limit."}
			}
			return c.validateURL(req.URL)
		},
	}
	return c
}

func (c *caldavClient) validateURL(target *url.URL) error {
	if target == nil || target.User != nil || target.Hostname() == "" || target.Fragment != "" {
		return &Error{Code: "caldav_invalid_url", Message: "CalDAV URL is invalid."}
	}
	if target.Scheme != "https" && !(target.Scheme == "http" && c.config.AllowInsecureHTTP) {
		return &Error{Code: "caldav_insecure_url", Message: "CalDAV URL must use https."}
	}
	if len(c.config.AllowedHosts) == 0 {
		return nil
	}
	host := strings.ToLower(target.Hostname())
	for _, item := range c.config.AllowedHosts {
		rule := strings.ToLower(strings.TrimSpace(item))
		if strings.HasPrefix(rule, "*.") {
			suffix := strings.TrimPrefix(rule, "*")
			if host == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(host, suffix) {
				return nil
			}
		} else if host == rule || strings.HasSuffix(host, "."+rule) {
			return nil
		}
	}
	return &Error{Code: "caldav_host_blocked", Message: "CalDAV host is not allowed."}
}

func (c *caldavClient) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := c.resolve(ctx, host)
	if err != nil || len(addresses) == 0 {
		return nil, &Error{Code: "caldav_dns_failed", Message: "Unable to resolve CalDAV host."}
	}
	blockPrivate := c.config.BlockPrivateNetwork == nil || *c.config.BlockPrivateNetwork
	for _, ip := range addresses {
		if unsafeAddress(ip, blockPrivate) {
			return nil, &Error{Code: "caldav_private_network", Message: "CalDAV host is in a private network."}
		}
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, ip := range addresses {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	return nil, lastErr
}

func unsafeAddress(ip netip.Addr, blockPrivate bool) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	return blockPrivate && (!ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || sharedAddressSpace.Contains(ip))
}

var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

func defaultResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	return addresses, nil
}

func (c *caldavClient) request(ctx context.Context, method, target, depth, body string) (*http.Response, error) {
	u, err := url.Parse(target)
	if err != nil || c.validateURL(u) != nil {
		return nil, &Error{Code: "caldav_invalid_url", Message: "CalDAV URL is invalid."}
	}
	prepare := func(authHeader string) *http.Request {
		r, _ := http.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
		if authHeader != "" {
			r.Header.Set("Authorization", authHeader)
		}
		if depth != "" {
			r.Header.Set("Depth", depth)
		}
		if body != "" {
			r.Header.Set("Content-Type", "application/xml; charset=utf-8")
		}
		return r
	}
	var authHeader string
	if c.authType == "basic" || c.authType == "auto" {
		authHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(c.user+":"+c.pass))
	}
	response, err := c.client.Do(prepare(authHeader))
	if err != nil {
		var policyErr *Error
		if errors.As(err, &policyErr) {
			return nil, policyErr
		}
		return nil, &Error{Code: "caldav_request_failed", Message: "CalDAV request failed."}
	}
	if response.StatusCode == http.StatusUnauthorized && (c.authType == "auto" || c.authType == "digest") {
		challenge := parseDigestChallenge(response.Header.Values("WWW-Authenticate"))
		if challenge != nil {
			response.Body.Close()
			header, digestErr := c.digestHeader(challenge, method, target)
			if digestErr != nil {
				return nil, digestErr
			}
			response, err = c.client.Do(prepare(header))
			if err != nil {
				return nil, &Error{Code: "caldav_request_failed", Message: "CalDAV request failed."}
			}
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				c.authType = "digest"
			}
		}
	} else if response.StatusCode >= 200 && response.StatusCode < 300 && c.authType == "auto" {
		c.authType = "basic"
	}
	return response, nil
}

type digestChallenge struct {
	Realm, Nonce, QOP, Algorithm, Opaque string
}

func parseDigestChallenge(values []string) *digestChallenge {
	for _, value := range values {
		start := strings.Index(strings.ToLower(value), "digest ")
		if start < 0 {
			continue
		}
		params := map[string]string{}
		for _, part := range strings.Split(value[start+7:], ",") {
			key, raw, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok {
				continue
			}
			params[strings.ToLower(key)] = strings.Trim(strings.TrimSpace(raw), `"`)
		}
		challenge := &digestChallenge{Realm: params["realm"], Nonce: params["nonce"], QOP: params["qop"], Algorithm: params["algorithm"], Opaque: params["opaque"]}
		if challenge.Nonce != "" {
			return challenge
		}
	}
	return nil
}

func (c *caldavClient) digestHeader(challenge *digestChallenge, method, target string) (string, error) {
	u, err := url.Parse(target)
	if err != nil {
		return "", &Error{Code: "caldav_invalid_url", Message: "CalDAV URL is invalid."}
	}
	algorithm := strings.ToLower(challenge.Algorithm)
	hash := func(value string) string {
		if strings.HasPrefix(algorithm, "sha-256") {
			sum := sha256.Sum256([]byte(value))
			return fmt.Sprintf("%x", sum)
		}
		sum := md5.Sum([]byte(value))
		return fmt.Sprintf("%x", sum)
	}
	qop := ""
	for _, value := range strings.Split(challenge.QOP, ",") {
		if strings.TrimSpace(value) == "auth" {
			qop = "auth"
			break
		}
	}
	cnonceBytes := make([]byte, 16)
	if _, err := rand.Read(cnonceBytes); err != nil {
		return "", err
	}
	cnonce := fmt.Sprintf("%x", cnonceBytes)
	uri := u.RequestURI()
	nc := fmt.Sprintf("%08x", c.nonceCount)
	c.nonceCount++
	ha1 := hash(c.user + ":" + challenge.Realm + ":" + c.pass)
	if strings.HasSuffix(algorithm, "-sess") {
		ha1 = hash(ha1 + ":" + challenge.Nonce + ":" + cnonce)
	}
	ha2 := hash(method + ":" + uri)
	response := ""
	if qop != "" {
		response = hash(ha1 + ":" + challenge.Nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
	} else {
		response = hash(ha1 + ":" + challenge.Nonce + ":" + ha2)
	}
	parts := []string{`username="` + c.user + `"`, `realm="` + challenge.Realm + `"`, `nonce="` + challenge.Nonce + `"`, `uri="` + uri + `"`, `response="` + response + `"`}
	if challenge.Opaque != "" {
		parts = append(parts, `opaque="`+challenge.Opaque+`"`)
	}
	if challenge.Algorithm != "" {
		parts = append(parts, "algorithm="+challenge.Algorithm)
	}
	if qop != "" {
		parts = append(parts, "qop=auth", "nc="+nc, `cnonce="`+cnonce+`"`)
	}
	return "Digest " + strings.Join(parts, ", "), nil
}

type xmlNode struct {
	XMLName  xml.Name
	Text     string    `xml:",chardata"`
	Children []xmlNode `xml:",any"`
}

func (n xmlNode) child(name string) xmlNode {
	for _, child := range n.Children {
		if child.XMLName.Local == name {
			return child
		}
	}
	return xmlNode{}
}

func (n xmlNode) find(name string) xmlNode {
	if n.XMLName.Local == name {
		return n
	}
	for _, child := range n.Children {
		if found := child.find(name); found.XMLName.Local != "" {
			return found
		}
	}
	return xmlNode{}
}

func decodeMultiStatus(response *http.Response) (xmlNode, error) {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCalDAVResponseBytes+1))
	if err != nil || len(body) > maxCalDAVResponseBytes {
		return xmlNode{}, &Error{Code: "caldav_response_invalid", Message: "CalDAV response is invalid."}
	}
	var root xmlNode
	if err := xml.Unmarshal(body, &root); err != nil {
		return xmlNode{}, &Error{Code: "caldav_response_invalid", Message: "CalDAV response is invalid."}
	}
	return root, nil
}

func (c *caldavClient) resolveDiscoveryURL(ctx context.Context) (string, error) {
	server, err := url.Parse(c.preset.ServerURL)
	if err != nil || c.validateURL(server) != nil {
		return "", &Error{Code: "caldav_invalid_url", Message: "CalDAV URL is invalid."}
	}
	wellKnown := *server
	wellKnown.Path = "/.well-known/caldav"
	response, err := c.request(ctx, http.MethodGet, wellKnown.String(), "", "")
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response.Request.URL.String(), nil
	}
	if response.StatusCode == 400 || response.StatusCode == 404 || response.StatusCode == 405 || response.StatusCode == 501 {
		return server.String(), nil
	}
	return "", providerError(response.StatusCode)
}

func (c *caldavClient) currentPrincipal(ctx context.Context, base string) (string, error) {
	body := `<?xml version="1.0" encoding="UTF-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:current-user-principal/></D:prop></D:propfind>`
	return c.propfindURL(ctx, base, body, "current-user-principal", base)
}

func (c *caldavClient) calendarHome(ctx context.Context, principal, base string) (string, error) {
	body := `<?xml version="1.0" encoding="UTF-8"?><D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-home-set/></D:prop></D:propfind>`
	return c.propfindURL(ctx, principal, body, "calendar-home-set", base)
}

func (c *caldavClient) propfindURL(ctx context.Context, target, body, property, base string) (string, error) {
	response, err := c.request(ctx, "PROPFIND", target, "0", body)
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return "", providerError(response.StatusCode)
	}
	root, err := decodeMultiStatus(response)
	if err != nil {
		return "", err
	}
	href := strings.TrimSpace(root.find(property).find("href").Text)
	if href == "" {
		return "", &Error{Code: "caldav_response_invalid", Message: "CalDAV discovery response is incomplete."}
	}
	baseURL, _ := url.Parse(base)
	relative, err := url.Parse(href)
	if err != nil {
		return "", &Error{Code: "caldav_response_invalid", Message: "CalDAV discovery response is invalid."}
	}
	return baseURL.ResolveReference(relative).String(), nil
}

type remoteCalendar struct {
	ID          string
	DisplayName *string
	Timezone    *string
	Color       *string
}

func (c *caldavClient) listCalendars(ctx context.Context, home string) ([]remoteCalendar, error) {
	body := `<?xml version="1.0" encoding="UTF-8"?><D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:IC="http://apple.com/ns/ical/"><D:prop><D:displayname/><D:resourcetype/><C:calendar-timezone/><IC:calendar-color/></D:prop></D:propfind>`
	response, err := c.request(ctx, "PROPFIND", home, "1", body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return nil, providerError(response.StatusCode)
	}
	root, err := decodeMultiStatus(response)
	if err != nil {
		return nil, err
	}
	homeURL, _ := url.Parse(home)
	items := []remoteCalendar{}
	for _, node := range root.Children {
		if node.XMLName.Local != "response" {
			continue
		}
		href := strings.TrimSpace(node.child("href").Text)
		if href == "" {
			continue
		}
		prop := node.find("prop")
		resource := prop.find("resourcetype")
		isCalendar := false
		for _, child := range resource.Children {
			if child.XMLName.Local == "calendar" {
				isCalendar = true
				break
			}
		}
		if !isCalendar {
			continue
		}
		calendarURL, parseErr := url.Parse(href)
		if parseErr != nil {
			continue
		}
		items = append(items, remoteCalendar{
			ID:          homeURL.ResolveReference(calendarURL).String(),
			DisplayName: optionalText(prop.child("displayname").Text),
			Timezone:    optionalText(prop.child("calendar-timezone").Text),
			Color:       optionalText(prop.child("calendar-color").Text),
		})
	}
	return items, nil
}

func optionalText(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func providerError(status int) error {
	return &Error{Code: "caldav_provider_error", Message: "CalDAV provider rejected the request (HTTP " + strconv.Itoa(status) + ").", Status: status}
}

func encryptCredential(privateKey, password string) (string, error) {
	key := sha256.Sum256([]byte(privateKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmt.Errorf("create calendar credential cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithTagSize(block, 12)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ciphertext := aead.Seal(nil, nonce, []byte(password), nil)
	tagStart := len(ciphertext) - aead.Overhead()
	envelope := make([]byte, 0, len(nonce)+len(ciphertext))
	envelope = append(envelope, nonce...)
	envelope = append(envelope, ciphertext[tagStart:]...)
	envelope = append(envelope, ciphertext[:tagStart]...)
	return base64.StdEncoding.EncodeToString(envelope), nil
}

func decryptCredential(privateKey, encoded string) (string, error) {
	key := sha256.Sum256([]byte(privateKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmt.Errorf("create calendar credential cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithTagSize(block, 12)
	if err != nil {
		return "", err
	}
	envelope, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(envelope) < aead.NonceSize()+aead.Overhead() {
		return "", errors.New("encrypted calendar credential is invalid")
	}
	nonce := envelope[:aead.NonceSize()]
	tagStart := aead.NonceSize()
	ciphertext := append([]byte(nil), envelope[tagStart+aead.Overhead():]...)
	ciphertext = append(ciphertext, envelope[tagStart:tagStart+aead.Overhead()]...)
	plain, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errors.New("encrypted calendar credential cannot be decrypted")
	}
	return string(plain), nil
}

func encryptCalendarToken(privateKey, value string) (string, error) {
	key := sha256.Sum256([]byte(privateKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmt.Errorf("create calendar token cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithTagSize(block, 12)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nil, nonce, []byte(value), nil)
	tagStart := len(sealed) - aead.Overhead()
	envelope := make([]byte, 0, len(nonce)+len(sealed))
	envelope = append(envelope, nonce...)
	envelope = append(envelope, sealed[tagStart:]...)
	envelope = append(envelope, sealed[:tagStart]...)
	return base64.StdEncoding.EncodeToString(envelope), nil
}

func decryptCalendarToken(privateKey, encoded string) (string, error) {
	key := sha256.Sum256([]byte(privateKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmt.Errorf("create calendar token cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithTagSize(block, 12)
	if err != nil {
		return "", err
	}
	envelope, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(envelope) < aead.NonceSize()+aead.Overhead() {
		return "", errors.New("encrypted calendar token is invalid")
	}
	nonce := envelope[:aead.NonceSize()]
	tagStart := aead.NonceSize()
	// CryptoHelper stores nonce, tag, then ciphertext. Earlier Go builds wrote
	// nonce, ciphertext, tag; accept both while always writing the legacy order.
	legacyCiphertext := append([]byte(nil), envelope[tagStart+aead.Overhead():]...)
	legacyCiphertext = append(legacyCiphertext, envelope[tagStart:tagStart+aead.Overhead()]...)
	if plain, openErr := aead.Open(nil, nonce, legacyCiphertext, nil); openErr == nil {
		return string(plain), nil
	}
	plain, err := aead.Open(nil, nonce, envelope[tagStart:tagStart+aead.Overhead()], envelope[tagStart+aead.Overhead():])
	if err != nil {
		return "", errors.New("encrypted calendar token cannot be decrypted")
	}
	return string(plain), nil
}
