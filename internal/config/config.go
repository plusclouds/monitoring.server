// Package config loads the process configuration of `monitor serve`,
// `monitor migrate` and `monitor admin` (Config) and of `monitor probe`
// (ProbeConfig).
//
// Only process-level settings live here (ADR-0011). Tenant-facing data is in
// the database. deploy/config.example.yaml and deploy/probe.example.yaml are
// the annotated references; every value they show is the default returned by
// Default and DefaultProbe.
package config

import "time"

// Role names accepted by `monitor serve --roles` (ADR-0001).
const (
	RoleAPI         = "api"
	RoleRunner      = "runner"
	RoleEngine      = "engine"
	RoleNotifier    = "notifier"
	RoleIngest      = "ingest"
	RoleMaintenance = "maintenance"
)

// AllRoles is the default role set: everything in one process.
var AllRoles = []string{RoleAPI, RoleRunner, RoleEngine, RoleNotifier, RoleIngest, RoleMaintenance}

type Config struct {
	Node           Node           `yaml:"node"`
	Logging        Logging        `yaml:"logging"`
	Database       Database       `yaml:"database"`
	Metrics        Metrics        `yaml:"metrics"`
	Crypto         Crypto         `yaml:"crypto"`
	TLS            TLSPolicy      `yaml:"tls"`
	Outbound       Outbound       `yaml:"outbound"`
	API            API            `yaml:"api"`
	ProbeService   ProbeService   `yaml:"probe_service"`
	Runner         Runner         `yaml:"runner"`
	Engine         Engine         `yaml:"engine"`
	Notifier       Notifier       `yaml:"notifier"`
	Ingest         Ingest         `yaml:"ingest"`
	TenantConfig   TenantConfig   `yaml:"tenant_config"`
	Platform       Platform       `yaml:"platform"`
	Usage          Usage          `yaml:"usage"`
	SelfMonitoring SelfMonitoring `yaml:"self_monitoring"`
}

type Node struct {
	ID            string   `yaml:"id"`
	Roles         []string `yaml:"roles"`
	ShutdownGrace Duration `yaml:"shutdown_grace"`
}

type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// DSN is a connection string given inline (development only) or through a file.
type DSN struct {
	DSN      string `yaml:"dsn" secret:"true"`
	DSNFile  string `yaml:"dsn_file"`
	MaxConns int    `yaml:"max_conns,omitempty"`
}

type Database struct {
	Owner            DSN      `yaml:"owner"`
	App              DSN      `yaml:"app"`
	System           DSN      `yaml:"system"`
	ListenDSN        string   `yaml:"listen_dsn" secret:"true"`
	ListenDSNFile    string   `yaml:"listen_dsn_file"`
	MigrateOnStart   bool     `yaml:"migrate_on_start"`
	ConnectTimeout   Duration `yaml:"connect_timeout"`
	StatementTimeout Duration `yaml:"statement_timeout"`
}

type Metrics struct {
	Backend         string       `yaml:"backend"`
	Write           MetricsWrite `yaml:"write"`
	PartitionsAhead Duration     `yaml:"partitions_ahead"`
	Rollups         Rollups      `yaml:"rollups"`
	RetentionEvery  Duration     `yaml:"retention_every"`
	Query           MetricsQuery `yaml:"query"`
}

type MetricsWrite struct {
	BatchRows     int      `yaml:"batch_rows"`
	FlushInterval Duration `yaml:"flush_interval"`
	MaxBuffered   Duration `yaml:"max_buffered"`
}

type Rollups struct {
	FiveMinuteEvery Duration `yaml:"five_minute_every"`
	HourlyEvery     Duration `yaml:"hourly_every"`
}

type MetricsQuery struct {
	MaxPointsPerSeries int `yaml:"max_points_per_series"`
}

type Crypto struct {
	KeyProvider string        `yaml:"key_provider"`
	File        FileKeySource `yaml:"file"`
}

