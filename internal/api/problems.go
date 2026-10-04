package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/tenancy"
)

// problemBase prefixes every problem type URI (RFC 9457, ADR-0007).
const problemBase = "https://monitor.plusclouds.com/problems/"

// Problem is an error with a stable type, returned as application/problem+json.
type Problem struct {
	Status int
	Type   string // suffix of problemBase
	Title  string
	Detail string
}

func (p *Problem) Error() string { return p.Title + ": " + p.Detail }

func problem(status int, typ, title, detail string) *Problem {
	return &Problem{Status: status, Type: typ, Title: title, Detail: detail}
}

var (
	errUnauthenticated = problem(http.StatusUnauthorized, "unauthenticated", "Unauthenticated",
		"Send a valid API key as `Authorization: Bearer <key>`.")
	errForbidden = problem(http.StatusForbidden, "forbidden", "Forbidden",
		"Your role does not allow this operation.")
	errPlatformOnly = problem(http.StatusForbidden, "platform-only", "Platform key required",
		"Only the platform key may call this operation.")
	errTenantRequired = problem(http.StatusBadRequest, "tenant-required", "Tenant required",
		"The platform key must name a tenant with X-Tenant-External-ID or X-Tenant-ID.")
	errTenantSuspended = problem(http.StatusForbidden, "tenant-suspended", "Tenant suspended",
		"The tenant is suspended: reads work, writes are refused.")
	errNotMember = problem(http.StatusForbidden, "not-a-member", "Not a member",
		"The acting user is not a member of this tenant.")
	errNotFound    = problem(http.StatusNotFound, "not-found", "Not found", "")
	errRateLimited = problem(http.StatusTooManyRequests, "rate-limited", "Too many requests",
		"This API key exceeded its request rate.")
)

// toProblem maps domain errors to problems; anything unknown is a 500 whose
// detail is logged, never returned.
func toProblem(err error) *Problem {
	var p *Problem
	var v *tenancy.ValidationError
	var inv *errs.Invalid
	var conflict *errs.Conflict
	switch {
	case errors.As(err, &p):
		return p
	case errors.Is(err, tenancy.ErrNotFound), errors.Is(err, errs.ErrNotFound):
		return errNotFound
	case errors.As(err, &inv):
		return problem(http.StatusUnprocessableEntity, "invalid-value", "Invalid value", inv.Error())
	case errors.As(err, &conflict):
		return problem(http.StatusConflict, conflict.Type, "Conflict", conflict.Message)
	case errors.Is(err, tenancy.ErrDeleted):
		return problem(http.StatusConflict, "tenant-deleted", "Tenant deleted", "The tenant is deleted and cannot be changed.")
	case errors.As(err, &v):
		return problem(http.StatusUnprocessableEntity, "invalid-value", "Invalid value", v.Error())
	default:
		return nil
	}
}

func writeProblem(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	p := toProblem(err)
	if p == nil {
		log.ErrorContext(r.Context(), "request failed", "error", err, "request_id", requestID(r.Context()))
		p = problem(http.StatusInternalServerError, "internal", "Internal error",
			"The request failed. Quote the request ID when reporting it.")
	}
	body := map[string]any{
		"type": problemBase + p.Type, "title": p.Title, "status": p.Status,
		"instance": r.URL.Path, "request_id": requestID(r.Context()),
	}
	if p.Detail != "" {
		body["detail"] = p.Detail
	}
	w.Header().Set("Content-Type", "application/problem+json")
	if p.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="monitor"`)
	}
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(body)
}
