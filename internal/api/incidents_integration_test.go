package api_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/plusclouds/monitoring.server/internal/engine"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// F05: state, incidents, acknowledge, comment and manual resolve.
func TestIncidentsAPI(t *testing.T) {
	e := setup(t, true)
	k := e.adminKey
	dev := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "web", "type": "web", "address": "https://example.com"}), 201))
	chk := e.id(e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{"name": "home", "plugin": "http", "failure_count": 1}), 201))
	e.must(e.do("GET", "/v1/checks/"+chk+"/state", k, nil), 404)
	e.must(e.do("POST", "/v1/checks/"+chk+"/run-now", k, nil), 202)

	eng, err := engine.New(engine.Options{System: db.System, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	me := e.must(e.do("GET", "/v1/tenant", k, nil), 200)
	apply := func(st plugin.Status) {
		_, err := eng.Apply(context.Background(), runner.Result{
			TenantID: uuid.MustParse(me.body["id"].(string)), DeviceID: uuid.MustParse(dev), CheckID: uuid.MustParse(chk),
			Plugin: "http", Interval: time.Minute, Result: plugin.Result{Status: st, Output: "boom", Time: time.Now()},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	apply(plugin.Critical)

	state := e.must(e.do("GET", "/v1/checks/"+chk+"/state", k, nil), 200)
	if state.body["phase"] != "PROBLEM" || state.body["incident_id"] == nil {
		t.Fatalf("state: %s", state.raw)
	}
	list := e.must(e.do("GET", "/v1/incidents?status=active", k, nil), 200)
	items := list.body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("incidents: %s", list.raw)
	}
	inc := items[0].(map[string]any)["id"].(string)

	ro := e.must(e.do("POST", "/v1/api-keys", k, map[string]any{"name": "ro", "role": "read-only"}), 201).body["key"].(string)
	e.must(e.do("POST", "/v1/incidents/"+inc+"/ack", ro, nil), 403)
	ack := e.must(e.do("POST", "/v1/incidents/"+inc+"/ack", k, nil), 200)
	if ack.body["status"] != "acknowledged" || ack.body["acknowledged_at"] == nil {
		t.Errorf("ack: %s", ack.raw)
	}
	e.must(e.do("POST", "/v1/incidents/"+inc+"/ack", k, nil), 200) // idempotent
	if n := auditCount(t, "incident.acknowledge"); n != 1 {
		t.Errorf("incident.acknowledge events = %d, want 1", n)
	}
	e.must(e.do("POST", "/v1/incidents/"+inc+"/comments", k, map[string]any{"body": "looking"}), 201)
	got := e.must(e.do("GET", "/v1/incidents/"+inc, k, nil), 200)
	if c := got.body["comments"].([]any); len(c) != 1 || c[0].(map[string]any)["body"] != "looking" {
		t.Errorf("comments: %s", got.raw)
	}

	res := e.must(e.do("POST", "/v1/incidents/"+inc+"/resolve", k, nil), 200)
	if res.body["status"] != "resolved" || res.body["resolved_by"] != "manual" {
		t.Errorf("resolve: %s", res.raw)
	}
	if st := e.must(e.do("GET", "/v1/checks/"+chk+"/state", k, nil), 200); st.body["phase"] != "OK" || st.body["incident_id"] != nil {
		t.Errorf("state after manual resolve: %s", st.raw)
	}
	e.must(e.do("POST", "/v1/incidents/"+inc+"/ack", k, nil), 409)

	// A persisting problem opens a new incident.
	apply(plugin.Critical)
	if l := e.must(e.do("GET", "/v1/incidents", k, nil), 200); len(l.body["items"].([]any)) != 2 {
		t.Errorf("after the problem persists: %s", l.raw)
	}
	var types []string
	rows, err := db.System.Query(context.Background(), `SELECT type FROM events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		types = append(types, s)
	}
	want := []string{"monitoring.incident.opened", "monitoring.incident.acknowledged", "monitoring.incident.commented",
		"monitoring.incident.resolved", "monitoring.incident.opened"}
	if len(types) != len(want) {
		t.Fatalf("events %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("event %d = %s, want %s", i, types[i], want[i])
		}
	}
}
