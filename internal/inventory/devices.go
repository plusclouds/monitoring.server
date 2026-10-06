package inventory

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/extref"
	"github.com/plusclouds/monitoring.server/internal/incident"
)

// DeviceTypes accepted by the API (F02, ADR-0014).
var DeviceTypes = []string{"network", "server", "bmc", "camera", "web", "hypervisor_pool", "hypervisor_host",
	"vm", "iot", "ups", "pdu", "sensor", "llm_endpoint", "llm_application", "other"}

// Device is anything with its own address or lifecycle.
type Device struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	Name           string
	Address        string
	Type           string
	Tags           map[string]string
	Notes          *string
	ParentID       *uuid.UUID // containment (ADR-0014)
	SiteID         *uuid.UUID
	PhysicalPeerID *uuid.UUID
	Inventory      map[string]any // written by plugins
	ManagedBy      string
	External       *extref.Ref
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Status         *Status // set by ListDevices and FillStatus
}

// DeviceInput is the client-writable part of a device. Updates replace it.
type DeviceInput struct {
	Name           string
	Address        string
	Type           string
	Tags           map[string]string
	Notes          *string
	ParentID       *uuid.UUID
	SiteID         *uuid.UUID
	PhysicalPeerID *uuid.UUID
	External       *extref.Ref
}

func (in DeviceInput) validate() error {
	if err := name("name", in.Name); err != nil {
		return err
	}
	if len(in.Address) > 2000 {
		return errs.Invalidf("address", "must be at most 2000 characters")
	}
	if !slices.Contains(DeviceTypes, in.Type) {
		return errs.Invalidf("type", "%q is not one of %v", in.Type, DeviceTypes)
	}
	if len(in.Tags) > 50 {
		return errs.Invalidf("tags", "at most 50 tags")
	}
	for k, v := range in.Tags {
		if k == "" || len(k) > 100 || len(v) > 200 {
			return errs.Invalidf("tags", "keys must be 1 to 100 characters, values at most 200")
		}
	}
	if in.Notes != nil && len(*in.Notes) > 10000 {
		return errs.Invalidf("notes", "must be at most 10000 characters")
	}
	return in.External.Validate()
}

const deviceCols = `id, tenant_id, name, address, type, tags, notes, parent_id, site_id, physical_peer_id,
	inventory, managed_by, external_source, external_type, external_id, created_at, updated_at`

var deviceConstraints = map[string]string{
	"devices_tenant_id_name_key": "a device with this name already exists",
	"devices_external":           "a device with this external ID already exists",
}

func scanDevice(row pgx.Row) (Device, error) {
	var d Device
	var src, typ, ext *string
	err := row.Scan(&d.ID, &d.TenantID, &d.Name, &d.Address, &d.Type, &d.Tags, &d.Notes, &d.ParentID,
		&d.SiteID, &d.PhysicalPeerID, &d.Inventory, &d.ManagedBy, &src, &typ, &ext, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, errs.ErrNotFound
	}
	d.External = extref.FromColumns(src, typ, ext)
	return d, err
}

func (d Device) snapshot() map[string]any {
	return map[string]any{"name": d.Name, "address": d.Address, "type": d.Type, "tags": d.Tags,
		"notes": d.Notes, "parent_id": d.ParentID, "site_id": d.SiteID,
		"physical_peer_id": d.PhysicalPeerID, "external": d.External}
}

