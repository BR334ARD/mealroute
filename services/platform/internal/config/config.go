// Package config loads platform runtime configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Config struct {
	RedisURL          string
	MenuCacheTTL      time.Duration
	RedisTimeout      time.Duration
	Addr              string
	DatabaseURL       string
	VenueID           uuid.UUID
	VenueAPIKey       string
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

func Load() (Config, error) {
	redisURL := strings.TrimSpace(os.Getenv("REDIS_URL"))
	var cacheTTL, redisTimeout time.Duration
	if redisURL != "" {
		var err error
		cacheTTL, err = requiredDuration("MENU_CACHE_TTL")
		if err != nil {
			return Config{}, err
		}
		redisTimeout, err = requiredDuration("REDIS_TIMEOUT")
		if err != nil {
			return Config{}, err
		}
	}
	addr, err := required("PLATFORM_ADDR")
	if err != nil {
		return Config{}, err
	}
	databaseURL, err := required("DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	venueIDValue, err := required("VENUE_ID")
	if err != nil {
		return Config{}, err
	}
	venueID, err := uuid.Parse(venueIDValue)
	if err != nil {
		return Config{}, fmt.Errorf("VENUE_ID must be a UUID: %w", err)
	}
	venueAPIKey, err := required("VENUE_API_KEY")
	if err != nil {
		return Config{}, err
	}
	if len(venueAPIKey) < 16 {
		return Config{}, fmt.Errorf("VENUE_API_KEY must contain at least 16 bytes")
	}
	readHeaderTimeout, err := requiredDuration("PLATFORM_READ_HEADER_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	writeTimeout, err := requiredDuration("PLATFORM_WRITE_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	idleTimeout, err := requiredDuration("PLATFORM_IDLE_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := requiredDuration("PLATFORM_SHUTDOWN_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	return Config{
		RedisURL:          redisURL,
		MenuCacheTTL:      cacheTTL,
		RedisTimeout:      redisTimeout,
		Addr:              addr,
		DatabaseURL:       databaseURL,
		VenueID:           venueID,
		VenueAPIKey:       venueAPIKey,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ShutdownTimeout:   shutdownTimeout,
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
