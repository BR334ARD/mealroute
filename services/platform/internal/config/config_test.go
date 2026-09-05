package config_test

import (
	"testing"

	"mealroute/platform/internal/config"
)

func TestLoadRequiresCompleteEnvironment(t *testing.T) {
	t.Setenv("REDIS_URL", "")
	t.Setenv("PLATFORM_ADDR", ":8080")
	t.Setenv("DATABASE_URL", "postgres://local")
	t.Setenv("VENUE_ID", "00000000-0000-0000-0000-000000000001")
	t.Setenv("VENUE_API_KEY", "test-venue-key-123")
	t.Setenv("PLATFORM_READ_HEADER_TIMEOUT", "5s")
	t.Setenv("PLATFORM_WRITE_TIMEOUT", "15s")
	t.Setenv("PLATFORM_IDLE_TIMEOUT", "60s")
	t.Setenv("PLATFORM_SHUTDOWN_TIMEOUT", "10s")

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.Addr != ":8080" || loaded.VenueAPIKey != "test-venue-key-123" || loaded.ReadHeaderTimeout.String() != "5s" {
		t.Fatalf("unexpected config: %+v", loaded)
	}

	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("MENU_CACHE_TTL", "0s")
	t.Setenv("REDIS_TIMEOUT", "100ms")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected non-positive cache TTL to fail")
	}
	t.Setenv("MENU_CACHE_TTL", "60s")
	t.Setenv("REDIS_TIMEOUT", "invalid")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected invalid Redis timeout to fail")
	}
	t.Setenv("REDIS_TIMEOUT", "100ms")
	if _, err := config.Load(); err != nil {
		t.Fatalf("valid cache configuration: %v", err)
	}
	t.Setenv("VENUE_API_KEY", "")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected missing VENUE_API_KEY to fail")
	}
}
