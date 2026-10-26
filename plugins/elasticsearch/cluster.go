package elasticsearch

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&Cluster{}) }

// Config of an elasticsearch.cluster collector.
type Config struct {
	Scheme               string `json:"scheme,omitempty" jsonschema:"enum=http,enum=https,default=https,description=Used when the device address is not a URL"`
	Port                 int    `json:"port,omitempty" jsonschema:"minimum=1,maximum=65535,default=9200,description=Used when the device address has no port"`
	VerifyCertificate    *bool  `json:"verify_certificate,omitempty" jsonschema:"default=true,description=Verify the cluster's TLS certificate. Turn off for a private CA the engine does not trust"`
	IndexPattern         string `json:"index_pattern,omitempty" jsonschema:"default=*,maxLength=256,description=Indices to report, as an Elasticsearch index pattern such as logs-*,metrics-*"`
	IncludeSystemIndices bool   `json:"include_system_indices,omitempty" jsonschema:"description=Report indices whose name starts with a dot"`
	MaxIndices           int    `json:"max_indices,omitempty" jsonschema:"minimum=0,maximum=1000,default=100,description=Most indices reported, the largest first. 0 reports none"`
}

var defaults = Config{Scheme: "https", Port: 9200, IndexPattern: "*", MaxIndices: 100}

// Metric slots, per object (cluster, node, index).
const (
	mHealth = iota
	mNodes
	mDataNodes
	mActiveShards
	mUnassigned
	mRelocating
	mInitializing
	mPendingTasks
	mHeap
	mCPU
	mDisk
	mGCOld
	mRejected
	mDocs
	mStore
	metricCount
)

// Cluster implements elasticsearch.cluster.
type Cluster struct{}

func (*Cluster) Manifest() plugin.Manifest {
	g := func(name, unit, desc string) plugin.MetricDef {
		return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "gauge", RetentionClass: plugin.RetentionStandard}
	}
	r := func(name, unit, desc string) plugin.MetricDef {
		return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "rate", RetentionClass: plugin.RetentionStandard}
	}
	return plugin.Manifest{
		Type: "elasticsearch.cluster",
		Kind: plugin.KindCollector,
		Description: "Elasticsearch cluster through its REST API: the cluster (health, shards, pending tasks), " +
			"every node (heap, CPU, disk, old-generation GC, rejected requests) and the largest indices (documents, size, health) are objects.",
		ConfigSchema:    plugin.SchemaFor[Config](),
		CredentialTypes: []string{"http_basic", "http_bearer"},
		Metrics: []plugin.MetricDef{
			g("health_code", "1", "Cluster and index health: 0 green, 1 yellow, 2 red"),
			g("nodes", "1", "Nodes in the cluster"),
			g("data_nodes", "1", "Data nodes in the cluster"),
			g("active_shards", "1", "Active shards"),
			g("unassigned_shards", "1", "Unassigned shards"),
			g("relocating_shards", "1", "Relocating shards"),
			g("initializing_shards", "1", "Initializing shards"),
			g("pending_tasks", "1", "Cluster tasks waiting for the master"),
			g("heap_used_percent", "percent", "JVM heap in use (nodes)"),
			g("cpu_percent", "percent", "CPU use (nodes)"),
			g("disk_used_percent", "percent", "Data path use (nodes)"),
			r("gc_old_ms_per_s", "ms/s", "Old-generation garbage collection time (nodes)"),
			r("rejected_per_s", "1/s", "Search and write requests rejected by a full queue (nodes)"),
			g("docs", "1", "Documents (indices)"),
			g("store_bytes", "By", "Size on disk (indices)"),
		},
		DefaultInterval: time.Minute,
		MinInterval:     30 * time.Second,
		Slow:            true,
		PerTargetLimit:  1,
		BillingClass:    plugin.BillingStandard,
		// Each node bills, not the cluster or its indices.
		BillableObjects: map[string]string{"node:": "node"},
	}
}

func (*Cluster) Validate(raw json.RawMessage) error {
	cfg, err := plugin.DecodeConfig(raw, defaults)
	if err != nil {
		return err
	}
	if cfg.Scheme != "http" && cfg.Scheme != "https" {
		return fmt.Errorf("scheme %q must be http or https", cfg.Scheme)
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("port %d is out of range", cfg.Port)
	}
	if cfg.MaxIndices < 0 || cfg.MaxIndices > 1000 {
		return fmt.Errorf("max_indices %d must be between 0 and 1000", cfg.MaxIndices)
	}
	return nil
}

