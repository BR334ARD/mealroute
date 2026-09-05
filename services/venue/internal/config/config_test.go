package config_test

import (
	"testing"

	"mealroute/venue/internal/config"
)

func TestLoadRequiresCompleteEnvironment(t *testing.T) {
	t.Setenv("VENUE_ADDR", ":8081")
	t.Setenv("PLATFORM_URL", "http://platform:8080")
	t.Setenv("VENUE_API_KEY", "test-venue-key-123")
	t.Setenv("VENUE_READ_HEADER_TIMEOUT", "5s")
	t.Setenv("VENUE_WRITE_TIMEOUT", "15s")
	t.Setenv("VENUE_IDLE_TIMEOUT", "60s")
	t.Setenv("VENUE_SHUTDOWN_TIMEOUT", "10s")
	t.Setenv("VENUE_SYNC_INTERVAL", "2s")
	t.Setenv("VENUE_PLATFORM_HTTP_TIMEOUT", "5s")

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.Addr != ":8081" || loaded.VenueAPIKey != "test-venue-key-123" || loaded.SyncInterval.String() != "2s" {
		t.Fatalf("unexpected config: %+v", loaded)
	}

	t.Setenv("PLATFORM_URL", "")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected missing PLATFORM_URL to fail")
	}
}
