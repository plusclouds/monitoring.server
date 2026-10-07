package inventory

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/auth"
	"github.com/plusclouds/monitoring.server/internal/errs"
)

// PushSource is what a push check (F08) shows about its ingest token and
// its last message. The token itself is shown once.
type PushSource struct {
	TokenPrefix    *string    `json:"token_prefix"` // nil for MQTT data checks: they have no HTTP token
	TokenCreatedAt *time.Time `json:"token_created_at"`
	LastPushAt     *time.Time `json:"last_push_at"`
}

// PushTokenScheme starts every ingest token: mpush_<8 characters>_<secret>.
const PushTokenScheme = "mpush_"

// HashPushToken is the stored form of an ingest token.
func HashPushToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// MatchPushToken compares a presented token with a stored hash in constant time.
func MatchPushToken(token string, stored []byte) bool {
	return strings.HasPrefix(token, PushTokenScheme) && subtle.ConstantTimeCompare(HashPushToken(token), stored) == 1
}

// setPushToken creates or replaces a check's ingest token and returns it.
func setPushToken(ctx context.Context, tx pgx.Tx, tenant, check uuid.UUID) (string, error) {
	k, err := auth.GenerateKey() // mon_<prefix>_<secret>
	if err != nil {
		return "", err
	}
	token := PushTokenScheme + strings.TrimPrefix(k.Full, "mon_")
	prefix := PushTokenScheme + k.Prefix
	_, err = tx.Exec(ctx, `
		INSERT INTO push_sources (check_id, tenant_id, token_hash, token_prefix)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (check_id) DO UPDATE SET token_hash = excluded.token_hash, token_prefix = excluded.token_prefix,
		       token_created_at = now()`, check, tenant, HashPushToken(token), prefix)
	return token, err
}

// RotatePushToken replaces a push check's ingest token: the old one stops
// working at once. It returns the check with the new token.
func RotatePushToken(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID) (Check, error) {
	c, err := LockCheck(ctx, tx, id)
	if err != nil {
		return Check{}, err
	}
	if c.Plugin != "push.http" {
		return Check{}, errs.Conflictf("not-push", "%s has no ingest token; only push.http checks do", c.Plugin)
	}
	token, err := setPushToken(ctx, tx, c.TenantID, id)
	if err != nil {
		return Check{}, err
	}
	if c, err = GetCheck(ctx, tx, id); err != nil {
		return Check{}, err
	}
	c.PushToken = token
	return c, audit.Write(ctx, tx, actor.Event(c.TenantID, "check.push_token.rotate", "check", id.String(), nil,
		map[string]any{"token_prefix": *c.Push.TokenPrefix}))
}
