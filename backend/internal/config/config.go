// Package config loads and validates all runtime configuration from the environment.
//
// Configuration is loaded exactly once, at process start, and validated before any
// dependency is opened. A process with invalid configuration must fail to boot rather
// than fail later under load: a missing JWT key discovered on the first sign-in is an
// outage, while the same problem discovered at boot is a failed deploy that never
// receives traffic.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Environment names the deployment context. It gates defaults that must never be
// permissive in production (debug endpoints, relaxed CORS, insecure cookies).
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvStaging     Environment = "staging"
	EnvProduction  Environment = "production"
	EnvTest        Environment = "test"
)

func (e Environment) IsProduction() bool { return e == EnvProduction }
func (e Environment) IsDevelopment() bool {
	return e == EnvDevelopment || e == EnvTest
}

// Config is the complete typed configuration for every MyJobFyt binary. Each binary
// uses the subset it needs; sharing one struct keeps a single source of truth for
// variable names across api, worker, scheduler and migrate.
type Config struct {
	Env         Environment `env:"APP_ENV" envDefault:"development"`
	ServiceName string      `env:"APP_SERVICE_NAME" envDefault:"myjobfyt"`
	Version     string      `env:"APP_VERSION" envDefault:"dev"`

	HTTP      HTTPConfig
	Log       LogConfig
	Postgres  PostgresConfig
	Redis     RedisConfig
	NATS      NATSConfig
	Storage   StorageConfig
	Auth      AuthConfig
	Crypto    CryptoConfig
	SMTP      SMTPConfig
	Antivirus AntivirusConfig
	Extract   ExtractConfig
	AI        AIConfig
	Embed     EmbedConfig
	Telemetry TelemetryConfig
	Worker    WorkerConfig
}

type HTTPConfig struct {
	Addr            string        `env:"HTTP_ADDR" envDefault:":8080"`
	ReadTimeout     time.Duration `env:"HTTP_READ_TIMEOUT" envDefault:"15s"`
	WriteTimeout    time.Duration `env:"HTTP_WRITE_TIMEOUT" envDefault:"30s"`
	IdleTimeout     time.Duration `env:"HTTP_IDLE_TIMEOUT" envDefault:"120s"`
	ShutdownTimeout time.Duration `env:"HTTP_SHUTDOWN_TIMEOUT" envDefault:"30s"`
	// MaxBodyBytes caps non-upload request bodies. Upload endpoints enforce their own,
	// larger cap while streaming so an oversized file is rejected during transfer
	// rather than after it has been buffered.
	MaxBodyBytes int64    `env:"HTTP_MAX_BODY_BYTES" envDefault:"1048576"`
	CORSOrigins  []string `env:"HTTP_CORS_ORIGINS" envSeparator:"," envDefault:"http://localhost:3000"`
	TrustedProxy bool     `env:"HTTP_TRUST_PROXY_HEADERS" envDefault:"false"`
}

type LogConfig struct {
	Level  string `env:"LOG_LEVEL" envDefault:"info"`
	Format string `env:"LOG_FORMAT" envDefault:"json"` // json | text
	// AddSource is expensive; useful in development, off by default elsewhere.
	AddSource bool `env:"LOG_ADD_SOURCE" envDefault:"false"`
}

type PostgresConfig struct {
	DSN             string        `env:"POSTGRES_DSN,required"`
	MaxConns        int32         `env:"POSTGRES_MAX_CONNS" envDefault:"20"`
	MinConns        int32         `env:"POSTGRES_MIN_CONNS" envDefault:"2"`
	MaxConnLifetime time.Duration `env:"POSTGRES_MAX_CONN_LIFETIME" envDefault:"1h"`
	MaxConnIdleTime time.Duration `env:"POSTGRES_MAX_CONN_IDLE_TIME" envDefault:"30m"`
	ConnectTimeout  time.Duration `env:"POSTGRES_CONNECT_TIMEOUT" envDefault:"10s"`
	// StatementTimeout is applied per session. It bounds the damage a pathological
	// query can do to the pool.
	StatementTimeout time.Duration `env:"POSTGRES_STATEMENT_TIMEOUT" envDefault:"30s"`
}

