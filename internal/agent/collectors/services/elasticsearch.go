package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/internal/agent"
)

type elasticsearch struct {
	name, url, user, password, token string
	client                           *http.Client
}

func newElasticsearch(o agent.Options) (agent.Collector, error) {
	to, err := o.Duration("timeout", 5*time.Second)
	if err != nil {
		return nil, err
	}
	u := strings.TrimRight(o.String("url", "http://127.0.0.1:9200"), "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return nil, fmt.Errorf("url %q must start with http:// or https://", u)
	}
	return &elasticsearch{name: o.Name, url: u, user: o.String("username", ""), password: o.String("password", ""),
		token: o.String("bearer_token", ""), client: httpClient(to, o.String("insecure_skip_verify", "") == "true")}, nil
}

// Collect reads GET /_cluster/health: green is OK, yellow a warning, red
// critical. The full cluster collector (nodes, JVM, indices) is the server's
// elasticsearch.cluster plugin; this is the in-image view of one node.
func (e *elasticsearch) Collect(ctx context.Context) ([]agent.Object, error) {
	obj := object("elasticsearch", e.name, map[string]string{"url": e.url})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.url+"/_cluster/health", nil)
	if err != nil {
		return nil, err
	}
	switch {
	case e.token != "":
		req.Header.Set("Authorization", "Bearer "+e.token)
	case e.user != "":
		req.SetBasicAuth(e.user, e.password)
	}
	start := time.Now()
	resp, err := e.client.Do(req)
	if err != nil {
		return down(obj, err), nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return down(obj, fmt.Errorf("_cluster/health answered %d", resp.StatusCode)), nil
	}
	var h struct {
		Cluster    string  `json:"cluster_name"`
		Status     string  `json:"status"`
		Nodes      float64 `json:"number_of_nodes"`
		Unassigned float64 `json:"unassigned_shards"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return down(obj, fmt.Errorf("not an elasticsearch answer: %w", err)), nil
	}
	m := obj.Metrics
	m["up"], m["response_ms"], m["es_unassigned_shards"] = 1, ms(time.Since(start)), h.Unassigned
	obj.Labels["cluster"] = h.Cluster
	switch h.Status {
	case "green":
		m["es_status"] = 0
	case "yellow":
		m["es_status"] = 1
		obj.Status, obj.Output = agent.Warning, fmt.Sprintf("cluster is yellow, %.0f unassigned shards", h.Unassigned)
	case "red":
		m["es_status"] = 2
		obj.Status, obj.Output = agent.Critical, fmt.Sprintf("cluster is red, %.0f unassigned shards", h.Unassigned)
	}
	return []agent.Object{obj}, nil
}