// GetDevice returns one device.
func GetDevice(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Device, error) {
	return scanDevice(tx.QueryRow(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = $1`, id))
}

// DeviceByExternal finds a device by external ID.
func DeviceByExternal(ctx context.Context, tx pgx.Tx, k extref.Key) (Device, error) {
	return one(ctx, tx, `SELECT `+deviceCols+` FROM devices WHERE `+extref.Where(1)+` LIMIT 2`, k.Args(), scanDevice)
}

// DeviceFilter narrows ListDevices.
type DeviceFilter struct {
	Type         *string
	SiteID       *uuid.UUID
	ParentID     *uuid.UUID
	Tags         map[string]string // every tag must match
	External     *extref.Key
	Availability []string // any of these
}

// ListDevices returns devices ordered by ID, with their status.
func ListDevices(ctx context.Context, tx pgx.Tx, f DeviceFilter, p Page) ([]Device, error) {
	src, typ, id := keyColumns(f.External)
	tags := f.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	rows, err := tx.Query(ctx, `SELECT d.id, d.tenant_id, d.name, d.address, d.type, d.tags, d.notes, d.parent_id,
		       d.site_id, d.physical_peer_id, d.inventory, d.managed_by, d.external_source, d.external_type,
		       d.external_id, d.created_at, d.updated_at, `+statusCols+`
		  FROM devices d `+statusJoin+`
		 WHERE ($1::text IS NULL OR d.type = $1)
		   AND ($2::uuid IS NULL OR d.site_id = $2)
		   AND ($3::uuid IS NULL OR d.parent_id = $3)
		   AND d.tags @> $4
		   AND ($5::text IS NULL OR d.external_source = $5)
		   AND ($6::text IS NULL OR d.external_type = $6)
		   AND ($7::text IS NULL OR d.external_id = $7)
		   AND ($8::uuid IS NULL OR d.id > $8)
		   AND ($10::text[] IS NULL OR `+availabilitySQL+` = ANY($10))
		 ORDER BY d.id LIMIT $9`, f.Type, f.SiteID, f.ParentID, tags, src, typ, id, p.After, p.Limit, f.Availability)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Device, error) {
		var d Device
		var st Status
		var src, typ, ext *string
		err := r.Scan(append([]any{&d.ID, &d.TenantID, &d.Name, &d.Address, &d.Type, &d.Tags, &d.Notes, &d.ParentID,
			&d.SiteID, &d.PhysicalPeerID, &d.Inventory, &d.ManagedBy, &src, &typ, &ext, &d.CreatedAt, &d.UpdatedAt},
			scanStatus(&st)...)...)
		d.External, d.Status = extref.FromColumns(src, typ, ext), &st
		return d, err
	})
}

// LockDevice reads a device for update, so a read-modify-write (PATCH)
// cannot lose a concurrent change.
func LockDevice(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Device, error) {
	return scanDevice(tx.QueryRow(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = $1 FOR UPDATE`, id))
}

// checkRefs verifies that referenced devices and the site exist in the
// tenant, so a bad ID is a 422 naming the field rather than a 409.
func checkRefs(ctx context.Context, tx pgx.Tx, in DeviceInput) error {
	for field, id := range map[string]*uuid.UUID{"parent_id": in.ParentID, "physical_peer_id": in.PhysicalPeerID} {
		if id == nil {
			continue
		}
		if _, err := GetDevice(ctx, tx, *id); errors.Is(err, errs.ErrNotFound) {
			return errs.Invalidf(field, "device %s does not exist", id)
		} else if err != nil {
			return err
		}
	}
	if in.SiteID != nil {
		if _, err := GetSite(ctx, tx, *in.SiteID); errors.Is(err, errs.ErrNotFound) {
			return errs.Invalidf("site_id", "site %s does not exist", in.SiteID)
		} else if err != nil {
			return err
		}
	}
	return nil
}

