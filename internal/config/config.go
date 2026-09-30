package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config contains process-level settings. Load validates it before Fx starts
// any listeners or workers.
type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	AWSRegion       string
	SQSEndpoint     string
	WagerQueueURL   string
	EventQueueURL   string
	OIDCIssuerURL   string
	OIDCAudience    string
	ShutdownTimeout time.Duration
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:      valueOrDefault("HTTP_ADDR", ":8080"),
		DatabaseURL:   strings.TrimSpace(os.Getenv("DATABASE_URL")),
		AWSRegion:     valueOrDefault("AWS_REGION", "us-east-1"),
		SQSEndpoint:   strings.TrimSpace(os.Getenv("SQS_ENDPOINT")),
		WagerQueueURL: strings.TrimSpace(os.Getenv("WAGER_QUEUE_URL")),
		EventQueueURL: strings.TrimSpace(os.Getenv("EVENT_QUEUE_URL")),
		OIDCIssuerURL: strings.TrimSpace(os.Getenv("OIDC_ISSUER_URL")),
		OIDCAudience:  strings.TrimSpace(os.Getenv("OIDC_AUDIENCE")),
	}
	var err error
	c.ShutdownTimeout, err = time.ParseDuration(valueOrDefault("SHUTDOWN_TIMEOUT", "30s"))
	if err != nil {
		return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) Validate() error {
	for name, value := range map[string]string{
		"HTTP_ADDR":       c.HTTPAddr,
		"DATABASE_URL":    c.DatabaseURL,
		"AWS_REGION":      c.AWSRegion,
		"WAGER_QUEUE_URL": c.WagerQueueURL,
		"EVENT_QUEUE_URL": c.EventQueueURL,
		"OIDC_ISSUER_URL": c.OIDCIssuerURL,
		"OIDC_AUDIENCE":   c.OIDCAudience,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s: %w", name, errors.New("required"))
		}
	}
	if c.ShutdownTimeout <= 0 {
		return errors.New("SHUTDOWN_TIMEOUT: must be positive")
	}
	return nil
}

func valueOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