// FileKeySource lists key-encryption keys (ADR-0006). Every key decrypts;
// Active encrypts.
type FileKeySource struct {
	Active string   `yaml:"active"`
	Keys   []KEKRef `yaml:"keys"`
}

type KEKRef struct {
	ID   string `yaml:"id"`
	Path string `yaml:"path,omitempty"`
	Env  string `yaml:"env,omitempty"`
}

type TLSPolicy struct {
	MinVersion   string   `yaml:"min_version"`
	CipherSuites []string `yaml:"cipher_suites"`
}

type Outbound struct {
	DenyNetworks []string `yaml:"deny_networks"`
	HTTPProxy    string   `yaml:"http_proxy"`
	UserAgent    string   `yaml:"user_agent"`
}

// ListenerTLS is a server certificate. Disabled is only allowed on loopback.
type ListenerTLS struct {
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	ClientCAFile string `yaml:"client_ca_file,omitempty"`
	Disabled     bool   `yaml:"disabled,omitempty"`
}

type API struct {
	Listen                string      `yaml:"listen"`
	PublicURL             string      `yaml:"public_url"`
	TLS                   ListenerTLS `yaml:"tls"`
	TrustedProxies        []string    `yaml:"trusted_proxies"`
	MaxBody               ByteSize    `yaml:"max_body"`
	RequestTimeout        Duration    `yaml:"request_timeout"`
	IdempotencyTTL        Duration    `yaml:"idempotency_ttl"`
	PlatformRatePerMinute int         `yaml:"platform_rate_per_minute"`
	Docs                  bool        `yaml:"docs"`
	Events                APIEvents   `yaml:"events"`
}

type APIEvents struct {
	ReplayWindow          Duration `yaml:"replay_window"`
	TelemetryTickInterval Duration `yaml:"telemetry_tick_interval"`
}

type ProbeService struct {
	Enabled                   bool                  `yaml:"enabled"`
	Listen                    string                `yaml:"listen"`
	PublicURL                 string                `yaml:"public_url"`
	TLS                       ListenerTLS           `yaml:"tls"`
	EnrollmentTokenTTL        Duration              `yaml:"enrollment_token_ttl"`
	PresharedEnrollmentTokens []PresharedEnrollment `yaml:"preshared_enrollment_tokens"`
	CertificateLifetime       Duration              `yaml:"certificate_lifetime"`
	SilenceAfter              Duration              `yaml:"silence_after"`
	ClockOffsetWarning        Duration              `yaml:"clock_offset_warning"`
	MaxResultsPerPush         int                   `yaml:"max_results_per_push"`
}

// PresharedEnrollment is a reusable probe enrollment token bound to a tenant (F09).
type PresharedEnrollment struct {
	Name            string    `yaml:"name"`
	Token           string    `yaml:"token,omitempty" secret:"true"`
	TokenFile       string    `yaml:"token_file,omitempty"`
	Tenant          string    `yaml:"tenant"`
	Site            string    `yaml:"site,omitempty"`
	MaxProbes       int       `yaml:"max_probes,omitempty"`
	ExpiresAt       time.Time `yaml:"expires_at,omitempty"`
	AllowedNetworks []string  `yaml:"allowed_networks,omitempty"`
}

// Runner settings are shared by the core and by probes (F04).
type Runner struct {
	ShardLease           Duration        `yaml:"shard_lease,omitempty"`
	ShardRenew           Duration        `yaml:"shard_renew,omitempty"`
	ResyncInterval       Duration        `yaml:"resync_interval,omitempty"`
	Pools                RunnerPools     `yaml:"pools"`
	PerTargetConcurrency int             `yaml:"per_target_concurrency"`
	Timeouts             RunnerTimeouts  `yaml:"timeouts"`
	ResultBuffer         int             `yaml:"result_buffer"`
	ICMP                 ICMP            `yaml:"icmp"`
	SourceAddresses      SourceAddresses `yaml:"source_addresses"`
	DNSResolvers         []string        `yaml:"dns_resolvers"`
}

type RunnerPools struct {
	Fast int `yaml:"fast"`
	Slow int `yaml:"slow"`
}

