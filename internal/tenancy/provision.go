package tenancy

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/store"
)

// ErrDeleted means the tenant was soft-deleted and cannot be changed.
var ErrDeleted = errors.New("tenant is deleted")

// Roles of a tenant member (F01).
const (
	RoleReadOnly = "read-only"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

// Service provisions tenants, users and memberships for the platform key.
type Service struct {
	DB             *pgxpool.Pool // the app role; every write is scoped by RLS
	IdentitySource string
	Defaults       config.TenantLimits
	JIT            config.JIT
}

func limitsFrom(c config.TenantLimits) Limits {
	nets, _ := ParseNetworks(c.AllowedTargetNetworks) // validated with the config
	classes := c.MetricClasses
	if len(classes) == 0 {
		classes = []string{"standard"}
	}
	return Limits{
		MaxDevices: c.MaxDevices, MaxChecks: c.MaxChecks,
		MinCheckIntervalSeconds: c.MinCheckIntervalSeconds, APIRatePerMinute: c.APIRatePerMinute,
		AllowedTargetNetworks: nets, MetricClasses: classes,
	}
}

// UpsertTenant creates or updates the tenant with this external ID. It
// reports whether the tenant was created; an update that changes nothing
// writes no audit event (F01).
func (s *Service) UpsertTenant(ctx context.Context, actor audit.Actor, source, externalID, name string,
	externalType *string, patch LimitsPatch) (Tenant, bool, error) {
	for range 3 {
		cur, err := ByExternal(ctx, s.DB, source, externalID)
		if errors.Is(err, ErrNotFound) {
			limits, err := patch.Apply(limitsFrom(s.Defaults))
			if err != nil {
				return Tenant{}, false, err
			}
			t, created, err := s.create(ctx, actor, source, externalID, name, externalType, limits, "api")
			if err != nil || created {
				return t, created, err
			}
			continue // created concurrently by another request; update it instead
		}
		if err != nil {
			return Tenant{}, false, err
		}
		t, err := s.change(ctx, actor, cur.ID, func(t *Tenant) error {
			if t.Status == StatusDeleted {
				return ErrDeleted
			}
			t.Name = name
			if externalType != nil {
				t.ExternalType = externalType
			}
			t.Limits, err = patch.Apply(t.Limits)
			return err
		})
		return t, false, err
	}
	return Tenant{}, false, errors.New("tenant upsert did not settle after concurrent changes")
}

// EnsureTenant returns the tenant with this external ID, creating it just in
// time with the JIT limits when it is unknown (ADR-0012).
func (s *Service) EnsureTenant(ctx context.Context, actor audit.Actor, source, externalID string) (Tenant, error) {
	for range 3 {
		t, err := ByExternal(ctx, s.DB, source, externalID)
		if !errors.Is(err, ErrNotFound) {
			return t, err
		}
		if !s.JIT.Enabled {
			return Tenant{}, ErrNotFound
		}
		t, created, err := s.create(ctx, actor, source, externalID, externalID, nil, limitsFrom(s.JIT.TenantLimits), "jit")
		if err != nil || created {
			return t, err
		}
	}
	return Tenant{}, errors.New("tenant creation did not settle after concurrent requests")
}

// PatchTenant changes the fields that are set.
func (s *Service) PatchTenant(ctx context.Context, actor audit.Actor, source, externalID string,
	name, status *string, patch LimitsPatch) (Tenant, error) {
	cur, err := ByExternal(ctx, s.DB, source, externalID)
	if err != nil {
		return Tenant{}, err
	}
	return s.change(ctx, actor, cur.ID, func(t *Tenant) error {
		if t.Status == StatusDeleted {
			return ErrNotFound
		}
		if name != nil {
			t.Name = *name
		}
		if status != nil {
			t.Status = *status
		}
		var err error
		t.Limits, err = patch.Apply(t.Limits)
		return err
	})
}

// DeleteTenant soft-deletes a tenant. Deleting a deleted tenant is a no-op.
func (s *Service) DeleteTenant(ctx context.Context, actor audit.Actor, source, externalID string) error {
	cur, err := ByExternal(ctx, s.DB, source, externalID)
	if err != nil {
		return err
	}
	_, err = s.change(ctx, actor, cur.ID, func(t *Tenant) error {
		t.Status = StatusDeleted
		return nil
	})
	return err
}

func (s *Service) create(ctx context.Context, actor audit.Actor, source, externalID, name string,
	externalType *string, limits Limits, provisioned string) (Tenant, bool, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Tenant{}, false, err
	}
	t := Tenant{
		ID: id, Name: name, Status: StatusActive, Limits: limits, Provisioned: provisioned,
		ExternalSource: &source, ExternalType: externalType, ExternalID: &externalID,
	}
	var created bool
	err = store.InTenant(ctx, s.DB, id, func(tx pgx.Tx) error {
		if created, err = insert(ctx, tx, t); err != nil || !created {
			return err
		}
		if t, err = lockForUpdate(ctx, tx, id); err != nil {
			return err
		}
		after := t.Snapshot()
		after["external_source"], after["external_id"], after["provisioned"] = source, externalID, provisioned
		return audit.Write(ctx, tx, actor.Event(id, "tenant.create", "tenant", id.String(), nil, after))
	})
	return t, created, err
}