type RedisConfig struct {
	Addr     string `env:"REDIS_ADDR" envDefault:"localhost:6379"`
	Password string `env:"REDIS_PASSWORD"`
	DB       int    `env:"REDIS_DB" envDefault:"0"`
	PoolSize int    `env:"REDIS_POOL_SIZE" envDefault:"20"`
}

type NATSConfig struct {
	URL           string        `env:"NATS_URL" envDefault:"nats://localhost:4222"`
	Name          string        `env:"NATS_CLIENT_NAME" envDefault:"myjobfyt"`
	MaxReconnects int           `env:"NATS_MAX_RECONNECTS" envDefault:"-1"`
	ReconnectWait time.Duration `env:"NATS_RECONNECT_WAIT" envDefault:"2s"`
	// StreamReplicas is 1 for single-node development and 3 for a production cluster.
	StreamReplicas int `env:"NATS_STREAM_REPLICAS" envDefault:"1"`
}

type StorageConfig struct {
	Endpoint  string `env:"STORAGE_ENDPOINT" envDefault:"localhost:9000"`
	AccessKey string `env:"STORAGE_ACCESS_KEY"`
	SecretKey string `env:"STORAGE_SECRET_KEY"`
	Bucket    string `env:"STORAGE_BUCKET" envDefault:"myjobfyt-cvs"`
	Region    string `env:"STORAGE_REGION" envDefault:"us-east-1"`
	UseSSL    bool   `env:"STORAGE_USE_SSL" envDefault:"false"`
	// PresignTTL bounds how long a CV download link stays valid. Short by design:
	// these links are bearer credentials for personal data.
	PresignTTL time.Duration `env:"STORAGE_PRESIGN_TTL" envDefault:"5m"`
	MaxCVBytes int64         `env:"STORAGE_MAX_CV_BYTES" envDefault:"10485760"`
}

type AuthConfig struct {
	// JWTPrivateKeyPEM and JWTPublicKeyPEM hold an RS256 keypair. RS256 rather than
	// HS256 so the frontend and browser extension can verify tokens via JWKS without
	// ever holding a signing secret.
	JWTPrivateKeyPEM string        `env:"AUTH_JWT_PRIVATE_KEY_PEM"`
	JWTPublicKeyPEM  string        `env:"AUTH_JWT_PUBLIC_KEY_PEM"`
	JWTKeyID         string        `env:"AUTH_JWT_KEY_ID" envDefault:"primary"`
	AccessTokenTTL   time.Duration `env:"AUTH_ACCESS_TOKEN_TTL" envDefault:"15m"`
	RefreshTokenTTL  time.Duration `env:"AUTH_REFRESH_TOKEN_TTL" envDefault:"720h"`
	Issuer           string        `env:"AUTH_ISSUER" envDefault:"myjobfyt"`
	Audience         string        `env:"AUTH_AUDIENCE" envDefault:"myjobfyt-api"`

	// Argon2id parameters. Defaults follow OWASP guidance (19 MiB, t=2, p=1 is the
	// documented minimum; we raise memory to 64 MiB because CV-platform credentials
	// protect a large amount of personal data).
	Argon2Memory      uint32 `env:"AUTH_ARGON2_MEMORY_KIB" envDefault:"65536"`
	Argon2Iterations  uint32 `env:"AUTH_ARGON2_ITERATIONS" envDefault:"3"`
	Argon2Parallelism uint8  `env:"AUTH_ARGON2_PARALLELISM" envDefault:"2"`
	Argon2SaltLength  uint32 `env:"AUTH_ARGON2_SALT_LEN" envDefault:"16"`
	Argon2KeyLength   uint32 `env:"AUTH_ARGON2_KEY_LEN" envDefault:"32"`

	// Lockout thresholds for credential stuffing.
	MaxFailedLogins  int           `env:"AUTH_MAX_FAILED_LOGINS" envDefault:"10"`
	LockoutWindow    time.Duration `env:"AUTH_LOCKOUT_WINDOW" envDefault:"15m"`
	LockoutDuration  time.Duration `env:"AUTH_LOCKOUT_DURATION" envDefault:"15m"`
	CookieDomain     string        `env:"AUTH_COOKIE_DOMAIN"`
	CookieSecure     bool          `env:"AUTH_COOKIE_SECURE" envDefault:"false"`
	VerificationTTL  time.Duration `env:"AUTH_VERIFICATION_TTL" envDefault:"24h"`
	DeletionGraceTTL time.Duration `env:"AUTH_DELETION_GRACE" envDefault:"168h"`
}