type RunnerTimeouts struct {
	FractionOfInterval float64  `yaml:"fraction_of_interval"`
	CheckMax           Duration `yaml:"check_max"`
	CollectorMax       Duration `yaml:"collector_max"`
}

type ICMP struct {
	Mode string `yaml:"mode"`
}

type SourceAddresses struct {
	IPv4 string `yaml:"ipv4"`
	IPv6 string `yaml:"ipv6"`
}

type Engine struct {
	StateWriters int `yaml:"state_writers"`
}

type Notifier struct {
	Workers               int        `yaml:"workers"`
	ClaimBatch            int        `yaml:"claim_batch"`
	PollInterval          Duration   `yaml:"poll_interval"`
	DeliveryTimeout       Duration   `yaml:"delivery_timeout"`
	MaxDeliveryTimeout    Duration   `yaml:"max_delivery_timeout"`
	RetrySchedule         []Duration `yaml:"retry_schedule"`
	GiveUpAfter           Duration   `yaml:"give_up_after"`
	SecretRotationOverlap Duration   `yaml:"secret_rotation_overlap"`
	ResponseCapture       ByteSize   `yaml:"response_capture"`
	// DependencyGrace holds back an incident's notification so an upstream
	// failure found meanwhile can suppress it (F05).
	DependencyGrace Duration `yaml:"dependency_grace"`
	// IncidentURLTemplate builds data.links.incident, the incident in the
	// panel. Placeholders: {incident_id}, {account_id}, {tenant_id}.
	IncidentURLTemplate string `yaml:"incident_url_template"`
	// DeliveryRetention is how long finished deliveries and routed events
	// are kept for the delivery log and replay. 0 keeps them forever.
	DeliveryRetention Duration `yaml:"delivery_retention"`
}

type Ingest struct {
	HTTP IngestHTTP `yaml:"http"`
	MQTT IngestMQTT `yaml:"mqtt"`
}

type Rate struct {
	PerSecond float64 `yaml:"per_second"`
	Burst     int     `yaml:"burst"`
}

type IngestHTTP struct {
	Enabled        bool        `yaml:"enabled"`
	Listen         string      `yaml:"listen"`
	PublicURL      string      `yaml:"public_url"`
	TLS            ListenerTLS `yaml:"tls"`
	TrustedProxies []string    `yaml:"trusted_proxies"`
	MaxBody        ByteSize    `yaml:"max_body"`
	RateLimit      Rate        `yaml:"rate_limit"`
}

type IngestMQTT struct {
	Enabled             bool         `yaml:"enabled"`
	Listen              string       `yaml:"listen"`
	TLS                 ListenerTLS  `yaml:"tls"`
	Legacy              MQTTLegacy   `yaml:"legacy"`
	ProxyProtocol       bool         `yaml:"proxy_protocol"`
	TrustedProxies      []string     `yaml:"trusted_proxies"`
	MaxConnections      int          `yaml:"max_connections"`
	MaxPacketSize       ByteSize     `yaml:"max_packet_size"`
	PublishRate         Rate         `yaml:"publish_rate"`
	ConnectRatePerIP    Rate         `yaml:"connect_rate_per_ip"`
	PipelineBuffer      int          `yaml:"pipeline_buffer"`
	CredentialCacheTTL  Duration     `yaml:"credential_cache_ttl"`
	PasswordHash        Argon2Params `yaml:"password_hash"`
	MaxConcurrentAuth   int          `yaml:"max_concurrent_auth"`
	MaxDiscoveredFields int          `yaml:"max_discovered_fields"`
}

type MQTTLegacy struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

type Argon2Params struct {
	Memory      ByteSize `yaml:"memory"`
	Iterations  int      `yaml:"iterations"`
	Parallelism int      `yaml:"parallelism"`
}

type TenantConfig struct {
	Directory string `yaml:"directory"`
	Watch     bool   `yaml:"watch"`
}

