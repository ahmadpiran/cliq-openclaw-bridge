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

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/gateway"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/handler"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/middleware"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/store"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/worker"
)

func main() {
	// --- Logger ---
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

	// --- Gateway Client ---
	gw := gateway.New(gateway.DefaultConfig(openclawBaseURL(), openclawAPIKey()))

	// --- Worker Pool (replace the stub handler) ---
	pool := worker.New(worker.DefaultConfig(), func(ctx context.Context, job worker.Job) error {
		return gw.Forward(ctx, gateway.ForwardRequest{
			Source:     "zoho_cliq",
			RequestID:  job.RequestID,
			Payload:    job.Payload,
			ReceivedAt: job.ReceivedAt,
		})
	})

	// --- Handlers ---
	webhookHandler := handler.NewWebhookHandler(pool)

	// --- Router ---
	r := chi.NewRouter()

	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(chimiddleware.Heartbeat("/healthz"))

	r.Route("/webhooks", func(r chi.Router) {
		r.Use(middleware.ZohoHMAC(zohoSecret()))
		r.Post("/zoho", webhookHandler.HandleZoho) // replaces the 501 stub
	})

	// --- HTTP Server ---
	srv := &http.Server{
		Addr:         listenAddr(),
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// --- Start Server ---
	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server starting", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// --- Signal Handling ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		slog.Error("server failed to start", "error", err)
		os.Exit(1)
	case sig := <-quit:
		slog.Info("shutdown signal received", "signal", sig)
	}

	// --- Graceful Shutdown ---
	// Both srv.Shutdown and pool.Shutdown share the same 30s budget.
	// HTTP stops accepting first, then the pool drains remaining jobs.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http server shutdown error", "error", err)
	}

	pool.Shutdown(shutdownCtx)

	slog.Info("server stopped cleanly")
}

func listenAddr() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}

func dbPath() string {
	if p := os.Getenv("BOLT_DB_PATH"); p != "" {
		return p
	}
	return "tokens.db"
}

func zohoSecret() string {
	s := os.Getenv("ZOHO_WEBHOOK_SECRET")
	if s == "" {
		slog.Error("ZOHO_WEBHOOK_SECRET env var is required")
		os.Exit(1)
	}
	return s
}

func openclawBaseURL() string {
	u := os.Getenv("OPENCLAW_BASE_URL")
	if u == "" {
		slog.Error("OPENCLAW_BASE_URL env var is required")
		os.Exit(1)
	}
	return u
}

func openclawAPIKey() string {
	k := os.Getenv("OPENCLAW_API_KEY")
	if k == "" {
		slog.Error("OPENCLAW_API_KEY env var is required")
		os.Exit(1)
	}
	return k
}