type SMTPConfig struct {
	Host     string `env:"SMTP_HOST" envDefault:"localhost"`
	Port     int    `env:"SMTP_PORT" envDefault:"1025"`
	Username string `env:"SMTP_USERNAME"`
	Password string `env:"SMTP_PASSWORD"`
	From     string `env:"SMTP_FROM" envDefault:"no-reply@myjobfyt.local"`
	FromName string `env:"SMTP_FROM_NAME" envDefault:"MyJobFyt"`
	// UseTLS requires STARTTLS. Off locally because Mailpit speaks plain SMTP;
	// production validation forces it on, since verification links and application
	// emails must not cross the network in the clear.
	UseTLS bool `env:"SMTP_USE_TLS" envDefault:"false"`
	// AppBaseURL is the origin used to build links in emails.
	AppBaseURL string        `env:"APP_BASE_URL" envDefault:"http://localhost:13000"`
	Timeout    time.Duration `env:"SMTP_TIMEOUT" envDefault:"15s"`
}

type AIConfig struct {
	// APIKey may be empty, in which case the SDK resolves credentials itself — an
	// environment variable, or an OAuth profile from `ant auth login`. An unset key
	// does not mean there are no credentials.
	APIKey string `env:"ANTHROPIC_API_KEY"`
	// Enabled gates the structure stage. Off by default so the platform runs end to
	// end with no provider configured and no spend; the worker says so at startup
	// rather than leaving it to be discovered from missing data.
	Enabled bool `env:"AI_ENABLED" envDefault:"false"`
	// BaseURL points at a gateway or a test double.
	BaseURL string `env:"AI_BASE_URL"`
	// MaxRetries bounds the SDK's transport-level retries. Low, because the AI layer
	// implements its own policy on top and two layers of retry multiply into a long
	// stall on a failing provider.
	MaxRetries int `env:"AI_MAX_RETRIES" envDefault:"1"`
	// ExtractionTokenBudget caps one CV extraction, independently of the monthly quota.
	ExtractionTokenBudget int64 `env:"AI_EXTRACTION_TOKEN_BUDGET" envDefault:"80000"`
}

type EmbedConfig struct {
	// BaseURL points at a self-hosted embedding service. Empty leaves matching on the
	// lexical fallback, which is a supported mode rather than a failure: the Claude API
	// has no embeddings endpoint, so this component is always self-hosted and always
	// allowed to be absent.
	BaseURL  string        `env:"EMBED_BASE_URL"`
	Timeout  time.Duration `env:"EMBED_TIMEOUT" envDefault:"30s"`
	MaxBatch int           `env:"EMBED_MAX_BATCH" envDefault:"32"`
	// BackfillInterval is how often the worker sweeps for versions left unembedded by
	// an outage. A sweep is one indexed query when there is nothing to do.
	BackfillInterval time.Duration `env:"EMBED_BACKFILL_INTERVAL" envDefault:"5m"`
	// BackfillBatch bounds one sweep so a large backlog drains over several sweeps
	// rather than starving live uploads.
	BackfillBatch int `env:"EMBED_BACKFILL_BATCH" envDefault:"50"`
}

