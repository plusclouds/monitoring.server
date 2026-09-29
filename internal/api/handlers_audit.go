package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/api/gen"
	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/store"
)

// ListAuditEvents returns the tenant's events, newest first. Reading the
// audit log is itself audited (F01).
func (s *Server) ListAuditEvents(ctx context.Context, req gen.ListAuditEventsRequestObject) (gen.ListAuditEventsResponseObject, error) {
	p, t, err := tenantScope(ctx, roleAdmin, false)
	if err != nil {
		return nil, err
	}
	q := req.Params
	limit := 100
	if q.Limit != nil {
		limit = *q.Limit
	}
	var beforeSeq *int64
	if q.Cursor != nil {
		b, err := base64.RawURLEncoding.DecodeString(*q.Cursor)
		n, perr := strconv.ParseInt(string(b), 10, 64)
		if err != nil || perr != nil {
			return nil, problem(http.StatusBadRequest, "invalid-cursor", "Invalid cursor", "Use next_cursor from the previous page.")
		}
		beforeSeq = &n
	}

	resp := gen.ListAuditEvents200JSONResponse{Items: []gen.AuditEvent{}}
	err = store.InTenant(ctx, s.db, t.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, seq, at, actor_kind, actor_user_id, actor_external_id, api_key_id, actor_detail,
			       action, object_type, object_id, before, after, request_id, source_ip::text
			  FROM audit_events
			 WHERE tenant_id = $1
			   AND ($2::text IS NULL OR object_type = $2)
			   AND ($3::text IS NULL OR object_id = $3)
			   AND ($4::text IS NULL OR action = $4)
			   AND ($5::timestamptz IS NULL OR at >= $5)
			   AND ($6::timestamptz IS NULL OR at < $6)
			   AND ($7::bigint IS NULL OR seq < $7)
			 ORDER BY seq DESC
			 LIMIT $8`, t.ID, q.ObjectType, q.ObjectId, q.Action, q.From, q.To, beforeSeq, limit+1)
		if err != nil {
			return err
		}
		events, err := pgx.CollectRows(rows, scanAuditEvent)
		if err != nil {
			return err
		}
		for i, e := range events {
			if i == limit {
				c := base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(events[i-1].Seq, 10)))
				resp.NextCursor = &c
				break
			}
			resp.Items = append(resp.Items, e)
		}
		return audit.Write(ctx, tx, p.Actor.Event(t.ID, "audit.query", "audit", "", nil, map[string]any{
			"object_type": q.ObjectType, "object_id": q.ObjectId, "action": q.Action,
			"from": q.From, "to": q.To, "returned": len(resp.Items),
		}))
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func scanAuditEvent(r pgx.CollectableRow) (gen.AuditEvent, error) {
	var e gen.AuditEvent
	var kind string
	var userID, keyID *uuid.UUID
	var before, after []byte
	err := r.Scan(&e.Id, &e.Seq, &e.At, &kind, &userID, &e.Actor.ExternalId, &keyID, &e.Actor.Detail,
		&e.Action, &e.ObjectType, &e.ObjectId, &before, &after, &e.RequestId, &e.SourceIp)
	if err != nil {
		return e, err
	}
	e.At = e.At.UTC().Truncate(time.Microsecond)
	e.Actor.Kind = gen.AuditEventActorKind(kind)
	e.Actor.UserId, e.Actor.ApiKeyId = userID, keyID
	if before != nil {
		e.Before = json.RawMessage(before)
	}
	if after != nil {
		e.After = json.RawMessage(after)
	}
	return e, nil
}
