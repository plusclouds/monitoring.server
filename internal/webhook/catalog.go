package webhook

import (
	"github.com/plusclouds/monitoring.server/internal/incident"
)

// EventInfo describes one notification a customer can receive (F06).
type EventInfo struct {
	Type        string
	Description string
	// Scope is "incident", "tenant" or "test".
	Scope string
	// Default: routes deliver it without naming it in match.event_types.
	Default bool
	// Trigger says what has to be set up for the event to be sent, when it
	// is not sent on its own.
	Trigger string
	Example map[string]any
}

// Catalog lists the notifications a webhook can receive, each with the
// payload as the receiver gets it. The examples use made-up values; the
// shape is the one the notifier sends.
func Catalog() []EventInfo {
	incidentEvent := func(typ, desc, trigger string, def bool, tweak func(obj, data map[string]any)) EventInfo {
		data, obj := exampleIncident()
		if tweak != nil {
			tweak(obj, data)
		}
		return EventInfo{Type: typ, Description: desc, Scope: "incident", Default: def, Trigger: trigger,
			Example: exampleEnvelope(typ, "0192f7c4-4a00-7000-8000-00000000a001", data)}
	}
	ack := func(obj, data map[string]any) {
		obj["status"] = "acknowledged"
		obj["acknowledged_at"] = "2026-10-12T09:20:00Z"
		obj["acknowledged_by"] = "0192f7a0-0000-7000-8000-0000000000a1"
		obj["acknowledged_by_external_id"] = "5d2f…"
		data["actor"] = map[string]any{"kind": "user", "user_id": "0192f7a0-0000-7000-8000-0000000000a1", "external_id": "5d2f…"}
	}
	return []EventInfo{
		incidentEvent(incident.EventOpened,
			"A check crossed a threshold and an incident opened. Also sent when a suppressed incident stops being explained by an upstream failure.",
			"", true, nil),
		incidentEvent(incident.EventUpdated,
			"The incident's severity changed, or it started or stopped flapping.", "", true,
			func(obj, _ map[string]any) { obj["severity"] = "warning"; obj["flapping"] = true }),
		incidentEvent(incident.EventAcknowledged, "A user acknowledged the incident.", "", true, ack),
		incidentEvent(incident.EventResolved,
			"The incident ended: the check recovered, or it was resolved by a user, a deleted or disabled check, or a vanished object.",
			"", true, func(obj, _ map[string]any) {
				obj["status"] = "resolved"
				obj["resolved_at"] = "2026-10-12T09:40:00Z"
				obj["resolved_by"] = "recovery"
			}),
		incidentEvent(incident.EventRenotify,
			"A reminder: the incident is still open and unacknowledged after the route's repeat interval.",
			"Set repeat_interval_seconds on the alert route.", false, nil),
		incidentEvent(incident.EventEscalated,
			"A later step of the route's escalation is due, with that step's labels.",
			"Give the alert route more than one step.", false, func(_, data map[string]any) {
				data["route"] = map[string]any{"id": exampleRouteID, "name": "datacenter critical", "step": 1,
					"labels": map[string]any{"recipient": "on-call"}}
			}),
		incidentEvent(incident.EventCommented,
			"Someone commented on the incident. data.object.comment holds the comment.",
			"List this type in match.event_types of the alert route.", false, func(obj, data map[string]any) {
				obj["comment"] = map[string]any{"id": "0192f7c4-4a00-7000-8000-00000000b001", "author_kind": "user",
					"author_user_id": "0192f7a0-0000-7000-8000-0000000000a1", "author_external_id": "5d2f…",
					"body": "Looking into it.", "created_at": "2026-10-12T09:18:00Z"}
				data["actor"] = map[string]any{"kind": "user", "user_id": "0192f7a0-0000-7000-8000-0000000000a1", "external_id": "5d2f…"}
			}),
		tenantExample(incident.EventDeviceLimitReached, "devices",
			"The account reached its device limit; the next device create is refused."),
		tenantExample(incident.EventCheckLimitReached, "checks",
			"The account reached its check limit; the next check create is refused."),
		{Type: "monitoring.webhook.test", Scope: "test",
			Description: "A test sent on request with POST /v1/webhooks/{id}/test. data.object is the webhook.",
			Trigger:     "POST /v1/webhooks/{id}/test.",
			Example: exampleEnvelope("monitoring.webhook.test", exampleWebhookID, map[string]any{
				"account_id": exampleAccount, "object_type": `Monitoring\Webhooks`,
				"object": map[string]any{"id": exampleWebhookID, "name": "ops channel", "url": "https://hooks.example.com/monitoring",
					"enabled": true},
				"actor": map[string]any{"kind": "user", "user_id": "0192f7a0-0000-7000-8000-0000000000a1", "external_id": "5d2f…"}})},
	}
}

