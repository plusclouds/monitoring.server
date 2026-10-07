package api_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// F07, M4: every query of every provisioned dashboard runs as the Grafana
// role, with Grafana's macros and variables filled in.
func TestGrafanaDashboards(t *testing.T) {
	setup(t, true)
	ctx := context.Background()
	graf, err := pgx.Connect(ctx, strings.Replace(db.SystemDSN, "monitor_system:change-me-system", "monitor_grafana:change-me-grafana", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = graf.Close(ctx) }()
	macros := []struct{ re, with string }{
		{`\$__timeGroupAlias\(([^,]+), \$__interval\)`, `date_bin('1 minute', $1, '2000-01-01') AS time`},
		{`\$__timeFilter\(([^)]+)\)`, `$1 > now() - interval '6 hours'`},
		{`\$__interval_ms`, `60000`},
		{`'\$tenant'`, `'00000000-0000-0000-0000-000000000000'`},
		{`\(\$check\)`, `('00000000-0000-0000-0000-000000000000')`},
		{`'\$device'`, `'00000000-0000-0000-0000-000000000000'`},
		{`'\$metric'`, `'x'`},
		{`\(\$metric\)`, `('x')`},
	}
	files, err := filepath.Glob("../../deploy/grafana/dashboards/*.json")
	if err != nil || len(files) < 7 {
		t.Fatalf("dashboards: %v %v", files, err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // the repository's own dashboards
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			Templating struct {
				List []struct{ Name, Query string }
			}
			Panels []struct {
				Title   string
				Targets []struct {
					RawSQL string `json:"rawSql"`
				}
			}
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var queries []string
		for _, v := range d.Templating.List {
			queries = append(queries, v.Query)
		}
		for _, p := range d.Panels {
			for _, tg := range p.Targets {
				queries = append(queries, tg.RawSQL)
			}
		}
		for _, q := range queries {
			for _, m := range macros {
				q = regexp.MustCompile(m.re).ReplaceAllString(q, m.with)
			}
			if strings.Contains(q, "$") {
				t.Errorf("%s: unreplaced variable in %s", filepath.Base(f), q)
				continue
			}
			rows, err := graf.Query(ctx, q)
			if err == nil {
				rows.Close()
				err = rows.Err()
			}
			if err != nil {
				t.Errorf("%s: %v\n%s", filepath.Base(f), err, q)
			}
		}
	}
}