// change applies fn to the locked tenant row and writes it, with an audit
// event, only when something changed.
func (s *Service) change(ctx context.Context, actor audit.Actor, id uuid.UUID, fn func(*Tenant) error) (Tenant, error) {
	var out Tenant
	err := store.InTenant(ctx, s.DB, id, func(tx pgx.Tx) error {
		cur, err := lockForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		next := cur
		next.Limits.AllowedTargetNetworks = append([]netip.Prefix(nil), cur.Limits.AllowedTargetNetworks...)
		next.Limits.MetricClasses = append([]string(nil), cur.Limits.MetricClasses...)
		if err := fn(&next); err != nil {
			return err
		}
		before, after := cur.Snapshot(), next.Snapshot()
		if reflect.DeepEqual(before, after) {
			out = cur
			return nil
		}
		if out, err = update(ctx, tx, next); err != nil {
			return mapError(err)
		}
		action := "tenant.update"
		switch {
		case next.Status == StatusDeleted && cur.Status != StatusDeleted:
			action = "tenant.delete"
		case next.Status == StatusSuspended && cur.Status != StatusSuspended:
			action = "tenant.suspend"
		case next.Status == StatusActive && cur.Status == StatusSuspended:
			action = "tenant.resume"
		}
		return audit.Write(ctx, tx, actor.Event(id, action, "tenant", id.String(), before, after))
	})
	return out, err
}

// User is the minimal mirror of a PlusClouds user: no personal data.
type User struct {
	ID             uuid.UUID
	DisplayName    *string
	Status         string
	ExternalSource *string
	ExternalType   *string
	ExternalID     *string
}

// Member is a user's role in a tenant.
type Member struct {
	User      User
	Role      string
	CreatedAt time.Time
}

const userCols = `id, display_name, status, external_source, external_type, external_id`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.DisplayName, &u.Status, &u.ExternalSource, &u.ExternalType, &u.ExternalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// UserByExternal finds a user across tenants.
func UserByExternal(ctx context.Context, q Querier, source, externalID string) (User, error) {
	return scanUser(q.QueryRow(ctx, `SELECT `+userCols+` FROM user_by_external($1, $2)`, source, externalID))
}

// EnsureUser returns the user with this external ID, creating the mirror row
// when needed. Creation is audited in the platform tenant, since users are
// global.
func (s *Service) EnsureUser(ctx context.Context, actor audit.Actor, source, externalID string,
	displayName *string, provisioned string) (User, bool, error) {
	newID, err := uuid.NewV7()
	if err != nil {
		return User{}, false, err
	}
	var id uuid.UUID
	var created bool
	if err := s.DB.QueryRow(ctx, `SELECT id, created FROM ensure_user($1, $2, $3, $4, $5)`,
		newID, source, externalID, displayName, provisioned).Scan(&id, &created); err != nil {
		return User{}, false, err
	}
	u, err := scanUser(s.DB.QueryRow(ctx, `SELECT `+userCols+` FROM user_by_id($1)`, id))
	if err != nil || !created {
		return u, created, err
	}
	err = s.platformEvent(ctx, actor, "user.create", "user", id.String(), nil,
		map[string]any{"external_source": source, "external_id": externalID, "display_name": displayName, "provisioned": provisioned})
	return u, true, err
}

