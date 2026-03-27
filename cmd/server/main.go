package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/config"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/gateway"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/handler"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/middleware"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/store"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/worker"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/zoho"
)

func main() {
	// --- Logger ---
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// --- Config ---
	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	// --- Token Store ---
	tokenStore, err := store.NewTokenStore(cfg.Store.DBPath)
	if err != nil {
		slog.Error("failed to open token store", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := tokenStore.Close(); err != nil {
			slog.Error("token store close error", "error", err)
		}
	}()
	slog.Info("token store opened", "path", cfg.Store.DBPath)

	// --- Token Refresher ---
	refresher := zoho.NewRefresher(zoho.RefresherConfig{
		ClientID:     cfg.Zoho.ClientID,
		ClientSecret: cfg.Zoho.ClientSecret,
		TokenKey:     cfg.Zoho.TokenKey,
	}, tokenStore)

	// --- Gateway Client ---
	gw := gateway.New(gateway.Config{
		BaseURL:        cfg.OpenClaw.BaseURL,
		APIKey:         cfg.OpenClaw.APIKey,
		MaxRetries:     cfg.OpenClaw.MaxRetries,
		InitialBackoff: cfg.OpenClaw.InitialBackoff,
		MaxBackoff:     cfg.OpenClaw.MaxBackoff,
		HTTPTimeout:    cfg.OpenClaw.HTTPTimeout,
	})

	_ = refresher

	// --- Worker Pool ---
	pool := worker.New(worker.Config{
		Workers:    cfg.Worker.Workers,
		QueueDepth: cfg.Worker.QueueDepth,
		JobTimeout: cfg.Worker.JobTimeout,
	}, func(ctx context.Context, job worker.Job) error {
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
		r.Use(middleware.ZohoHMAC(cfg.Zoho.WebhookSecret))
		r.Post("/zoho", webhookHandler.HandleZoho)
	})

	// --- HTTP Server ---
	srv := &http.Server{
		Addr:         cfg.Server.Addr(),
		Handler:      r,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http server shutdown error", "error", err)
	}

	pool.Shutdown(shutdownCtx)

	slog.Info("server stopped cleanly")
}
