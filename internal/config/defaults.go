package config

import "time"

func dur(d time.Duration) Duration { return Duration(d) }

// Default returns the defaults documented in deploy/config.example.yaml.
// Secret file paths are not defaulted: a missing value is a validation error,
// never a silent guess.
func Default() Config {
	return Config{
		Node: Node{
			Roles:         append([]string(nil), AllRoles...),
			ShutdownGrace: dur(30 * time.Second),
		},
		Logging: Logging{Level: "info", Format: "json"},
		Database: Database{
			App:              DSN{MaxConns: 20},
			System:           DSN{MaxConns: 40},
			MigrateOnStart:   true,
			ConnectTimeout:   dur(5 * time.Second),
			StatementTimeout: dur(30 * time.Second),
		},
		Metrics: Metrics{
			Backend: "auto",
			Write: MetricsWrite{
				BatchRows:     5000,
				FlushInterval: dur(time.Second),
				MaxBuffered:   dur(60 * time.Second),
			},
			PartitionsAhead: dur(72 * time.Hour),
			Rollups: Rollups{
				FiveMinuteEvery: dur(time.Minute),
				HourlyEvery:     dur(10 * time.Minute),
			},
			RetentionEvery: dur(time.Hour),
			Query:          MetricsQuery{MaxPointsPerSeries: 10000},
		},
		Crypto: Crypto{KeyProvider: "file"},
		TLS:    TLSPolicy{MinVersion: "1.2"},
		Outbound: Outbound{
			DenyNetworks: []string{"169.254.169.254/32", "fd00:ec2::254/128"},
			UserAgent:    "monitor/{version}",
		},
		API: API{
			Listen:                ":8443",
			MaxBody:               4 << 20,
			RequestTimeout:        dur(90 * time.Second),
			IdempotencyTTL:        dur(24 * time.Hour),
			PlatformRatePerMinute: 6000,
			Docs:                  true,
			Events: APIEvents{
				ReplayWindow:          dur(10 * time.Minute),
				TelemetryTickInterval: dur(5 * time.Second),
			},
		},
		ProbeService: ProbeService{
			Enabled:             true,
			Listen:              ":8444",
			EnrollmentTokenTTL:  dur(24 * time.Hour),
			CertificateLifetime: dur(720 * time.Hour),
			SilenceAfter:        dur(60 * time.Second),
			ClockOffsetWarning:  dur(2 * time.Second),
			MaxResultsPerPush:   1000,
		},
		Runner: Runner{
			ShardLease:           dur(15 * time.Second),
			ShardRenew:           dur(5 * time.Second),
			ResyncInterval:       dur(5 * time.Minute),
			Pools:                RunnerPools{Fast: 1500, Slow: 500},
			PerTargetConcurrency: 4,
			Timeouts: RunnerTimeouts{
				FractionOfInterval: 0.8,
				CheckMax:           dur(60 * time.Second),
				CollectorMax:       dur(5 * time.Minute),
			},
			ResultBuffer: 10000,
			ICMP:         ICMP{Mode: "auto"},
		},
		Engine: Engine{StateWriters: 16},
		Notifier: Notifier{
			Workers:            32,
			ClaimBatch:         100,
			PollInterval:       dur(time.Second),
			DependencyGrace:    dur(30 * time.Second),
			DeliveryRetention:  dur(30 * 24 * time.Hour),
			DeliveryTimeout:    dur(10 * time.Second),
			MaxDeliveryTimeout: dur(30 * time.Second),
			RetrySchedule: []Duration{
				dur(5 * time.Second), dur(30 * time.Second), dur(2 * time.Minute),
				dur(10 * time.Minute), dur(30 * time.Minute), dur(time.Hour),
			},
			GiveUpAfter:           dur(24 * time.Hour),
			SecretRotationOverlap: dur(24 * time.Hour),
			ResponseCapture:       1 << 10,
		},
		Ingest: Ingest{
			HTTP: IngestHTTP{
				Enabled:   true,
				Listen:    ":9443",
				MaxBody:   64 << 10,
				RateLimit: Rate{PerSecond: 1, Burst: 10},
			},
			MQTT: IngestMQTT{
				Enabled:             true,
				Listen:              ":8883",
				Legacy:              MQTTLegacy{Listen: ":1883"},
				MaxConnections:      20000,
				MaxPacketSize:       64 << 10,
				PublishRate:         Rate{PerSecond: 10, Burst: 50},
				ConnectRatePerIP:    Rate{PerSecond: 5, Burst: 20},
				PipelineBuffer:      50000,
				CredentialCacheTTL:  dur(60 * time.Second),
				PasswordHash:        Argon2Params{Memory: 19 << 20, Iterations: 2, Parallelism: 1},
				MaxConcurrentAuth:   16,
				MaxDiscoveredFields: 200,
			},
		},
		TenantConfig: TenantConfig{Directory: "/etc/monitor/tenants", Watch: true},
		Platform: Platform{
			IdentitySource: "plusclouds",
			JIT: JIT{
				Enabled: true,
				TenantLimits: TenantLimits{
					MinCheckIntervalSeconds: 60,
					APIRatePerMinute:        600,
				},
			},
			TenantDefaults: TenantLimits{
				MaxDevices:              1000,
				MaxChecks:               10000,
				MaxWebhooks:             20,
				MaxAlertRoutes:          50,
				MinCheckIntervalSeconds: 30,
				APIRatePerMinute:        1200,
				MetricClasses:           []string{"standard"},
			},
			TenantPurgeGrace: dur(720 * time.Hour),
			AuditRetention:   dur(8760 * time.Hour),
			UsageRetention:   dur(9504 * time.Hour),
			UsageCloseDelay:  dur(5 * time.Minute),
		},
		Usage: Usage{Weights: map[string]float64{"icmp": 1, "http": 2}, DefaultWeight: 1},
		SelfMonitoring: SelfMonitoring{
			Listen:             "127.0.0.1:9090",
			TLS:                ListenerTLS{Disabled: true},
			StoreAsMetrics:     true,
			ClockOffsetWarning: dur(2 * time.Second),
			Heartbeat:          Heartbeat{Interval: dur(60 * time.Second)},
		},
	}
}

// DefaultProbe returns the defaults documented in deploy/probe.example.yaml.
func DefaultProbe() ProbeConfig {
	return ProbeConfig{
		Probe: Probe{
			StateDir:          "/var/lib/monitor-probe",
			Buffer:            ProbeBuffer{MaxSize: 512 << 20},
			HeartbeatInterval: dur(10 * time.Second),
		},
		Logging: Logging{Level: "info", Format: "json"},
		HTTP: ProbeStatus{
			Enabled: true,
			Listen:  "127.0.0.1:9443",
		},
		Runner: Runner{
			Pools:                RunnerPools{Fast: 200, Slow: 50},
			PerTargetConcurrency: 4,
			Timeouts: RunnerTimeouts{
				FractionOfInterval: 0.8,
				CheckMax:           dur(60 * time.Second),
				CollectorMax:       dur(5 * time.Minute),
			},
			ResultBuffer: 2000,
			ICMP:         ICMP{Mode: "auto"},
		},
		TLS: TLSPolicy{MinVersion: "1.2"},
	}
}