type Platform struct {
	IdentitySource   string       `yaml:"identity_source"`
	JIT              JIT          `yaml:"jit"`
	TenantDefaults   TenantLimits `yaml:"tenant_defaults"`
	TenantPurgeGrace Duration     `yaml:"tenant_purge_grace"`
	AuditRetention   Duration     `yaml:"audit_retention"`
	UsageRetention   Duration     `yaml:"usage_retention"`
	UsageCloseDelay  Duration     `yaml:"usage_close_delay"`
}

// Usage configures metering (F13): the billing weight of each plugin type.
// The maintenance node records changes, effective from the next full hour.
type Usage struct {
	Weights       map[string]float64 `yaml:"weights"`
	DefaultWeight float64            `yaml:"default_weight"`
}

type JIT struct {
	Enabled      bool         `yaml:"enabled"`
	TenantLimits TenantLimits `yaml:"tenant_limits"`
}

// TenantLimits are the starting limits of a new tenant (F01).
type TenantLimits struct {
	MaxDevices              int      `yaml:"max_devices"`
	MaxChecks               int      `yaml:"max_checks"`
	MaxWebhooks             int      `yaml:"max_webhooks"`
	MaxAlertRoutes          int      `yaml:"max_alert_routes"`
	MinCheckIntervalSeconds int      `yaml:"min_check_interval_seconds"`
	APIRatePerMinute        int      `yaml:"api_rate_per_minute"`
	AllowedTargetNetworks   []string `yaml:"allowed_target_networks,omitempty"`
	MetricClasses           []string `yaml:"metric_classes,omitempty"`
}

type SelfMonitoring struct {
	Listen             string      `yaml:"listen"`
	TLS                ListenerTLS `yaml:"tls"`
	StatusToken        string      `yaml:"status_token" secret:"true"`
	StatusTokenFile    string      `yaml:"status_token_file"`
	AllowedNetworks    []string    `yaml:"allowed_networks"`
	StoreAsMetrics     bool        `yaml:"store_as_metrics"`
	ClockOffsetWarning Duration    `yaml:"clock_offset_warning"`
	Heartbeat          Heartbeat   `yaml:"heartbeat"`
}

type Heartbeat struct {
	Interval Duration          `yaml:"interval"`
	Targets  []HeartbeatTarget `yaml:"targets"`
}

type HeartbeatTarget struct {
	URL        string `yaml:"url"`
	TokenFile  string `yaml:"token_file,omitempty"`
	SecretFile string `yaml:"secret_file,omitempty"`
}

// ProbeConfig is the configuration of `monitor probe`.
type ProbeConfig struct {
	Probe   Probe       `yaml:"probe"`
	Logging Logging     `yaml:"logging"`
	HTTP    ProbeStatus `yaml:"http"`
	Runner  Runner      `yaml:"runner"`
	TLS     TLSPolicy   `yaml:"tls"`
}

type Probe struct {
	CoreURL           string      `yaml:"core_url"`
	Name              string      `yaml:"name"`
	Site              string      `yaml:"site"`
	EnrollToken       string      `yaml:"enroll_token" secret:"true"`
	EnrollTokenFile   string      `yaml:"enroll_token_file"`
	CoreCAFile        string      `yaml:"core_ca_file"`
	StateDir          string      `yaml:"state_dir"`
	Buffer            ProbeBuffer `yaml:"buffer"`
	HeartbeatInterval Duration    `yaml:"heartbeat_interval"`
	HTTPProxy         string      `yaml:"http_proxy"`
}

type ProbeBuffer struct {
	MaxSize ByteSize `yaml:"max_size"`
}

// ProbeStatus is the probe's local status server (F09).
type ProbeStatus struct {
	Enabled         bool        `yaml:"enabled"`
	Listen          string      `yaml:"listen"`
	TLS             ListenerTLS `yaml:"tls"`
	StatusToken     string      `yaml:"status_token" secret:"true"`
	StatusTokenFile string      `yaml:"status_token_file"`
	AllowedNetworks []string    `yaml:"allowed_networks"`
}
