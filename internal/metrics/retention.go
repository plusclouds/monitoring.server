package metrics

import (
	"context"
	"errors"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/errs"
)

// Class is a retention class with how long each level is kept. A nil
// duration keeps the level until a policy is set.
type Class struct {
	ID   int16
	Name string
	Keep Keep
}

// Keep is the retention of each level, in days.
type Keep struct {
	Raw        *int `json:"raw_days"`
	FiveMinute *int `json:"rollup_5m_days"`
	Hourly     *int `json:"rollup_1h_days"`
}

func (k *Keep) level(l string) **int {
	switch l {
	case LevelRaw:
		return &k.Raw
	case Level5m:
		return &k.FiveMinute
	}
	return &k.Hourly
}

var className = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ListClasses returns every retention class with its policies.
func ListClasses(ctx context.Context, tx pgx.Tx) ([]Class, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.id, c.name, p.level, p.keep_days
		  FROM retention_classes c LEFT JOIN retention_policies p ON p.class_id = c.id
		 ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	var out []Class
	var id int16
	var name string
	var level *string
	var days *int
	_, err = pgx.ForEachRow(rows, []any{&id, &name, &level, &days}, func() error {
		if len(out) == 0 || out[len(out)-1].ID != id {
			out = append(out, Class{ID: id, Name: name})
		}
		if level != nil {
			d := *days
			*out[len(out)-1].Keep.level(*level) = &d
		}
		return nil
	})
	return out, err
}

// GetClass returns one class by name.
func GetClass(ctx context.Context, tx pgx.Tx, name string) (Class, error) {
	all, err := ListClasses(ctx, tx)
	if err != nil {
		return Class{}, err
	}
	for _, c := range all {
		if c.Name == name {
			return c, nil
		}
	}
	return Class{}, errs.ErrNotFound
}

// PutClass creates a class or replaces its retention. The audit event goes
// to the platform tenant. It reports whether the class was created.
func PutClass(ctx context.Context, tx pgx.Tx, actor audit.Actor, platform uuid.UUID, name string, keep Keep) (Class, bool, error) {
	if !className.MatchString(name) {
		return Class{}, false, errs.Invalidf("name", "must be lowercase letters, digits and dashes, at most 40 characters")
	}
	for _, l := range []string{LevelRaw, Level5m, Level1h} {
		if d := *keep.level(l); d != nil && (*d < 1 || *d > 36600) {
			return Class{}, false, errs.Invalidf(l, "must be 1 to 36600 days")
		}
	}
	// Rollups are computed from the level below: it must live long enough.
	if r, f := keep.Raw, keep.FiveMinute; r != nil && f != nil && *f < *r {
		return Class{}, false, errs.Invalidf("rollup_5m_days", "must not be shorter than raw_days")
	}
	if f, h := keep.FiveMinute, keep.Hourly; f != nil && h != nil && *h < *f {
		return Class{}, false, errs.Invalidf("rollup_1h_days", "must not be shorter than rollup_5m_days")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('retention_classes'))`); err != nil {
		return Class{}, false, err
	}
	before, err := GetClass(ctx, tx, name)
	created := errors.Is(err, errs.ErrNotFound)
	switch {
	case created:
		if err := tx.QueryRow(ctx, `INSERT INTO retention_classes (name) VALUES ($1) RETURNING id`, name).Scan(&before.ID); err != nil {
			return Class{}, false, err
		}
	case err != nil:
		return Class{}, false, err
	}
	for _, l := range []string{LevelRaw, Level5m, Level1h} {
		d := *keep.level(l)
		if d == nil {
			_, err = tx.Exec(ctx, `DELETE FROM retention_policies WHERE class_id = $1 AND level = $2`, before.ID, l)
		} else {
			_, err = tx.Exec(ctx, `
				INSERT INTO retention_policies (class_id, level, keep_days) VALUES ($1, $2, $3)
				ON CONFLICT (class_id, level) DO UPDATE SET keep_days = excluded.keep_days, updated_at = now()
				WHERE retention_policies.keep_days <> excluded.keep_days`, before.ID, l, *d)
		}
		if err != nil {
			return Class{}, false, err
		}
	}
	after := Class{ID: before.ID, Name: name, Keep: keep}
	var prev any
	if !created {
		if keepEqual(before.Keep, keep) {
			return after, false, nil
		}
		prev = before.Keep
	}
	action := "retention_class.update"
	if created {
		action = "retention_class.create"
	}
	return after, created, audit.Write(ctx, tx, actor.Event(platform, action, "retention_class", name, prev, keep))
}

func keepEqual(a, b Keep) bool {
	eq := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return eq(a.Raw, b.Raw) && eq(a.FiveMinute, b.FiveMinute) && eq(a.Hourly, b.Hourly)
}
