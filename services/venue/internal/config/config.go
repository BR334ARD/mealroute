// Package config loads venue runtime configuration from the environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	Addr                string
	PlatformURL         string
	VenueAPIKey         string
	ReadHeaderTimeout   time.Duration
	WriteTimeout        time.Duration
	IdleTimeout         time.Duration
	ShutdownTimeout     time.Duration
	SyncInterval        time.Duration
	PlatformHTTPTimeout time.Duration
}

func Load() (Config, error) {
	addr, err := required("VENUE_ADDR")
	if err != nil {
		return Config{}, err
	}
	platformURL, err := required("PLATFORM_URL")
	if err != nil {
		return Config{}, err
	}
	parsedPlatformURL, err := url.Parse(platformURL)
	if err != nil || (parsedPlatformURL.Scheme != "http" && parsedPlatformURL.Scheme != "https") || parsedPlatformURL.Host == "" {
		return Config{}, fmt.Errorf("PLATFORM_URL must be an absolute HTTP(S) URL")
	}
	venueAPIKey, err := required("VENUE_API_KEY")
	if err != nil {
		return Config{}, err
	}
	if len(venueAPIKey) < 16 {
		return Config{}, fmt.Errorf("VENUE_API_KEY must contain at least 16 bytes")
	}
	readHeaderTimeout, err := requiredDuration("VENUE_READ_HEADER_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	writeTimeout, err := requiredDuration("VENUE_WRITE_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	idleTimeout, err := requiredDuration("VENUE_IDLE_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := requiredDuration("VENUE_SHUTDOWN_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	syncInterval, err := requiredDuration("VENUE_SYNC_INTERVAL")
	if err != nil {
		return Config{}, err
	}
	platformHTTPTimeout, err := requiredDuration("VENUE_PLATFORM_HTTP_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	return Config{
		Addr:                addr,
		PlatformURL:         platformURL,
		VenueAPIKey:         venueAPIKey,
		ReadHeaderTimeout:   readHeaderTimeout,
		WriteTimeout:        writeTimeout,
		IdleTimeout:         idleTimeout,
		ShutdownTimeout:     shutdownTimeout,
		SyncInterval:        syncInterval,
		PlatformHTTPTimeout: platformHTTPTimeout,
	}, nil
}

func required(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func requiredDuration(name string) (time.Duration, error) {
	value, err := required(name)
	if err != nil {
		return 0, err
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return duration, nil
}
