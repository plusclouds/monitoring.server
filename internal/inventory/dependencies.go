package inventory

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/internal/extref"
)

// Dependency is a stored "needs to work" edge (ADR-0014). Containment is an
// implied dependency and is reported with Source "containment".
type Dependency struct {
	DeviceID    uuid.UUID
	DependsOnID uuid.UUID
	Source      string
	CreatedAt   *time.Time
}

// upstreamSQL lists every device $1 depends on, directly or transitively,
// through stored dependencies and containment, with the distance.
const upstreamSQL = `
	WITH RECURSIVE up(id, depth) AS (
		SELECT e.up, 1 FROM (
			SELECT depends_on_id AS up FROM device_dependencies WHERE device_id = $1
			UNION SELECT parent_id FROM devices WHERE id = $1 AND parent_id IS NOT NULL
		) e
		UNION
		SELECT e.up, up.depth + 1 FROM up CROSS JOIN LATERAL (
			SELECT depends_on_id AS up FROM device_dependencies WHERE device_id = up.id
			UNION SELECT parent_id FROM devices WHERE id = up.id AND parent_id IS NOT NULL
		) e
		WHERE up.depth < 100
	)
	SELECT id, min(depth) FROM up GROUP BY id`

// downstreamSQL lists every device that depends on $1, directly or
// transitively: what is suppressed when $1 fails (the impact).
const downstreamSQL = `
	WITH RECURSIVE down(id, depth) AS (
		SELECT e.down, 1 FROM (
			SELECT device_id AS down FROM device_dependencies WHERE depends_on_id = $1
			UNION SELECT id FROM devices WHERE parent_id = $1
		) e
		UNION
		SELECT e.down, down.depth + 1 FROM down CROSS JOIN LATERAL (
			SELECT device_id AS down FROM device_dependencies WHERE depends_on_id = down.id
			UNION SELECT id FROM devices WHERE parent_id = down.id
		) e
		WHERE down.depth < 100
	)
	SELECT id, min(depth) FROM down GROUP BY id`

// noCycle returns an *errs.Invalid for field when device would depend on
// itself after adding an edge from device to upstream. The caller holds the
// tenant's graph lock.
func noCycle(ctx context.Context, tx pgx.Tx, field string, device, upstream uuid.UUID) error {
	if device == upstream {
		return errs.Invalidf(field, "a device cannot depend on itself")
	}
	var found bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM (`+upstreamSQL+`) u WHERE u.id = $2)`, upstream, device).Scan(&found)
	if err != nil {
		return err
	}
	if found {
		return errs.Invalidf(field, "device %s already depends on %s, so this would create a cycle", upstream, device)
	}
	return nil
}

// AddDependency records that device needs dependsOn to work. Adding an
// existing edge is a no-op.
func AddDependency(ctx context.Context, tx pgx.Tx, actor audit.Actor, device, dependsOn uuid.UUID) (bool, error) {
	d, err := GetDevice(ctx, tx, device)
	if err != nil {
		return false, err
	}
	if _, err := GetDevice(ctx, tx, dependsOn); errors.Is(err, errs.ErrNotFound) {
		return false, errs.Invalidf("depends_on_id", "device %s does not exist", dependsOn)
	} else if err != nil {
		return false, err
	}
	if err := lockTenant(ctx, tx, "graph", d.TenantID); err != nil {
		return false, err
	}
	if err := noCycle(ctx, tx, "depends_on_id", device, dependsOn); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO device_dependencies (tenant_id, device_id, depends_on_id)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, d.TenantID, device, dependsOn)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	return true, audit.Write(ctx, tx, actor.Event(d.TenantID, "device.dependency.add", "device", device.String(),
		nil, map[string]any{"depends_on_id": dependsOn}))
}

// RemoveDependency deletes a stored edge; removing a missing edge is a no-op.
func RemoveDependency(ctx context.Context, tx pgx.Tx, actor audit.Actor, device, dependsOn uuid.UUID) error {
	d, err := GetDevice(ctx, tx, device)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM device_dependencies WHERE device_id = $1 AND depends_on_id = $2`, device, dependsOn)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return audit.Write(ctx, tx, actor.Event(d.TenantID, "device.dependency.remove", "device", device.String(),
		map[string]any{"depends_on_id": dependsOn}, nil))
}

// Dependencies lists what device depends on directly: stored edges plus
// its container.
func Dependencies(ctx context.Context, tx pgx.Tx, device uuid.UUID) ([]Dependency, error) {
	d, err := GetDevice(ctx, tx, device)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT device_id, depends_on_id, source, created_at FROM device_dependencies
		WHERE device_id = $1 ORDER BY created_at, depends_on_id`, device)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, scanDependency)
	if err != nil {
		return nil, err
	}
	if d.ParentID != nil {
		out = append([]Dependency{{DeviceID: device, DependsOnID: *d.ParentID, Source: "containment"}}, out...)
	}
	return out, nil
}

// Dependents lists the devices that depend on device directly: stored edges
// plus the devices it contains.
func Dependents(ctx context.Context, tx pgx.Tx, device uuid.UUID) ([]Dependency, error) {
	if _, err := GetDevice(ctx, tx, device); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT id, parent_id, 'containment', NULL::timestamptz FROM devices WHERE parent_id = $1
		UNION ALL
		SELECT device_id, depends_on_id, source, created_at FROM device_dependencies WHERE depends_on_id = $1
		ORDER BY 1`, device)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanDependency)
}

func scanDependency(r pgx.CollectableRow) (Dependency, error) {
	var d Dependency
	err := r.Scan(&d.DeviceID, &d.DependsOnID, &d.Source, &d.CreatedAt)
	return d, err
}

// Impacted is a device that depends on another, at a distance in edges.
type Impacted struct {
	Device   Device
	Distance int
}

// Impact lists every device that would be suppressed if device failed,
// nearest first.
func Impact(ctx context.Context, tx pgx.Tx, device uuid.UUID) ([]Impacted, error) {
	if _, err := GetDevice(ctx, tx, device); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT d.id, d.tenant_id, d.name, d.address, d.type, d.tags, d.notes, d.parent_id,
		       d.site_id, d.physical_peer_id, d.inventory, d.managed_by, d.external_source, d.external_type,
		       d.external_id, d.created_at, d.updated_at, x.min
		  FROM (`+downstreamSQL+`) x JOIN devices d ON d.id = x.id
		 WHERE x.id <> $1
		 ORDER BY x.min, d.name`, device)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Impacted, error) {
		var i Impacted
		var src, typ, ext *string
		d := &i.Device
		err := r.Scan(&d.ID, &d.TenantID, &d.Name, &d.Address, &d.Type, &d.Tags, &d.Notes, &d.ParentID,
			&d.SiteID, &d.PhysicalPeerID, &d.Inventory, &d.ManagedBy, &src, &typ, &ext, &d.CreatedAt,
			&d.UpdatedAt, &i.Distance)
		d.External = extref.FromColumns(src, typ, ext)
		return i, err
	})
}