type AntivirusConfig struct {
	// Addr is the clamd TCP address. Empty selects the development pass-through
	// scanner, which production validation refuses.
	Addr     string        `env:"ANTIVIRUS_ADDR"`
	Timeout  time.Duration `env:"ANTIVIRUS_TIMEOUT" envDefault:"60s"`
	MaxBytes int64         `env:"ANTIVIRUS_MAX_BYTES" envDefault:"26214400"`
}

type ExtractConfig struct {
	MaxPages int `env:"EXTRACT_MAX_PAGES" envDefault:"20"`
	MaxChars int `env:"EXTRACT_MAX_CHARS" envDefault:"200000"`
}

type CryptoConfig struct {
	// DataKeys is the column-encryption keyring, primary first, each entry in
	// "id:base64-32-bytes" form. Generate one with `make crypto-key`.
	//
	// Several entries allow rotation: new writes use the first key while values
	// written under an older key stay readable until a re-encryption job has run.
	DataKeys []string `env:"DATA_ENCRYPTION_KEYS" envSeparator:","`
}

type TelemetryConfig struct {
	OTLPEndpoint   string  `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	TraceSampleTat float64 `env:"OTEL_TRACES_SAMPLER_ARG" envDefault:"1.0"`
	MetricsAddr    string  `env:"METRICS_ADDR" envDefault:":9090"`
	Enabled        bool    `env:"TELEMETRY_ENABLED" envDefault:"true"`
}

type WorkerConfig struct {
	// Groups selects which consumer groups this worker process runs. Splitting groups
	// across deployments means an AI-provider outage stalls cv consumers while
	// applications keeps submitting.
	Groups      []string      `env:"WORKER_GROUPS" envSeparator:"," envDefault:"cv,jobs,matching,applications,outreach"`
	Concurrency int           `env:"WORKER_CONCURRENCY" envDefault:"8"`
	AckWait     time.Duration `env:"WORKER_DEFAULT_ACK_WAIT" envDefault:"2m"`
	MaxDeliver  int           `env:"WORKER_DEFAULT_MAX_DELIVER" envDefault:"5"`
	ShutdownTO  time.Duration `env:"WORKER_SHUTDOWN_TIMEOUT" envDefault:"45s"`
	// DLQPollInterval is how often the dead-letter depth gauge is refreshed. One
	// stream-info call per interval, so this is cheap.
	DLQPollInterval time.Duration `env:"WORKER_DLQ_POLL_INTERVAL" envDefault:"30s"`
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("parse environment: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate enforces the invariants that cannot be expressed as struct tags, and the
// stricter rules that apply only in production.
func (c *Config) Validate() error {
	var problems []string

	switch c.Env {
	case EnvDevelopment, EnvStaging, EnvProduction, EnvTest:
	default:
		problems = append(problems, fmt.Sprintf("APP_ENV %q must be one of development, staging, production, test", c.Env))
	}

	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf("LOG_LEVEL %q must be one of debug, info, warn, error", c.Log.Level))
	}

	if c.Postgres.MinConns > c.Postgres.MaxConns {
		problems = append(problems, "POSTGRES_MIN_CONNS must not exceed POSTGRES_MAX_CONNS")
	}
	if c.Postgres.MaxConns < 1 {
		problems = append(problems, "POSTGRES_MAX_CONNS must be at least 1")
	}
	if c.Worker.Concurrency < 1 {
		problems = append(problems, "WORKER_CONCURRENCY must be at least 1")
	}
	if c.Worker.MaxDeliver < 1 {
		problems = append(problems, "WORKER_DEFAULT_MAX_DELIVER must be at least 1")
	}
	if c.SMTP.Port < 1 || c.SMTP.Port > 65535 {
		problems = append(problems, "SMTP_PORT must be between 1 and 65535")
	}
	if c.AI.Enabled && c.AI.ExtractionTokenBudget < 1000 {
		problems = append(problems, "AI_EXTRACTION_TOKEN_BUDGET is implausibly small")
	}
	if c.Embed.MaxBatch < 1 {
		problems = append(problems, "EMBED_MAX_BATCH must be at least 1")
	}
	if c.Extract.MaxPages < 1 {
		problems = append(problems, "EXTRACT_MAX_PAGES must be at least 1")
	}
	if c.Extract.MaxChars < 1000 {
		problems = append(problems, "EXTRACT_MAX_CHARS is implausibly small")
	}
	if c.Storage.MaxCVBytes < 1024 {
		problems = append(problems, "STORAGE_MAX_CV_BYTES is implausibly small")
	}
	if c.Auth.AccessTokenTTL >= c.Auth.RefreshTokenTTL {
		problems = append(problems, "AUTH_ACCESS_TOKEN_TTL must be shorter than AUTH_REFRESH_TOKEN_TTL")
	}

	// Argon2id parameters below these floors would weaken password storage to the
	// point where a database disclosure becomes a credential disclosure.
	if c.Auth.Argon2Memory < 19456 {
		problems = append(problems, "AUTH_ARGON2_MEMORY_KIB must be at least 19456 (19 MiB, OWASP minimum)")
	}
	if c.Auth.Argon2Iterations < 2 {
		problems = append(problems, "AUTH_ARGON2_ITERATIONS must be at least 2")
	}
	if c.Auth.Argon2Parallelism < 1 {
		problems = append(problems, "AUTH_ARGON2_PARALLELISM must be at least 1")
	}
	if c.Auth.Argon2KeyLength < 32 {
		problems = append(problems, "AUTH_ARGON2_KEY_LEN must be at least 32")
	}

	if c.Env.IsProduction() {
		if c.Auth.JWTPrivateKeyPEM == "" || c.Auth.JWTPublicKeyPEM == "" {
			problems = append(problems, "AUTH_JWT_PRIVATE_KEY_PEM and AUTH_JWT_PUBLIC_KEY_PEM are required in production")
		}
		if !c.Auth.CookieSecure {
			problems = append(problems, "AUTH_COOKIE_SECURE must be true in production")
		}
		if c.Storage.AccessKey == "" || c.Storage.SecretKey == "" {
			problems = append(problems, "storage credentials are required in production")
		}
		if !c.SMTP.UseTLS {
			problems = append(problems, "SMTP_USE_TLS must be true in production")
		}
		if !strings.HasPrefix(c.SMTP.AppBaseURL, "https://") {
			problems = append(problems, "APP_BASE_URL must use https in production")
		}
		if !c.AI.Enabled {
			// Without the structure stage a CV never becomes matchable, so the product
			// does not work. Better to refuse to start than to run a platform that
			// silently produces empty profiles.
			problems = append(problems, "AI_ENABLED must be true in production")
		}
		if c.Antivirus.Addr == "" {
			// A pass-through scanner is indistinguishable from having none, and this
			// endpoint accepts arbitrary files from the internet.
			problems = append(problems, "ANTIVIRUS_ADDR is required in production")
		}
		if len(c.Crypto.DataKeys) == 0 {
			// Without a key, phone numbers and SMTP passwords would have to be stored
			// in plaintext. Refuse to start rather than quietly downgrade.
			problems = append(problems, "DATA_ENCRYPTION_KEYS is required in production")
		}
		for _, o := range c.HTTP.CORSOrigins {
			if o == "*" {
				problems = append(problems, "HTTP_CORS_ORIGINS must not contain '*' in production")
			}
			if strings.HasPrefix(o, "http://") {
				problems = append(problems, fmt.Sprintf("HTTP_CORS_ORIGINS entry %q must use https in production", o))
			}
		}
		if !c.Storage.UseSSL {
			problems = append(problems, "STORAGE_USE_SSL must be true in production")
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}
