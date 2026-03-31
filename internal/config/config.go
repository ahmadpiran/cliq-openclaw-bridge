package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the single source of truth for all runtime configuration.
// It is populated once at startup via Load() and passed to every component
// that needs it. No component should call os.Getenv directly.
type Config struct {
	Server   ServerConfig
	Zoho     ZohoConfig
	OpenClaw OpenClawConfig
	Store    StoreConfig
	Worker   WorkerConfig
}

// ServerConfig holds HTTP server tuning knobs.
type ServerConfig struct {
	// Port is the TCP port the server listens on.
	Port string

	// ReadTimeout is the maximum duration for reading an entire request.
	ReadTimeout time.Duration

	// WriteTimeout is the maximum duration before timing out a response write.
	WriteTimeout time.Duration

	// IdleTimeout is the maximum duration to keep an idle connection alive.
	IdleTimeout time.Duration

	// ShutdownTimeout is the maximum time to wait for in-flight work to finish.
	ShutdownTimeout time.Duration
}

// ZohoConfig holds Zoho Cliq integration credentials.
type ZohoConfig struct {
	WebhookSecret       string
	ClientID            string
	ClientSecret        string
	TokenKey            string
	RedirectURI         string
	CliqReplyWebhookURL string
}

// OpenClawConfig holds outbound gateway coordinates and retry policy.
type OpenClawConfig struct {
	BaseURL        string
	APIKey         string
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	HTTPTimeout    time.Duration
	AgentsDir      string        // path to OpenClaw agents dir inside container
	ReplyTimeout   time.Duration // how long to wait for agent reply
}

// StoreConfig holds BoltDB settings.
type StoreConfig struct {
	// DBPath is the file path for the BoltDB database.
	DBPath string
}

// WorkerConfig holds worker pool sizing.
type WorkerConfig struct {
	// Workers is the number of goroutines processing jobs concurrently.
	Workers int

	// QueueDepth is the capacity of the buffered job channel.
	QueueDepth int

	// JobTimeout is the maximum time a single job may run.
	JobTimeout time.Duration
}

// Load reads all configuration from environment variables, applies defaults
// for optional fields, and validates that all required fields are present.
// Returns a descriptive error listing every missing or invalid value found
// so operators can fix all problems in one restart cycle.
func Load() (*Config, error) {
	var errs []string

	cfg := &Config{}

	// --- Server ---
	cfg.Server = ServerConfig{
		Port:            envOr("PORT", "8080"),
		ReadTimeout:     envDuration("SERVER_READ_TIMEOUT", 10*time.Second),
		WriteTimeout:    envDuration("SERVER_WRITE_TIMEOUT", 10*time.Second),
		IdleTimeout:     envDuration("SERVER_IDLE_TIMEOUT", 120*time.Second),
		ShutdownTimeout: envDuration("SERVER_SHUTDOWN_TIMEOUT", 30*time.Second),
	}

	// --- Zoho ---
	cfg.Zoho = ZohoConfig{
		WebhookSecret:       envRequired("ZOHO_WEBHOOK_SECRET", &errs),
		ClientID:            envRequired("ZOHO_CLIENT_ID", &errs),
		ClientSecret:        envRequired("ZOHO_CLIENT_SECRET", &errs),
		TokenKey:            envOr("ZOHO_TOKEN_KEY", "zoho:default"),
		RedirectURI:         envOr("ZOHO_REDIRECT_URI", ""),
		CliqReplyWebhookURL: envOr("ZOHO_REPLY_WEBHOOK_URL", ""),
	}

	// --- OpenClaw ---
	cfg.OpenClaw = OpenClawConfig{
		BaseURL:        envRequired("OPENCLAW_BASE_URL", &errs),
		APIKey:         envRequired("OPENCLAW_API_KEY", &errs),
		MaxRetries:     envInt("OPENCLAW_MAX_RETRIES", 4),
		InitialBackoff: envDuration("OPENCLAW_INITIAL_BACKOFF", 500*time.Millisecond),
		MaxBackoff:     envDuration("OPENCLAW_MAX_BACKOFF", 30*time.Second),
		HTTPTimeout:    envDuration("OPENCLAW_HTTP_TIMEOUT", 15*time.Second),
		AgentsDir:      envOr("OPENCLAW_AGENTS_DIR", ""),
		ReplyTimeout:   envDuration("OPENCLAW_REPLY_TIMEOUT", 60*time.Second),
	}

	// --- Store ---
	cfg.Store = StoreConfig{
		DBPath: envOr("BOLT_DB_PATH", "tokens.db"),
	}

	// --- Worker ---
	cfg.Worker = WorkerConfig{
		Workers:    envInt("WORKER_COUNT", 8),
		QueueDepth: envInt("WORKER_QUEUE_DEPTH", 512),
		JobTimeout: envDuration("WORKER_JOB_TIMEOUT", 30*time.Second),
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("configuration errors:\n  - %s", strings.Join(errs, "\n  - "))
	}

	return cfg, nil
}

// Addr returns the full TCP listen address derived from the port.
func (c *ServerConfig) Addr() string {
	return ":" + c.Port
}

// --- helpers ---

// envRequired reads a mandatory env var. Records a descriptive error if absent.
func envRequired(key string, errs *[]string) string {
	v := os.Getenv(key)
	if v == "" {
		*errs = append(*errs, fmt.Sprintf("%s is required but not set", key))
	}
	return v
}

// envOr reads an env var, falling back to defaultVal if it is unset or empty.
func envOr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// envInt reads an env var as an integer, returning defaultVal on parse failure
// or if the var is unset. Negative values are clamped to the default.
func envInt(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultVal
	}
	return n
}

// envDuration reads an env var as a time.Duration string (e.g. "5s", "1m30s").
// Falls back to defaultVal if unset or unparseable.
func envDuration(key string, defaultVal time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return defaultVal
	}
	return d
}

// Validate performs a post-load sanity check. Useful in tests that construct
// a Config manually rather than through Load().
func (c *Config) Validate() error {
	var errs []string
	if c.Zoho.WebhookSecret == "" {
		errs = append(errs, "Zoho.WebhookSecret is empty")
	}
	if c.OpenClaw.BaseURL == "" {
		errs = append(errs, "OpenClaw.BaseURL is empty")
	}
	if c.OpenClaw.APIKey == "" {
		errs = append(errs, "OpenClaw.APIKey is empty")
	}
	if c.Worker.Workers <= 0 {
		errs = append(errs, "Worker.Workers must be > 0")
	}
	if c.Worker.QueueDepth <= 0 {
		errs = append(errs, "Worker.QueueDepth must be > 0")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
