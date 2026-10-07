package inventory

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Availability of a device, from its host check (v0.2.1).
const (
	AvailabilityUp          = "up"          // host check OK or PENDING
	AvailabilityDown        = "down"        // host check in PROBLEM
	AvailabilityUnusual     = "unusual"     // host check in PROBLEM only through Whoopsy!: it works, but not as usual
	AvailabilityUnknown     = "unknown"     // host check UNKNOWN or without a result yet
	AvailabilityUnmonitored = "unmonitored" // no host check
	AvailabilityDisabled    = "disabled"    // host check disabled
)

// Status is a device's rolled-up state: whether it is up, from its host
// check, and how healthy it is, from its open incidents.
type Status struct {
	Availability  string
	Since         *time.Time // when availability last changed
	Health        string     // ok, warning or critical: the worst open incident
	OpenIncidents int
}

// statusJoin adds the columns of statusCols for a device aliased d.
const statusJoin = `
	LEFT JOIN checks hc ON hc.device_id = d.id AND hc.is_host_check
	LEFT JOIN check_state hs ON hs.check_id = hc.id
	LEFT JOIN LATERAL (
		SELECT count(*) AS n, coalesce(bool_or(i.severity = 'critical'), false) AS critical
		  FROM incidents i WHERE i.device_id = d.id AND i.status <> 'resolved'
	) inc ON true`

// availabilitySQL is the availability of the device in statusJoin.
const availabilitySQL = `CASE
		WHEN hc.id IS NULL THEN 'unmonitored'
		WHEN NOT hc.enabled THEN 'disabled'
		WHEN hs.check_id IS NULL OR hs.status = 'UNKNOWN' THEN 'unknown'
		WHEN hs.phase = 'PROBLEM' AND ` + whoopsyOnlySQL + ` THEN 'unusual'
		WHEN hs.phase = 'PROBLEM' THEN 'down'
		ELSE 'up' END`

// whoopsyOnlySQL holds when the host check's problem comes only from its
// Whoopsy! band: the plugin itself reports OK and no fixed threshold is
// breached. The device works, just not as usual.
const whoopsyOnlySQL = `(hs.machine->'whoopsy'->>'level' IS NOT NULL AND hs.last_status = 'OK'
		AND NOT jsonb_path_exists(hs.machine, '$.rules.*.level'))`

const statusCols = availabilitySQL + `,
	CASE
		WHEN hc.id IS NULL THEN NULL
		WHEN NOT hc.enabled THEN hc.updated_at
		WHEN hs.check_id IS NULL THEN NULL
		WHEN hs.status = 'UNKNOWN' THEN hs.since
		ELSE coalesce(hs.availability_since, hs.since) END,
	CASE WHEN inc.critical THEN 'critical' WHEN inc.n > 0 THEN 'warning' ELSE 'ok' END,
	inc.n`

func scanStatus(dst *Status) []any {
	return []any{&dst.Availability, &dst.Since, &dst.Health, &dst.OpenIncidents}
}

// FillStatus sets Status on each device.
func FillStatus(ctx context.Context, tx pgx.Tx, devs []Device) error {
	if len(devs) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(devs))
	for i, d := range devs {
		ids[i] = d.ID
	}
	rows, err := tx.Query(ctx, `SELECT d.id, `+statusCols+` FROM devices d `+statusJoin+` WHERE d.id = ANY($1)`, ids)
	if err != nil {
		return err
	}
	byID := map[uuid.UUID]Status{}
	for rows.Next() {
		var id uuid.UUID
		var st Status
		if err := rows.Scan(append([]any{&id}, scanStatus(&st)...)...); err != nil {
			rows.Close()
			return err
		}
		byID[id] = st
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range devs {
		st := byID[devs[i].ID]
		devs[i].Status = &st
	}
	return nil
}
