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

	"mealroute/venue/internal/config"
	"mealroute/venue/internal/httpapi"
	platformintegration "mealroute/venue/internal/integration/platform"
	"mealroute/venue/internal/repository/memory"
	"mealroute/venue/internal/service"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("venue stopped: %v", err)
	}
}

func run() error {
	runtimeConfig, err := config.Load()
	if err != nil {
		return fmt.Errorf("load runtime configuration: %w", err)
	}
	venueRepository := memory.New()
	platformGateway := platformintegration.New(runtimeConfig.PlatformURL, runtimeConfig.VenueAPIKey, runtimeConfig.PlatformHTTPTimeout)
	application := service.New(venueRepository, platformGateway)
	runContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go application.Run(runContext, runtimeConfig.SyncInterval)
	router, err := httpapi.NewRouter(application)
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

	serverErrors := make(chan error, 1)
	log.Printf("venue service listening on %s", runtimeConfig.Addr)
	go func() { serverErrors <- server.ListenAndServe() }()
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve venue HTTP: %w", err)
		}
	case <-runContext.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), runtimeConfig.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("graceful venue shutdown: %w", err)
		}
	}
	return nil
}