// API records, only the fields read.
type (
	healthRecord struct {
		ClusterName  string `json:"cluster_name"`
		Status       string `json:"status"`
		Nodes        int    `json:"number_of_nodes"`
		DataNodes    int    `json:"number_of_data_nodes"`
		ActiveShards int    `json:"active_shards"`
		Relocating   int    `json:"relocating_shards"`
		Initializing int    `json:"initializing_shards"`
		Unassigned   int    `json:"unassigned_shards"`
		PendingTasks int    `json:"number_of_pending_tasks"`
	}
	nodeStats struct {
		Nodes map[string]nodeRecord `json:"nodes"`
	}
	nodeRecord struct {
		Name  string   `json:"name"`
		Host  string   `json:"host"`
		IP    string   `json:"ip"`
		Roles []string `json:"roles"`
		JVM   struct {
			Mem struct {
				HeapUsedPercent *float64 `json:"heap_used_percent"`
			} `json:"mem"`
			GC struct {
				Collectors map[string]struct {
					TimeMillis float64 `json:"collection_time_in_millis"`
				} `json:"collectors"`
			} `json:"gc"`
		} `json:"jvm"`
		OS struct {
			CPU struct {
				Percent *float64 `json:"percent"`
			} `json:"cpu"`
		} `json:"os"`
		Process struct {
			CPU struct {
				Percent *float64 `json:"percent"`
			} `json:"cpu"`
		} `json:"process"`
		FS struct {
			Total struct {
				Total     float64 `json:"total_in_bytes"`
				Available float64 `json:"available_in_bytes"`
			} `json:"total"`
		} `json:"fs"`
		ThreadPool map[string]struct {
			Rejected float64 `json:"rejected"`
		} `json:"thread_pool"`
	}
	// indexRecord is a _cat/indices row; every value is a string, null for a
	// closed index.
	indexRecord struct {
		Index     string `json:"index"`
		Health    string `json:"health"`
		Status    string `json:"status"`
		DocsCount string `json:"docs.count"`
		StoreSize string `json:"store.size"`
	}
)

