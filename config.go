package main

import (
	"flag"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Config struct {
	ListenAddr   string
	WowzaWSURL   string
	AllowedHosts string // Comma-separated list, supports wildcards like *.wowza.com

	WsTimeout time.Duration
	InsecureTLS  bool
	Verbose      bool
	LogFormat    string
}

func NewConfig() *Config {
	c := &Config{
		ListenAddr:   env("LISTEN_ADDR", ":8080"),
		WowzaWSURL:   env("WOWZA_WEBSOCKET_URL", ""),
		AllowedHosts: env("ALLOWED_HOSTS", ""),
		WsTimeout:    envDuration("WS_TIMEOUT", 30*time.Second),
		InsecureTLS:  envBool("INSECURE_TLS", false),
		Verbose:      envBool("VERBOSE", false),
		LogFormat:    env("LOG_FORMAT", "auto"),
	}

	flag.StringVar(&c.ListenAddr, "listen", c.ListenAddr, "HTTP listen address (env: LISTEN_ADDR)")
	flag.StringVar(&c.WowzaWSURL, "websocket", c.WowzaWSURL, "Wowza WebSocket URL for static mode (env: WOWZA_WEBSOCKET_URL)")
	flag.StringVar(&c.AllowedHosts, "allowed-hosts", c.AllowedHosts, "Allowed Wowza hosts, comma-separated, supports wildcards (env: ALLOWED_HOSTS)")
	flag.DurationVar(&c.WsTimeout, "ws-timeout", c.WsTimeout, "WebSocket signaling timeout (env: WS_TIMEOUT)")
	flag.BoolVar(&c.InsecureTLS, "insecure-tls", c.InsecureTLS, "Skip TLS verification (env: INSECURE_TLS)")
	flag.BoolVar(&c.Verbose, "verbose", c.Verbose, "Enable debug logging (env: VERBOSE)")
	flag.StringVar(&c.LogFormat, "log-format", c.LogFormat, "Log format: auto, text, json (env: LOG_FORMAT)")

	return c
}

// IsHostAllowed checks if a host is in the allowed list.
// Empty string or "*" means all hosts allowed.
func (c *Config) IsHostAllowed(host string) bool {
	allowed := strings.TrimSpace(c.AllowedHosts)
	if allowed == "" || allowed == "*" {
		return true
	}
	host = strings.ToLower(strings.TrimSpace(host))
	for _, pattern := range strings.Split(allowed, ",") {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" || pattern == "*" {
			return true
		}
		if matchHost(pattern, host) {
			return true
		}
	}
	return false
}

func matchHost(pattern, host string) bool {
	if pattern == host {
		return true
	}
	// Wildcard match: *.example.com matches foo.example.com and bar.foo.example.com
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // .example.com
		return strings.HasSuffix(host, suffix)
	}
	return false
}

func (c *Config) Logger() *slog.Logger {
	level := slog.LevelInfo
	if c.Verbose {
		level = slog.LevelDebug
	}

	format := strings.ToLower(c.LogFormat)
	if format == "auto" {
		if os.Getenv("TERM") != "" {
			format = "text"
		} else {
			format = "json"
		}
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		v = strings.ToLower(v)
		return v == "true" || v == "1" || v == "yes"
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