// CreateDevice adds a device within the tenant's max_devices limit.
func CreateDevice(ctx context.Context, tx pgx.Tx, actor audit.Actor, tenant uuid.UUID, maxDevices int, in DeviceInput) (Device, error) {
	if err := in.validate(); err != nil {
		return Device{}, err
	}
	if err := lockTenant(ctx, tx, "devices", tenant); err != nil {
		return Device{}, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM devices WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
		return Device{}, err
	}
	if n >= maxDevices {
		return Device{}, errs.Conflictf("limit-reached", "the tenant has reached its limit of %d devices", maxDevices)
	}
	if err := checkRefs(ctx, tx, in); err != nil {
		return Device{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Device{}, err
	}
	src, typ, ext := in.External.Columns()
	d, err := scanDevice(tx.QueryRow(ctx, `
		INSERT INTO devices (id, tenant_id, name, address, type, tags, notes, parent_id, site_id, physical_peer_id,
		                     external_source, external_type, external_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING `+deviceCols,
		id, tenant, in.Name, in.Address, in.Type, nonNilTags(in.Tags), in.Notes, in.ParentID, in.SiteID,
		in.PhysicalPeerID, src, typ, ext))
	if err != nil {
		return Device{}, errs.FromDB(err, deviceConstraints)
	}
	if n+1 == maxDevices {
		if err := incident.LimitReached(ctx, tx, tenant, "devices", maxDevices, n+1); err != nil {
			return Device{}, err
		}
	}
	return d, audit.Write(ctx, tx, actor.Event(tenant, "device.create", "device", id.String(), nil, d.snapshot()))
}

// UpdateDevice replaces a device's client-writable fields. Devices kept by a
// collector only accept changes to tags, notes and external IDs.
func UpdateDevice(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID, in DeviceInput) (Device, error) {
	if err := in.validate(); err != nil {
		return Device{}, err
	}
	cur, err := scanDevice(tx.QueryRow(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return Device{}, err
	}
	next := cur
	next.Tags, next.Notes, next.External = in.Tags, in.Notes, in.External
	if cur.ManagedBy == ManagedByAPI {
		next.Name, next.Address, next.Type = in.Name, in.Address, in.Type
		next.ParentID, next.SiteID, next.PhysicalPeerID = in.ParentID, in.SiteID, in.PhysicalPeerID
	} else if cur.ManagedBy == "file" || !sameManaged(cur, in) {
		return Device{}, writable(cur.ManagedBy)
	}
	if next.Tags == nil {
		next.Tags = map[string]string{}
	}
	before, after := cur.snapshot(), next.snapshot()
	if reflect.DeepEqual(before, after) {
		return cur, nil
	}
	if err := checkRefs(ctx, tx, in); err != nil {
		return Device{}, err
	}
	if next.ParentID != nil && !equalID(cur.ParentID, next.ParentID) {
		if err := lockTenant(ctx, tx, "graph", cur.TenantID); err != nil {
			return Device{}, err
		}
		if err := noCycle(ctx, tx, "parent_id", id, *next.ParentID); err != nil {
			return Device{}, err
		}
	}
	src, typ, ext := next.External.Columns()
	out, err := scanDevice(tx.QueryRow(ctx, `
		UPDATE devices SET name = $2, address = $3, type = $4, tags = $5, notes = $6, parent_id = $7,
		       site_id = $8, physical_peer_id = $9, external_source = $10, external_type = $11,
		       external_id = $12, updated_at = now()
		 WHERE id = $1 RETURNING `+deviceCols,
		id, next.Name, next.Address, next.Type, next.Tags, next.Notes, next.ParentID, next.SiteID,
		next.PhysicalPeerID, src, typ, ext))
	if err != nil {
		return Device{}, errs.FromDB(err, deviceConstraints)
	}
	return out, audit.Write(ctx, tx, actor.Event(cur.TenantID, "device.update", "device", id.String(), before, after))
}

// sameManaged reports whether in keeps every field a collector owns.
func sameManaged(cur Device, in DeviceInput) bool {
	return cur.Name == in.Name && cur.Address == in.Address && cur.Type == in.Type &&
		equalID(cur.ParentID, in.ParentID) && equalID(cur.SiteID, in.SiteID) &&
		equalID(cur.PhysicalPeerID, in.PhysicalPeerID)
}

func equalID(a, b *uuid.UUID) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// DeleteDevice removes a device with its checks and child devices. A device
// with children needs confirm (F02).
func DeleteDevice(ctx context.Context, tx pgx.Tx, actor audit.Actor, id uuid.UUID, confirm bool) error {
	cur, err := GetDevice(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := writable(cur.ManagedBy); err != nil && cur.ManagedBy == "file" {
		return err
	}
	var children int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM devices WHERE parent_id = $1`, id).Scan(&children); err != nil {
		return err
	}
	if children > 0 && !confirm {
		return errs.Conflictf("has-children", "the device contains %d devices, which would be deleted too; repeat with confirm=true", children)
	}
	// End the incidents of every check that goes with the device.
	if err := incident.ResolveForChecks(ctx, tx, `
		WITH RECURSIVE sub(id) AS (
			SELECT $1::uuid UNION SELECT d.id FROM devices d JOIN sub ON d.parent_id = sub.id
		)
		SELECT c.id FROM checks c JOIN sub ON c.device_id = sub.id`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM devices WHERE id = $1`, id); err != nil {
		return err
	}
	before := cur.snapshot()
	before["deleted_children"] = children
	return audit.Write(ctx, tx, actor.Event(cur.TenantID, "device.delete", "device", id.String(), before, nil))
}

func nonNilTags(t map[string]string) map[string]string {
	if t == nil {
		return map[string]string{}
	}
	return maps.Clone(t)
}