func (*Cluster) Collect(ctx context.Context, t plugin.Target) (plugin.Batch, plugin.Inventory, error) {
	cfg, err := plugin.DecodeConfig(t.Config, defaults)
	if err != nil {
		return plugin.Batch{}, plugin.Inventory{}, err
	}
	base, err := baseURL(t.Address, cfg)
	if err != nil {
		return plugin.Batch{Status: plugin.Unknown, Output: err.Error()}, plugin.Inventory{}, nil
	}
	c := newClient(t, base, cfg.VerifyCertificate == nil || *cfg.VerifyCertificate)
	defer c.close()

	var health healthRecord
	if err := c.get(ctx, "/_cluster/health", &health); err != nil {
		return failed(base, err), plugin.Inventory{}, nil
	}
	var stats nodeStats
	if err := c.get(ctx, "/_nodes/stats/jvm,os,process,fs,thread_pool?filter_path=nodes.*.name,nodes.*.host,nodes.*.ip,nodes.*.roles,nodes.*.jvm.mem.heap_used_percent,nodes.*.jvm.gc.collectors.*.collection_time_in_millis,nodes.*.os.cpu.percent,nodes.*.process.cpu.percent,nodes.*.fs.total,nodes.*.thread_pool.search.rejected,nodes.*.thread_pool.write.rejected", &stats); err != nil {
		return failed(base, err), plugin.Inventory{}, nil
	}
	var indices []indexRecord
	var indexErr error
	if cfg.MaxIndices > 0 {
		q := url.Values{"format": {"json"}, "bytes": {"b"}, "h": {"index,health,status,docs.count,store.size"}}
		indexErr = c.get(ctx, "/_cat/indices/"+url.PathEscape(cfg.IndexPattern)+"?"+q.Encode(), &indices)
		if isStatus(indexErr, 404) { // the pattern matches nothing
			indexErr = nil
		}
	}

	now := time.Now()
	prev := loadRates(t.State)
	cur := rates{At: now, Nodes: map[string][2]float64{}}
	elapsed := now.Sub(prev.At).Seconds()

	objects := []plugin.Object{{
		Key: "cluster", Name: nameOr(health.ClusterName, "cluster"), Metrics: plugin.NaNs(metricCount),
		Status: healthStatus(health.Status), Output: clusterOutput(health),
		Labels: map[string]string{"kind": "cluster", "status": health.Status},
	}}
	cm := objects[0].Metrics
	cm[mHealth] = healthCode(health.Status)
	cm[mNodes], cm[mDataNodes] = float64(health.Nodes), float64(health.DataNodes)
	cm[mActiveShards], cm[mUnassigned] = float64(health.ActiveShards), float64(health.Unassigned)
	cm[mRelocating], cm[mInitializing] = float64(health.Relocating), float64(health.Initializing)
	cm[mPendingTasks] = float64(health.PendingTasks)

	for _, id := range sortedKeys(stats.Nodes) {
		n := stats.Nodes[id]
		o := plugin.Object{Key: "node:" + id, Name: nameOr(n.Name, id), Metrics: plugin.NaNs(metricCount),
			Labels: map[string]string{"kind": "node", "host": n.Host, "ip": n.IP, "roles": strings.Join(n.Roles, ",")}}
		if n.JVM.Mem.HeapUsedPercent != nil {
			o.Metrics[mHeap] = *n.JVM.Mem.HeapUsedPercent
		}
		switch {
		case n.OS.CPU.Percent != nil:
			o.Metrics[mCPU] = *n.OS.CPU.Percent
		case n.Process.CPU.Percent != nil:
			o.Metrics[mCPU] = *n.Process.CPU.Percent
		}
		if n.FS.Total.Total > 0 {
			o.Metrics[mDisk] = (n.FS.Total.Total - n.FS.Total.Available) / n.FS.Total.Total * 100
		}
		gc, hasGC := 0.0, false
		if old, ok := n.JVM.GC.Collectors["old"]; ok {
			gc, hasGC = old.TimeMillis, true
		}
		rej := n.ThreadPool["search"].Rejected + n.ThreadPool["write"].Rejected
		cur.Nodes[id] = [2]float64{gc, rej}
		if p, ok := prev.Nodes[id]; ok && elapsed > 0 {
			if hasGC && gc >= p[0] {
				o.Metrics[mGCOld] = (gc - p[0]) / elapsed
			}
			if rej >= p[1] {
				o.Metrics[mRejected] = (rej - p[1]) / elapsed
			}
		}
		o.Output = fmt.Sprintf("heap %s, cpu %s, disk %s", pct(o.Metrics[mHeap]), pct(o.Metrics[mCPU]), pct(o.Metrics[mDisk]))
		objects = append(objects, o)
	}
	saveRates(t.State, cur)

	shown, total := 0, 0
	if cfg.MaxIndices > 0 && indexErr == nil {
		var picked []indexRecord
		for _, ix := range indices {
			if ix.Index == "" || (!cfg.IncludeSystemIndices && strings.HasPrefix(ix.Index, ".")) {
				continue
			}
			picked = append(picked, ix)
		}
		total = len(picked)
		sort.SliceStable(picked, func(i, j int) bool {
			si, sj := num(picked[i].StoreSize), num(picked[j].StoreSize)
			if si != sj {
				return si > sj
			}
			return picked[i].Index < picked[j].Index
		})
		if len(picked) > cfg.MaxIndices {
			picked = picked[:cfg.MaxIndices]
		}
		slices.SortFunc(picked, func(a, b indexRecord) int { return strings.Compare(a.Index, b.Index) })
		for _, ix := range picked {
			o := plugin.Object{Key: "index:" + ix.Index, Name: ix.Index, Metrics: plugin.NaNs(metricCount),
				Status: healthStatus(ix.Health), Labels: map[string]string{"kind": "index", "status": ix.Status, "health": ix.Health}}
			if ix.Health != "" {
				o.Metrics[mHealth] = healthCode(ix.Health)
			}
			o.Metrics[mDocs], o.Metrics[mStore] = num(ix.DocsCount), num(ix.StoreSize)
			if ix.Status == "close" {
				o.Output = "closed"
			} else {
				o.Output = fmt.Sprintf("%s docs, %s", countText(o.Metrics[mDocs]), bytesText(o.Metrics[mStore]))
			}
			objects = append(objects, o)
		}
		shown = len(picked)
	}

	out := clusterOutput(health)
	switch {
	case indexErr != nil:
		out += "; indices not read: " + indexErr.Error()
	case cfg.MaxIndices > 0 && total > shown:
		out += fmt.Sprintf("; %d of %d indices reported (largest first, max_indices)", shown, total)
	}
	var inv plugin.Inventory
	if v := version(ctx, c); v != "" {
		inv.Device = &plugin.DeviceInfo{Vendor: "Elastic", Firmware: v}
	}
	return plugin.Batch{Status: healthStatus(health.Status), Output: out, Objects: objects}, inv, nil
}

