// Package audit writes audit events (F01). Events are written in the same
// transaction as the change they describe; the database assigns the
// sequence number and the hash chain.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Actor kinds, matching the audit_events.actor_kind constraint.
const (
	ActorUser     = "user"
	ActorAPIKey   = "api_key"
	ActorPlatform = "platform"
	ActorAdminCLI = "admin-cli"
	ActorFile     = "file"
	ActorSystem   = "system"
)

// Changed replaces secret values in Before and After (F01, F02).
const Changed = "[changed]"

// Event is one audit record.
type Event struct {
	TenantID        uuid.UUID
	ActorKind       string
	ActorUserID     *uuid.UUID
	ActorExternalID string
	APIKeyID        *uuid.UUID
	ActorDetail     string // host name for admin-cli, file hash for file
	Action          string // "tenant.create", "api_key.create"
	ObjectType      string
	ObjectID        string
	Before, After   any // marshalled to JSON; nil stays NULL
	RequestID       string
	SourceIP        netip.Addr
}

// Write inserts an event inside tx.
func Write(ctx context.Context, tx pgx.Tx, e Event) error {
	before, err := jsonOrNil(e.Before)
	if err != nil {
		return err
	}
	after, err := jsonOrNil(e.After)
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	var ip *string
	if e.SourceIP.IsValid() {
		s := e.SourceIP.String()
		ip = &s
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (id, tenant_id, actor_kind, actor_user_id, actor_external_id,
		                          api_key_id, actor_detail, action, object_type, object_id,
		                          before, after, request_id, source_ip)
		VALUES ($1, $2, $3, $4, nullif($5, ''), $6, nullif($7, ''), $8, $9, nullif($10, ''),
		        $11, $12, nullif($13, ''), $14::inet)`,
		id, e.TenantID, e.ActorKind, e.ActorUserID, e.ActorExternalID,
		e.APIKeyID, e.ActorDetail, e.Action, e.ObjectType, e.ObjectID,
		before, after, e.RequestID, ip)
	if err != nil {
		return fmt.Errorf("write audit event %s: %w", e.Action, err)
	}
	return nil
}

func jsonOrNil(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// Problem is a broken link in a tenant's audit chain.
type Problem struct {
	TenantID uuid.UUID
	Seq      int64
	Detail   string
}

// Verify checks the hash chain of every tenant, or of one when tenantID is
// set, and returns the first problem per tenant.
func Verify(ctx context.Context, db interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, tenantID *uuid.UUID) ([]Problem, error) {
	rows, err := db.Query(ctx, `
		SELECT t.id, v.seq, v.problem
		  FROM tenants t CROSS JOIN LATERAL audit_verify(t.id) v
		 WHERE $1::uuid IS NULL OR t.id = $1
		 ORDER BY t.id`, tenantID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Problem, error) {
		var p Problem
		err := r.Scan(&p.TenantID, &p.Seq, &p.Detail)
		return p, err
	})
}
