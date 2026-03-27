package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/middleware"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/store"
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

func main() {
	// --- Logger ---
	// Structured JSON logging; swap to slog.NewTextHandler for local dev readability.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// --- Token Store ---
	tokenStore, err := store.NewTokenStore(dbPath())
	if err != nil {
		slog.Error("failed to open token store", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := tokenStore.Close(); err != nil {
			slog.Error("token store close error", "error", err)
		}
	}()

	slog.Info("token store opened", "path", dbPath())

	// --- Router ---
	r := chi.NewRouter()

	// Core middleware stack applied to every route.
	r.Use(chimiddleware.RequestID)             // Injects X-Request-ID into every request context.
	r.Use(chimiddleware.RealIP)                // Trusts X-Real-IP / X-Forwarded-For headers.
	r.Use(chimiddleware.Recoverer)             // Catches panics in handlers, returns 500, logs stack trace.
	r.Use(chimiddleware.Heartbeat("/healthz")) // Lightweight liveness probe; no auth, no logging.

	r.Route("/webhooks", func(r chi.Router) {
		r.Use(middleware.ZohoHMAC(zohoSecret()))
		r.Post("/zoho", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotImplemented)
		})
	})

	// --- HTTP Server ---
	srv := &http.Server{
		Addr:         listenAddr(),
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second, // Will be raised for streaming endpoints in Step 5.
		IdleTimeout:  120 * time.Second,
	}

	// --- Graceful Shutdown ---
	// Run the server in a goroutine so the main goroutine can block on the signal channel.
	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server starting", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		slog.Error("server failed to start", "error", err)
		os.Exit(1)
	case sig := <-quit:
		slog.Info("shutdown signal received", "signal", sig)
	}

	// Give in-flight requests up to 30 seconds to complete.
	// This window will matter once the worker pool is draining.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}

	slog.Info("server stopped cleanly")
}

// listenAddr returns the bind address from the PORT env var, defaulting to :8080.
func listenAddr() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}

// dbPath returns the BoltDB file path from env, defaulting to a local file.
func dbPath() string {
	if p := os.Getenv("BOLT_DB_PATH"); p != "" {
		return p
	}
	return "tokens.db"
}

// zohoSecret reads the HMAC shared secret from the environment.
// The service refuses to start with an empty secret.
func zohoSecret() string {
	s := os.Getenv("ZOHO_WEBHOOK_SECRET")
	if s == "" {
		slog.Error("ZOHO_WEBHOOK_SECRET env var is required")
		os.Exit(1)
	}
	return s
}