// PatchUser changes a user's status and, when setDisplay, its display name.
func (s *Service) PatchUser(ctx context.Context, actor audit.Actor, source, externalID string,
	status *string, setDisplay bool, displayName *string) (User, error) {
	u, err := UserByExternal(ctx, s.DB, source, externalID)
	if err != nil {
		return u, err
	}
	var before, after map[string]any
	if err := s.DB.QueryRow(ctx, `SELECT before, after FROM update_user($1, $2, $3, $4)`,
		u.ID, status, setDisplay, displayName).Scan(&before, &after); err != nil {
		return u, err
	}
	if !maps.EqualFunc(before, after, func(a, b any) bool { return reflect.DeepEqual(a, b) }) {
		if err := s.platformEvent(ctx, actor, "user.update", "user", u.ID.String(), before, after); err != nil {
			return u, err
		}
	}
	return scanUser(s.DB.QueryRow(ctx, `SELECT `+userCols+` FROM user_by_id($1)`, u.ID))
}

// UpsertMember adds a user to a tenant or changes its role, creating the
// user mirror row when needed.
func (s *Service) UpsertMember(ctx context.Context, actor audit.Actor, tenantID uuid.UUID, source, userExternalID,
	role string, displayName *string) (Member, bool, error) {
	if _, err := s.activeTenant(ctx, tenantID); err != nil {
		return Member{}, false, err
	}
	u, created, err := s.EnsureUser(ctx, actor, source, userExternalID, displayName, "api")
	if err != nil {
		return Member{}, false, err
	}
	if !created && displayName != nil && (u.DisplayName == nil || *u.DisplayName != *displayName) {
		if u, err = s.PatchUser(ctx, actor, source, userExternalID, nil, true, displayName); err != nil {
			return Member{}, false, err
		}
	}
	return s.setRole(ctx, actor, tenantID, u, role)
}

func (s *Service) setRole(ctx context.Context, actor audit.Actor, tenantID uuid.UUID, u User, role string) (Member, bool, error) {
	m := Member{User: u, Role: role}
	var added bool
	err := store.InTenant(ctx, s.DB, tenantID, func(tx pgx.Tx) error {
		var cur string
		err := tx.QueryRow(ctx, `SELECT role, created_at FROM tenant_members WHERE tenant_id = $1 AND user_id = $2 FOR UPDATE`,
			tenantID, u.ID).Scan(&cur, &m.CreatedAt)
		ref := map[string]any{"user_id": u.ID, "external_id": u.ExternalID}
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			added = true
			if err := tx.QueryRow(ctx, `INSERT INTO tenant_members (tenant_id, user_id, role) VALUES ($1, $2, $3) RETURNING created_at`,
				tenantID, u.ID, role).Scan(&m.CreatedAt); err != nil {
				return err
			}
			return audit.Write(ctx, tx, actor.Event(tenantID, "member.create", "member", u.ID.String(), nil, merge(ref, "role", role)))
		case err != nil:
			return err
		case cur == role:
			return nil
		default:
			if _, err := tx.Exec(ctx, `UPDATE tenant_members SET role = $3, updated_at = now() WHERE tenant_id = $1 AND user_id = $2`,
				tenantID, u.ID, role); err != nil {
				return err
			}
			return audit.Write(ctx, tx, actor.Event(tenantID, "member.update", "member", u.ID.String(),
				merge(ref, "role", cur), merge(ref, "role", role)))
		}
	})
	return m, added, err
}

