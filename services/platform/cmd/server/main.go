package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"mealroute/platform/internal/config"
	"mealroute/platform/internal/httpapi"
	"mealroute/platform/internal/repository/postgres"
	"mealroute/platform/internal/service"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("platform stopped: %v", err)
	}
}

func run() error {
	runtimeConfig, err := config.Load()
	if err != nil {
		return fmt.Errorf("load runtime configuration: %w", err)
	}
	ctx := context.Background()
	store, err := postgres.New(ctx, runtimeConfig.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}
	defer store.Close()
	if err := store.ConfigureVenueAPIKey(ctx, runtimeConfig.VenueID, runtimeConfig.VenueAPIKey); err != nil {
		return fmt.Errorf("configure venue API key: %w", err)
	}
	router, err := httpapi.NewRouter(service.New(store))
	if err != nil {
		return fmt.Errorf("build HTTP router: %w", err)
	}

	server := &http.Server{
		Addr:              runtimeConfig.Addr,
		Handler:           router,
		ReadHeaderTimeout: runtimeConfig.ReadHeaderTimeout,
		WriteTimeout:      runtimeConfig.WriteTimeout,
		IdleTimeout:       runtimeConfig.IdleTimeout,
	}

	runContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverErrors := make(chan error, 1)
	log.Printf("platform service listening on %s", runtimeConfig.Addr)
	go func() { serverErrors <- server.ListenAndServe() }()
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve platform HTTP: %w", err)
		}
	case <-runContext.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), runtimeConfig.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("graceful platform shutdown: %w", err)
		}
	}
	return nil
}