// version is the cluster's version number; "" when it cannot be read (it is
// only descriptive, so a failure is not one for the run).
func version(ctx context.Context, c *client) string {
	var root struct {
		Version struct {
			Number string `json:"number"`
		} `json:"version"`
	}
	if err := c.get(ctx, "/?filter_path=version.number", &root); err != nil {
		return ""
	}
	return root.Version.Number
}

func clusterOutput(h healthRecord) string {
	return fmt.Sprintf("cluster %s is %s: %d nodes, %d active shards, %d unassigned",
		nameOr(h.ClusterName, "(unnamed)"), nameOr(h.Status, "unknown"), h.Nodes, h.ActiveShards, h.Unassigned)
}

// failed turns an error into a failed collection: no answer, or no master,
// is CRITICAL; a rejected login, a refused address or a certificate problem
// is UNKNOWN (the configuration, not the cluster).
func failed(base string, err error) plugin.Batch {
	var pe *plugin.PolicyError
	var he *httpError
	var ce *tls.CertificateVerificationError
	var ne net.Error
	var op *net.OpError
	switch {
	case errors.As(err, &pe):
		return plugin.Batch{Status: plugin.Unknown, Output: pe.Error()}
	case errors.As(err, &he) && (he.Code == 401):
		return plugin.Batch{Status: plugin.Unknown, Output: "the cluster rejected the login: check the credential"}
	case errors.As(err, &he) && he.Code == 403:
		return plugin.Batch{Status: plugin.Unknown, Output: "the account may not read the cluster: grant it the monitor cluster privilege (" + he.Error() + ")"}
	case errors.As(err, &he):
		return plugin.Batch{Status: plugin.Critical, Output: fmt.Sprintf("cluster %s answered %v", base, he)}
	case errors.As(err, &ce):
		return plugin.Batch{Status: plugin.Unknown, Output: "certificate verification failed: " + ce.Err.Error() + " (set verify_certificate to false for a private CA)"}
	case errors.As(err, &ne) && ne.Timeout(), errors.As(err, &op), errors.Is(err, context.DeadlineExceeded):
		return plugin.Batch{Status: plugin.Critical, Output: fmt.Sprintf("cluster %s does not answer: %v", base, err)}
	}
	return plugin.Batch{Status: plugin.Unknown, Output: fmt.Sprintf("cluster %s: %v", base, err)}
}

func isStatus(err error, code int) bool {
	var he *httpError
	return errors.As(err, &he) && he.Code == code
}

func healthStatus(s string) plugin.Status {
	switch s {
	case "green":
		return plugin.OK
	case "yellow":
		return plugin.Warning
	case "red":
		return plugin.Critical
	}
	return plugin.Unknown
}

func healthCode(s string) float64 {
	switch s {
	case "green":
		return 0
	case "yellow":
		return 1
	case "red":
		return 2
	}
	return math.NaN()
}

// rates is the counters of the last run, kept to turn them into rates.
type rates struct {
	At    time.Time             `json:"at"`
	Nodes map[string][2]float64 `json:"nodes"` // node id -> old GC ms, rejected requests
}

func loadRates(s plugin.StateStore) rates {
	var r rates
	if s == nil {
		return r
	}
	if b, ok := s.Get("rates"); ok {
		_ = json.Unmarshal(b, &r)
	}
	return r
}

func saveRates(s plugin.StateStore, r rates) {
	if s == nil {
		return
	}
	if b, err := json.Marshal(r); err == nil {
		_ = s.Set("rates", b) // too many nodes for the state store: no rates, not a failure
	}
}

// num reads a _cat number; NaN when it is empty (a closed index).
func num(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

func pct(v float64) string {
	if math.IsNaN(v) {
		return "n/a"
	}
	return fmt.Sprintf("%.0f %%", v)
}

func countText(n float64) string {
	switch {
	case math.IsNaN(n):
		return "n/a"
	case n >= 1e9:
		return fmt.Sprintf("%.1f G", n/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f M", n/1e6)
	}
	return fmt.Sprintf("%.0f", n)
}

func bytesText(n float64) string {
	switch {
	case math.IsNaN(n):
		return "n/a"
	case n >= 1e12:
		return fmt.Sprintf("%.1f TB", n/1e12)
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", n/1e9)
	}
	return fmt.Sprintf("%.0f MB", n/1e6)
}

func nameOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return strings.TrimSpace(s)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