// RemoveMember removes a user from a tenant. Removing a non-member is a no-op.
func (s *Service) RemoveMember(ctx context.Context, actor audit.Actor, tenantID uuid.UUID, source, userExternalID string) error {
	if _, err := s.activeTenant(ctx, tenantID); err != nil {
		return err
	}
	u, err := UserByExternal(ctx, s.DB, source, userExternalID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return store.InTenant(ctx, s.DB, tenantID, func(tx pgx.Tx) error {
		var role string
		err := tx.QueryRow(ctx, `DELETE FROM tenant_members WHERE tenant_id = $1 AND user_id = $2 RETURNING role`,
			tenantID, u.ID).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return audit.Write(ctx, tx, actor.Event(tenantID, "member.delete", "member", u.ID.String(),
			map[string]any{"user_id": u.ID, "external_id": u.ExternalID, "role": role}, nil))
	})
}

// ListMembers returns a tenant's members.
func (s *Service) ListMembers(ctx context.Context, tenantID uuid.UUID) ([]Member, error) {
	if _, err := s.activeTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	var out []Member
	err := store.InTenant(ctx, s.DB, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT u.id, u.display_name, u.status, u.external_source, u.external_type, u.external_id, m.role, m.created_at
			  FROM tenant_members m JOIN users u ON u.id = m.user_id
			 WHERE m.tenant_id = $1 ORDER BY m.created_at`, tenantID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) {
			var m Member
			err := r.Scan(&m.User.ID, &m.User.DisplayName, &m.User.Status, &m.User.ExternalSource,
				&m.User.ExternalType, &m.User.ExternalID, &m.Role, &m.CreatedAt)
			return m, err
		})
		return err
	})
	return out, err
}

// MemberRole is the user's role in the tenant, or ErrNotFound.
func MemberRole(ctx context.Context, db *pgxpool.Pool, tenantID, userID uuid.UUID) (string, error) {
	var role string
	err := store.InTenant(ctx, db, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT role FROM tenant_members WHERE tenant_id = $1 AND user_id = $2`,
			tenantID, userID).Scan(&role)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

// EnsureActor resolves the acting user of a platform-key request. A user
// seen for the first time is created with a read-only membership in the
// tenant (ADR-0012); an existing user keeps whatever membership it has.
func (s *Service) EnsureActor(ctx context.Context, actor audit.Actor, tenantID uuid.UUID, source, externalID string) (User, string, error) {
	u, err := UserByExternal(ctx, s.DB, source, externalID)
	if errors.Is(err, ErrNotFound) {
		if !s.JIT.Enabled {
			return User{}, "", ErrNotFound
		}
		if u, err = s.createActor(ctx, actor, tenantID, source, externalID); err != nil {
			return User{}, "", err
		}
	} else if err != nil {
		return User{}, "", err
	}
	role, err := MemberRole(ctx, s.DB, tenantID, u.ID)
	return u, role, err
}

// createActor creates a user and its read-only membership in one
// transaction. Concurrent requests for the same new user wait on the unique
// index inside ensure_user until this transaction commits, so none of them
// can see the user without its membership.
func (s *Service) createActor(ctx context.Context, actor audit.Actor, tenantID uuid.UUID, source, externalID string) (User, error) {
	newID, err := uuid.NewV7()
	if err != nil {
		return User{}, err
	}
	var id uuid.UUID
	var created bool
	err = store.InTenant(ctx, s.DB, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id, created FROM ensure_user($1, $2, $3, NULL, 'jit')`,
			newID, source, externalID).Scan(&id, &created); err != nil {
			return err
		}
		if !created {
			return nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_members (tenant_id, user_id, role) VALUES ($1, $2, $3)`,
			tenantID, id, RoleReadOnly); err != nil {
			return err
		}
		return audit.Write(ctx, tx, actor.Event(tenantID, "member.create", "member", id.String(), nil,
			map[string]any{"user_id": id, "external_id": externalID, "role": RoleReadOnly}))
	})
	if err != nil {
		return User{}, err
	}
	if created {
		// Users are global, so their creation is recorded in the platform tenant.
		if err := s.platformEvent(ctx, actor, "user.create", "user", id.String(), nil,
			map[string]any{"external_source": source, "external_id": externalID, "provisioned": "jit"}); err != nil {
			return User{}, err
		}
	}
	return scanUser(s.DB.QueryRow(ctx, `SELECT `+userCols+` FROM user_by_id($1)`, id))
}

func (s *Service) activeTenant(ctx context.Context, id uuid.UUID) (Tenant, error) {
	t, err := ByID(ctx, s.DB, id)
	if err == nil && t.Status == StatusDeleted {
		return t, ErrNotFound
	}
	return t, err
}

func (s *Service) platformEvent(ctx context.Context, actor audit.Actor, action, objectType, objectID string, before, after any) error {
	platform, err := PlatformID(ctx, s.DB)
	if err != nil {
		return err
	}
	return store.InTenant(ctx, s.DB, platform, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, actor.Event(platform, action, objectType, objectID, before, after))
	})
}

func merge(m map[string]any, k string, v any) map[string]any {
	out := maps.Clone(m)
	out[k] = v
	return out
}
