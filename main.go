package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Config is read from environment variables (see README.md).
type Config struct {
	ClientID     string
	ClientSecret string
	Username     string
	Password     string
	UserAgent    string

	ListenAddr         string
	BaseURL            string
	LinkBase           string
	CacheTTL           time.Duration
	MinRequestInterval time.Duration
}

// LoadConfig reads the configuration from the environment.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		ClientID:     os.Getenv("REDDIT_CLIENT_ID"),
		ClientSecret: os.Getenv("REDDIT_CLIENT_SECRET"),
		Username:     os.Getenv("REDDIT_USERNAME"),
		Password:     os.Getenv("REDDIT_PASSWORD"),
		UserAgent:    os.Getenv("REDDIT_USER_AGENT"),
		ListenAddr:   envOr("LISTEN_ADDR", ":8080"),
		BaseURL:      strings.TrimRight(os.Getenv("BASE_URL"), "/"),
		LinkBase:     strings.TrimRight(envOr("LINK_BASE", "https://www.reddit.com"), "/"),
	}

	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("REDDIT_CLIENT_ID and REDDIT_CLIENT_SECRET must be set")
	}
	if cfg.UserAgent == "" {
		// Reddit asks for "<platform>:<app ID>:<version> (by /u/<username>)".
		cfg.UserAgent = "linux:reddit-rss:v1.0"
		if cfg.Username != "" {
			cfg.UserAgent += " (by /u/" + cfg.Username + ")"
		}
	}

	var err error
	if cfg.CacheTTL, err = envDuration("CACHE_TTL", 15*time.Minute); err != nil {
		return nil, err
	}
	if cfg.MinRequestInterval, err = envDuration("MIN_REQUEST_INTERVAL", time.Second); err != nil {
		return nil, err
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %v", key, v, err)
	}
	return d, nil
}

func main() {
	log.Println("Starting reddit-rss...")

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	server := NewServer(cfg, NewRedditClient(cfg))
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("Listening on %s (cache TTL %s).\n", cfg.ListenAddr, cfg.CacheTTL)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Shutdown signal received, stopping...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown: %v\n", err)
	}
	log.Println("reddit-rss stopped.")
}
