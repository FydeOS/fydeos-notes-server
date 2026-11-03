// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/clientip"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/mailquota"
	"github.com/fydeos/fydeos-notes-server/internal/serverconfig"
	"github.com/jackc/pgx/v5"
)

type mailRequest struct {
	name         string
	to           string
	userID       *string
	props        map[string]any
	dedupeKey    string
	expires      time.Time
	sourcePrefix string
	privateKey   string
}

func recipientHash(privateKey, email string) string {
	key := sha256.Sum256([]byte(privateKey))
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(strings.ToLower(email)))
	return hex.EncodeToString(mac.Sum(nil))
}

func emailDomain(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}

// queueMail enqueues an auth-class mail with dedupe and quota checks.
func queueMail(ctx context.Context, tx pgx.Tx, m mailRequest) error {
	if strings.TrimSpace(m.privateKey) == "" {
		return errors.New("auth: app private key is not configured")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, m.dedupeKey); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mail_deliveries WHERE dedupe_key=$1)`, m.dedupeKey).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	now, err := dbNow(ctx, tx)
	if err != nil {
		return err
	}
	lower := strings.ToLower(m.to)
	domain := emailDomain(lower)
	scopes := []mailquota.Scope{
		{Key: "mail:recipient:" + mailquota.RecipientScopeHash(lower) + ":class:auth", WindowSeconds: 3600, Limit: 20, LegacyBucketSeconds: 300},
		{Key: "mail:recipient_domain:" + domain + ":class:auth", WindowSeconds: 3600, Limit: 250, LegacyBucketSeconds: 300},
		{Key: "mail:provider_global:default", WindowSeconds: 60, Limit: 500, LegacyBucketSeconds: 10},
	}
	if m.sourcePrefix != "" {
		scopes = append(scopes, mailquota.Scope{Key: "mail:source_prefix:" + m.sourcePrefix + ":class:auth", WindowSeconds: 3600, Limit: 50, LegacyBucketSeconds: 300})
	}
	denial, err := mailquota.CommitDetailed(ctx, tx, scopes, now)
	if err != nil {
		return err
	}
	if denial != nil {
		return ErrMagicLinkQuotaDenied
	}
	payload, _ := json.Marshal(map[string]any{"name": m.name, "to": m.to, "props": m.props})
	_, err = tx.Exec(ctx, `INSERT INTO mail_deliveries(mail_name,mail_class,priority,status,dedupe_key,recipient_email,recipient_hash,recipient_domain,recipient_user_id,quota_decision,payload,send_after,expires_at,max_attempts)
		VALUES($1,'auth','critical','queued',$2,$3,$4,$5,$6,'{"allowed":true,"mailClass":"auth","requested":1}'::jsonb,$7::jsonb,$8,$9,3)`,
		m.name, m.dedupeKey, m.to, recipientHash(m.privateKey, lower), domain, m.userID, string(payload), now, m.expires)
	if err != nil {
		return fmt.Errorf("queue %s mail: %w", m.name, err)
	}
	return nil
}

func mailConfigured(ctx context.Context, db RowQuerier, cfg config.Config) (bool, error) {
	if cfg.NodeEnv == "test" || cfg.NodeEnv == "development" {
		return true, nil
	}
	raw, found, err := config.ReadRuntimeValue(ctx, db, cfg.RuntimeConfigPath, "mailer.SMTP.host")
	if err != nil {
		return false, err
	}
	if found {
		var host string
		if err := json.Unmarshal(raw, &host); err != nil {
			return false, errors.New("invalid mailer.SMTP.host")
		}
		if strings.TrimSpace(host) != "" {
			return true, nil
		}
		return false, nil
	}
	return os.Getenv("MAILER_HOST") != "", nil
}

func serverName(ctx context.Context, db RowQuerier, cfg config.Config) string {
	raw, found, err := config.ReadRuntimeValue(ctx, db, cfg.RuntimeConfigPath, "server.name")
	if err == nil && found {
		var name string
		if json.Unmarshal(raw, &name) == nil && strings.TrimSpace(name) != "" {
			return serverconfig.NormalizeDisplayName(name)
		}
	}
	if cfg.Deployment == "selfhosted" {
		return "FydeOS Notes"
	}
	return "FydeOS Notes Cloud"
}

func requestClientIP(r *http.Request, cfg config.Config) string {
	return clientip.RequestClientIP(r, cfg.TrustedProxyCIDRs, cfg.TrustedCloudflareCIDRs, cfg.TrustedXRealIPCIDRs)
}

// sourcePrefix masks the client IP to /24 (IPv4) or /48 (IPv6).
func sourcePrefix(ip string) string {
	addr, err := netip.ParseAddr(strings.Trim(ip, "[]"))
	if err != nil {
		if host, _, splitErr := net.SplitHostPort(ip); splitErr == nil {
			addr, err = netip.ParseAddr(host)
		}
		if err != nil {
			return ""
		}
	}
	addr = addr.Unmap()
	bits := 48
	if addr.Is4() {
		bits = 24
	}
	prefix, err := addr.Prefix(bits)
	if err != nil {
		return ""
	}
	return prefix.String()
}
