// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package maildelivery

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestAccountDeletionCancelsAndAnonymizesMatchingMail(t *testing.T) {
	pool := mailDeliveryTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE mail_deliveries (
		id text PRIMARY KEY, mail_name text NOT NULL, status text NOT NULL, recipient_email text,
		recipient_hash text NOT NULL, actor_user_id text, payload jsonb, canceled_at timestamptz,
		settled_at timestamptz, locked_by text, locked_until timestamptz, retention_state text,
		anonymized_at timestamptz, last_error_code text, updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	const privateKey = "test-private-key"
	repository := NewRepository(pool, config.Config{}, func(context.Context) (string, error) { return privateKey, nil }, nil)
	key := sha256.Sum256([]byte(privateKey))
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte("user@example.test"))
	recipientHash := hex.EncodeToString(mac.Sum(nil))
	for _, row := range []struct {
		id, name, status, recipient, hash, actor string
	}{
		{"recipient-queued", "Welcome", "queued", "user@example.test", recipientHash, "other"},
		{"recipient-sending", "Welcome", "sending", "user@example.test", recipientHash, "other"},
		{"recipient-sent", "Welcome", "sent", "user@example.test", recipientHash, "other"},
		{"actor-invitation", "MemberInvitation", "retry_wait", "invitee@example.test", "other-hash", "deleted-user"},
		{"other-invitation", "MemberInvitation", "queued", "other@example.test", "other-hash", "other-user"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO mail_deliveries(id,mail_name,status,recipient_email,recipient_hash,actor_user_id,payload,retention_state)
			VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,'full')`, row.id, row.name, row.status, row.recipient, row.hash, row.actor, json.RawMessage(`{"secret":"personal data"}`)); err != nil {
			t.Fatal(err)
		}
		if row.id == "recipient-sending" {
			if _, err := pool.Exec(ctx, `UPDATE mail_deliveries SET locked_by='mail-worker',locked_until=now()+interval '1 minute' WHERE id=$1`, row.id); err != nil {
				t.Fatal(err)
			}
		}
	}
	recipientCount, err := repository.CancelByRecipient(ctx, " User@Example.Test ")
	if err != nil || recipientCount != 2 {
		t.Fatalf("CancelByRecipient = %d, %v; want 2 canceled deliveries", recipientCount, err)
	}
	actorCount, err := repository.CancelMemberInvitationByActor(ctx, "deleted-user")
	if err != nil || actorCount != 1 {
		t.Fatalf("CancelMemberInvitationByActor = %d, %v; want 1 canceled delivery", actorCount, err)
	}
	rows, err := pool.Query(ctx, `SELECT id,status,recipient_email,payload,retention_state,last_error_code,locked_by,locked_until,
		canceled_at IS NOT NULL,settled_at IS NOT NULL,anonymized_at IS NOT NULL FROM mail_deliveries ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][]any{}
	for rows.Next() {
		var id, status, retention string
		var reason pgtype.Text
		var recipient, lockedBy, lockedUntil *string
		var payload []byte
		var canceled, settled, anonymized bool
		if err := rows.Scan(&id, &status, &recipient, &payload, &retention, &reason, &lockedBy, &lockedUntil, &canceled, &settled, &anonymized); err != nil {
			t.Fatal(err)
		}
		var reasonValue any
		if reason.Valid {
			reasonValue = reason.String
		}
		got[id] = []any{status, recipient, payload, retention, reasonValue, lockedBy, lockedUntil, canceled, settled, anonymized}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for id, reason := range map[string]string{"recipient-queued": "recipient_deleted", "recipient-sending": "recipient_deleted", "actor-invitation": "actor_deleted"} {
		state := got[id]
		recipient := state[1].(*string)
		payload := state[2].([]byte)
		lockedBy := state[5].(*string)
		lockedUntil := state[6].(*string)
		if len(state) != 10 || state[0] != "canceled" || recipient != nil || len(payload) != 0 || state[3] != "anonymized" || state[4] != reason || lockedBy != nil || lockedUntil != nil || state[7] != true || state[8] != true || state[9] != true {
			t.Fatalf("canceled row %s = %#v", id, state)
		}
	}
	if got["recipient-sent"][0] != "sent" || got["other-invitation"][0] != "queued" {
		t.Fatalf("unmatched terminal or unrelated mail changed: %#v", got)
	}
}
