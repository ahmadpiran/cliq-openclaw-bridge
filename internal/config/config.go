package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Server   ServerConfig
	Zoho     ZohoConfig
	OpenClaw OpenClawConfig
	Store    StoreConfig
	Worker   WorkerConfig
}

type ServerConfig struct {
	Port            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	NotifySecret    string
}

type ZohoConfig struct {
	WebhookSecret       string
	ClientID            string
	ClientSecret        string
	TokenKey            string
	RedirectURI         string
	CliqReplyWebhookURL string
}

type OpenClawConfig struct {
	BaseURL        string
	APIKey         string
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	HTTPTimeout    time.Duration
	AgentsDir      string
	WorkspaceDir   string
	ReplyTimeout   time.Duration
}

type StoreConfig struct {
	DBPath string
}

type WorkerConfig struct {
	Workers    int
	QueueDepth int
	JobTimeout time.Duration
}

func Load() (*Config, error) {
	var errs []string

	cfg := &Config{}

	cfg.Server = ServerConfig{
		Port:            envOr("PORT", "8080"),
		ReadTimeout:     envDuration("SERVER_READ_TIMEOUT", 10*time.Second),
		WriteTimeout:    envDuration("SERVER_WRITE_TIMEOUT", 10*time.Second),
		IdleTimeout:     envDuration("SERVER_IDLE_TIMEOUT", 120*time.Second),
		ShutdownTimeout: envDuration("SERVER_SHUTDOWN_TIMEOUT", 30*time.Second),
		NotifySecret:    envRequired("BRIDGE_NOTIFY_SECRET", &errs),
	}

	cfg.Zoho = ZohoConfig{
		WebhookSecret:       envRequired("ZOHO_WEBHOOK_SECRET", &errs),
		ClientID:            envRequired("ZOHO_CLIENT_ID", &errs),
		ClientSecret:        envRequired("ZOHO_CLIENT_SECRET", &errs),
		TokenKey:            envOr("ZOHO_TOKEN_KEY", "zoho:default"),
		RedirectURI:         envOr("ZOHO_REDIRECT_URI", ""),
		CliqReplyWebhookURL: envOr("ZOHO_REPLY_WEBHOOK_URL", ""),
	}

	cfg.OpenClaw = OpenClawConfig{
		BaseURL:        envRequired("OPENCLAW_BASE_URL", &errs),
		APIKey:         envRequired("OPENCLAW_API_KEY", &errs),
		MaxRetries:     envInt("OPENCLAW_MAX_RETRIES", 4),
		InitialBackoff: envDuration("OPENCLAW_INITIAL_BACKOFF", 500*time.Millisecond),
		MaxBackoff:     envDuration("OPENCLAW_MAX_BACKOFF", 30*time.Second),
		HTTPTimeout:    envDuration("OPENCLAW_HTTP_TIMEOUT", 15*time.Second),
		AgentsDir:      envOr("OPENCLAW_AGENTS_DIR", ""),
		WorkspaceDir:   envOr("OPENCLAW_WORKSPACE_DIR", ""),
		ReplyTimeout:   envDuration("OPENCLAW_REPLY_TIMEOUT", 120*time.Second),
	}

	cfg.Store = StoreConfig{
		DBPath: envOr("BOLT_DB_PATH", "tokens.db"),
	}

	cfg.Worker = WorkerConfig{
		Workers:    envInt("WORKER_COUNT", 8),
		QueueDepth: envInt("WORKER_QUEUE_DEPTH", 512),
		JobTimeout: envDuration("WORKER_JOB_TIMEOUT", 90*time.Second),
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("configuration errors:\n  - %s", strings.Join(errs, "\n  - "))
	}

	return cfg, nil
}

func (c *ServerConfig) Addr() string {
	return ":" + c.Port
}

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

func envRequired(key string, errs *[]string) string {
	v := os.Getenv(key)
	if v == "" {
		*errs = append(*errs, fmt.Sprintf("%s is required but not set", key))
	}
	return v
}

func envOr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

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
