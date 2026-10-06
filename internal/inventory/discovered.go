package inventory

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/errs"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// DiscoveredRetention is how long a discovered device the collector no
// longer reports is kept before it is removed with its checks and history.
const DiscoveredRetention = 7 * 24 * time.Hour

// Discovered devices (ADR-0014): a collector's children, such as an XCP-ng
// pool's hosts and VMs. managed_by is the collector check's ID; the
// collector's key (a UUID) identifies the device across renames and
// migrations. They are not billed and do not count toward max_devices
// (F13 rule 10); only their own checks count.

type discovered struct {
	id      uuid.UUID
	name    string
	address string
	typ     string
	parent  *uuid.UUID
	info    map[string]any
	goneAt  *time.Time
}

// ApplyDiscovered applies a collector's inventory under its device root and
// returns the device of every child key. Children are created, renamed,
// moved (a VM that migrated gets its new host as parent) and, once missing
// for DiscoveredRetention, removed.
func ApplyDiscovered(ctx context.Context, tx pgx.Tx, tenant, check, root uuid.UUID, now time.Time,
	inv plugin.Inventory) (map[string]uuid.UUID, error) {
	if inv.Device != nil {
		if _, err := tx.Exec(ctx, `UPDATE devices SET inventory = inventory || $2, updated_at = now()
			WHERE id = $1 AND NOT inventory @> $2`, root, infoMap(*inv.Device)); err != nil {
			return nil, err
		}
	}
	if inv.Children == nil {
		return nil, nil
	}
	managed := check.String()
	rows, err := tx.Query(ctx, `SELECT id, collector_key, name, address, type, parent_id, inventory, collector_gone_at
		FROM devices WHERE tenant_id = $1 AND managed_by = $2 AND collector_key IS NOT NULL FOR UPDATE`, tenant, managed)
	if err != nil {
		return nil, err
	}
	have := map[string]*discovered{}
	var key string
	var d discovered
	if _, err := pgx.ForEachRow(rows, []any{&d.id, &key, &d.name, &d.address, &d.typ, &d.parent, &d.info, &d.goneAt}, func() error {
		c := d
		if d.parent != nil {
			p := *d.parent
			c.parent = &p
		}
		if d.goneAt != nil {
			g := *d.goneAt
			c.goneAt = &g
		}
		have[key] = &c
		return nil
	}); err != nil {
		return nil, err
	}
	var site *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT site_id FROM devices WHERE id = $1`, root).Scan(&site); err != nil {
		return nil, err
	}
	system := audit.Actor{Kind: audit.ActorSystem, Detail: "collector " + managed}

	ids := map[string]uuid.UUID{}
	// Parents first: a VM needs its host's device. Children whose parent
	// never resolves (a cycle) go under the root.
	upsert := func(c plugin.ChildDevice) error {
		parent := root
		if id, ok := ids[c.ParentKey]; ok {
			parent = id
		}
		id, err := upsertDiscovered(ctx, tx, system, tenant, managed, site, parent, c, have[c.Key])
		ids[c.Key] = id
		return err
	}
	pending := slices.Clone(inv.Children)
	for len(pending) > 0 {
		var later []plugin.ChildDevice
		for _, c := range pending {
			if _, ok := ids[c.ParentKey]; c.ParentKey != "" && !ok {
				later = append(later, c)
				continue
			}
			if err := upsert(c); err != nil {
				return nil, err
			}
		}
		if len(later) == len(pending) { // no progress: a cycle
			for _, c := range later {
				c.ParentKey = ""
				if err := upsert(c); err != nil {
					return nil, err
				}
			}
			break
		}
		pending = later
	}

	// Children no longer reported: mark them, remove them after a while.
	for key, d := range have {
		if _, ok := ids[key]; ok {
			continue
		}
		switch {
		case d.goneAt == nil:
			if _, err := tx.Exec(ctx, `UPDATE devices SET collector_gone_at = $2 WHERE id = $1`, d.id, now); err != nil {
				return nil, err
			}
		case now.Sub(*d.goneAt) > DiscoveredRetention:
			if err := DeleteDevice(ctx, tx, system, d.id, true); err != nil && !errors.Is(err, errs.ErrNotFound) {
				return nil, err
			}
		}
	}
	return ids, nil
}

func upsertDiscovered(ctx context.Context, tx pgx.Tx, actor audit.Actor, tenant uuid.UUID, managed string, site *uuid.UUID,
	parent uuid.UUID, c plugin.ChildDevice, cur *discovered) (uuid.UUID, error) {
	if !slices.Contains(DeviceTypes, c.Type) {
		c.Type = "other"
	}
	info := infoMap(c.Info)
	if cur == nil {
		id, err := uuid.NewV7()
		if err != nil {
			return id, err
		}
		name, err := freeName(ctx, tx, tenant, uuid.Nil, c.Name, c.Key)
		if err != nil {
			return id, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO devices (id, tenant_id, name, address, type, parent_id, site_id, inventory, managed_by, collector_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			id, tenant, name, c.Address, c.Type, parent, site, info, managed, c.Key); err != nil {
			return id, err
		}
		return id, audit.Write(ctx, tx, actor.Event(tenant, "device.discover", "device", id.String(), nil,
			map[string]any{"name": name, "type": c.Type, "collector_key": c.Key, "parent_id": parent}))
	}
	name := cur.name
	if !sameName(cur.name, c.Name, c.Key) {
		var err error
		if name, err = freeName(ctx, tx, tenant, cur.id, c.Name, c.Key); err != nil {
			return cur.id, err
		}
	}
	if name == cur.name && c.Address == cur.address && c.Type == cur.typ && cur.parent != nil && *cur.parent == parent &&
		cur.goneAt == nil && sameInfo(cur.info, info) {
		return cur.id, nil
	}
	_, err := tx.Exec(ctx, `UPDATE devices SET name = $2, address = $3, type = $4, parent_id = $5, inventory = $6,
		collector_gone_at = NULL, updated_at = now() WHERE id = $1`, cur.id, name, c.Address, c.Type, parent, info)
	return cur.id, err
}

// freeName returns name, or name with the key's start when another device
// of the tenant has that name (device names are unique per tenant).
func freeName(ctx context.Context, tx pgx.Tx, tenant, self uuid.UUID, name, key string) (string, error) {
	short := key
	if len(short) > 8 {
		short = short[:8]
	}
	for _, n := range []string{name, name + " [" + short + "]", name + " [" + key + "]"} {
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM devices WHERE tenant_id = $1 AND name = $2 AND id <> $3)`,
			tenant, n, self).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return n, nil
		}
	}
	return name + " [" + uuid.NewString() + "]", nil
}

// sameName reports whether a device's current name is what freeName
// would give for the collector's name, so a disambiguated name is kept.
func sameName(current, name, key string) bool {
	short := key
	if len(short) > 8 {
		short = short[:8]
	}
	return current == name || current == name+" ["+short+"]" || current == name+" ["+key+"]"
}

func infoMap(i plugin.DeviceInfo) map[string]any {
	m := map[string]any{}
	for k, v := range map[string]string{"vendor": i.Vendor, "model": i.Model, "serial": i.Serial, "firmware": i.Firmware} {
		if v != "" {
			m[k] = v
		}
	}
	return m
}

func sameInfo(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}

// deleteDiscovered removes the devices a collector check discovered, when
// the check is deleted.
func deleteDiscovered(ctx context.Context, tx pgx.Tx, actor audit.Actor, check uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT id FROM devices d WHERE managed_by = $1 AND collector_key IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM devices p WHERE p.id = d.parent_id AND p.managed_by = $1)`, check.String())
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := DeleteDevice(ctx, tx, actor, id, true); err != nil && !errors.Is(err, errs.ErrNotFound) {
			return err
		}
	}
	return nil
}