// RouteMatchFields are the filters of an alert route's match, with what
// each one tests.
var RouteMatchFields = map[string]string{
	"event_types":  "Event types to deliver. Empty means the defaults of the catalog.",
	"severity":     "warning or critical.",
	"device_types": "The device's type.",
	"site_ids":     "The device's site.",
	"device_ids":   "Specific devices.",
	"check_ids":    "Specific checks.",
	"tags":         "Tags the device carries; every key and value must be present.",
}

const (
	exampleRouteID   = "0192f7a0-0000-7000-8000-00000000f001"
	exampleWebhookID = "0192f7a0-0000-7000-8000-00000000f101"
	exampleAccount   = "9b1c2d3e-0000-4000-8000-000000000001"
)

func tenantExample(typ, resource, desc string) EventInfo {
	return EventInfo{Type: typ, Description: desc, Scope: "tenant", Default: true,
		Trigger: "Only routes without device, check or severity filters deliver it.",
		Example: exampleEnvelope(typ, "tenant:0192f7b0-0000-7000-8000-000000000001", map[string]any{
			"account_id": exampleAccount, "object_type": `Monitoring\Tenants`,
			"object": map[string]any{"tenant_id": "0192f7b0-0000-7000-8000-000000000001", "resource": resource,
				"limit": 50, "count": 50},
			"device": nil, "check": nil, "route": routeExample(0), "actor": nil})}
}

func routeExample(step int) map[string]any {
	return map[string]any{"id": exampleRouteID, "name": "datacenter critical", "step": step,
		"labels": map[string]any{"recipient": "noc"}}
}

func exampleEnvelope(typ, subject string, data map[string]any) map[string]any {
	return map[string]any{
		"specversion": "1.0", "id": "0192f7c4-5b1e-7a3c-9d2e-3f4a5b6c7d8e",
		"source": "/monitoring/0192f7a0-0000-7000-8000-000000000001/0192f7b0-0000-7000-8000-000000000001",
		"type":   typ, "time": "2026-10-12T09:15:00Z", "datacontenttype": "application/json",
		"subject": subject, "data": data,
	}
}

// exampleIncident returns data of an opened incident, and its object.
func exampleIncident() (data, obj map[string]any) {
	obj = map[string]any{
		"id": "0192f7c4-4a00-7000-8000-00000000a001", "check_id": "0192f7a0-0000-7000-8000-00000000c001",
		"device_id": "0192f7a0-0000-7000-8000-00000000d001", "object_key": nil, "object_name": nil,
		"severity": "critical", "status": "open", "summary": "CPU load 97 % (critical above 90 %)",
		"last_output": "load=97", "rule_id": "cpu-critical", "rule_name": "CPU critical", "flapping": false,
		"suppressed": false, "root_incident_id": nil, "root_device_id": "0192f7a0-0000-7000-8000-00000000d001",
		"opened_at": "2026-10-12T09:15:00Z", "acknowledged_at": nil, "acknowledged_by": nil,
		"acknowledged_by_external_id": nil, "resolved_at": nil, "resolved_by": nil, "updated_at": "2026-10-12T09:15:00Z",
	}
	data = map[string]any{
		"account_id": exampleAccount, "object_type": incident.ObjectType, "object": obj,
		"device": map[string]any{
			"id": "0192f7a0-0000-7000-8000-00000000d001", "name": "web-01", "address": "10.0.0.11", "type": "server",
			"site_id": "0192f7a0-0000-7000-8000-00000000e001", "tags": map[string]any{"rack": "a12"},
			"external": map[string]any{"source": "plusclouds", "type": `NextDeveloper\IAAS\Database\Models\VirtualMachines`, "id": "9b1c…"},
			"path":     []any{map[string]any{"id": "0192f7a0-0000-7000-8000-00000000b001", "name": "hv-07", "external": nil}},
		},
		"check": map[string]any{
			"id": "0192f7a0-0000-7000-8000-00000000c001", "name": "CPU load", "plugin": "cpu", "last_output": "load=97",
			"rule_id": "cpu-critical", "rule_name": "CPU critical", "runbook_url": nil, "thresholds": []any{},
		},
		"route": routeExample(0), "actor": nil,
		"links": map[string]any{"incident": "https://panel.example.com/monitoring/incidents/0192f7c4-4a00-7000-8000-00000000a001"},
	}
	return data, obj
}
