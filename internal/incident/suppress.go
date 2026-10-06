package incident

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Root is the incident that explains another one: an open availability
// incident (a host check's, or an availability object's such as an XCP-ng
// host) of the device itself or of a device it depends on (F05).
type Root struct {
	IncidentID uuid.UUID
	DeviceID   uuid.UUID
}

// FindRoot returns the nearest open availability incident upstream of a
// device: on the device itself (unless it is the caller's own: the same
// check and object), then on its containers and the devices it depends
// on, transitively. An availability object counts once it is CRITICAL. A
// suppressed root is followed to its own root while that one is open.
func FindRoot(ctx context.Context, tx pgx.Tx, device, check uuid.UUID, object *string) (*Root, error) {
	var r Root
	err := tx.QueryRow(ctx, `
		WITH RECURSIVE up(id, depth, path) AS (
			SELECT $1::uuid, 0, ARRAY[$1::uuid]
			UNION ALL
			SELECT n.id, up.depth + 1, up.path || n.id
			  FROM up CROSS JOIN LATERAL (
			        SELECT d.parent_id AS id FROM devices d WHERE d.id = up.id AND d.parent_id IS NOT NULL
			        UNION
			        SELECT dd.depends_on_id FROM device_dependencies dd WHERE dd.device_id = up.id) n
			 WHERE NOT n.id = ANY (up.path) AND up.depth < 20
		)
		SELECT CASE WHEN r.status <> 'resolved' THEN r.id ELSE i.id END,
		       CASE WHEN r.status <> 'resolved' THEN r.device_id ELSE i.device_id END
		  FROM up
		  JOIN incidents i ON i.device_id = up.id AND i.availability AND i.status <> 'resolved'
		                  AND (i.object_key IS NULL OR i.severity = 'critical')
		  LEFT JOIN incidents r ON r.id = i.root_incident_id
		 WHERE up.depth > 0 OR i.check_id IS DISTINCT FROM $2 OR coalesce(i.object_key, '') <> coalesce($3, '')
		 ORDER BY up.depth, i.opened_at
		 LIMIT 1`, device, check, object).Scan(&r.IncidentID, &r.DeviceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

// HasUpstream reports whether anything upstream could explain a failure of
// check on device, so its notification waits for the dependency grace.
func HasUpstream(ctx context.Context, tx pgx.Tx, device, check uuid.UUID) (bool, error) {
	var yes bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM devices WHERE id = $1 AND parent_id IS NOT NULL)
		OR EXISTS (SELECT 1 FROM device_dependencies WHERE device_id = $1)
		OR EXISTS (SELECT 1 FROM checks WHERE device_id = $1 AND is_host_check AND id <> $2)`, device, check).Scan(&yes)
	return yes, err
}

// Suppress marks an open incident suppressed when a root explains it now.
// The router calls it when the dependency grace has passed. It reports
// whether the incident is suppressed.
func Suppress(ctx context.Context, tx pgx.Tx, id uuid.UUID) (bool, error) {
	inc, err := getForUpdate(ctx, tx, id)
	if err != nil || inc.Status == StatusResolved || inc.CheckID == nil || inc.DeviceID == nil {
		return inc.Suppressed, err
	}
	if inc.Suppressed {
		return true, nil
	}
	root, err := FindRoot(ctx, tx, *inc.DeviceID, *inc.CheckID, inc.ObjectKey)
	if err != nil || root == nil || root.IncidentID == inc.ID {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE incidents SET suppressed = true, root_incident_id = $2, root_device_id = $3,
		updated_at = now() WHERE id = $1`, id, root.IncidentID, root.DeviceID)
	return err == nil, err
}

// release re-evaluates the open incidents a resolved root suppressed: each
// links to another root that still explains it, or stops being suppressed
// and is announced with a fresh opened event.
func release(ctx context.Context, tx pgx.Tx, root uuid.UUID) error {
	// Availability incidents first: the other incidents of their devices
	// then find them as their new root.
	rows, err := tx.Query(ctx, `SELECT `+prefixed("i.")+` FROM incidents i
		WHERE i.root_incident_id = $1 AND i.status <> 'resolved'
		ORDER BY i.availability DESC, i.opened_at FOR UPDATE OF i`, root)
	if err != nil {
		return err
	}
	children, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Incident, error) { return scan(r) })
	if err != nil {
		return err
	}
	for _, c := range children {
		var next *Root
		if c.CheckID != nil && c.DeviceID != nil {
			if next, err = FindRoot(ctx, tx, *c.DeviceID, *c.CheckID, c.ObjectKey); err != nil {
				return err
			}
		}
		if next != nil && next.IncidentID != root && next.IncidentID != c.ID {
			if _, err := tx.Exec(ctx, `UPDATE incidents SET root_incident_id = $2, root_device_id = $3, updated_at = now()
				WHERE id = $1`, c.ID, next.IncidentID, next.DeviceID); err != nil {
				return err
			}
			continue
		}
		inc, err := scan(tx.QueryRow(ctx, `UPDATE incidents SET suppressed = false, root_incident_id = NULL,
			root_device_id = device_id, updated_at = now() WHERE id = $1 RETURNING `+cols, c.ID))
		if err != nil {
			return err
		}
		if err := emit(ctx, tx, EventOpened, inc, nil, map[string]any{"unsuppressed": true}); err != nil {
			return err
		}
	}
	return nil
}

// prefixed is cols with every column qualified by p.
func prefixed(p string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = p + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}
