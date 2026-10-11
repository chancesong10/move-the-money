// Command server runs the Move the Money HTTP API.
//
// Configuration (environment variables):
//
//	DATABASE_URL  Postgres connection string (default: the docker-compose database)
//	ADDR          listen address (default :8080)
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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chancesong10/move-the-money/internal/api"
	"github.com/chancesong10/move-the-money/internal/bank"
)

const defaultDatabaseURL = "postgres://money:money@localhost:5432/money"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = defaultDatabaseURL
		logger.Info("DATABASE_URL not set, using docker-compose default")
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return errors.Join(errors.New("cannot reach database (is docker compose up?)"), err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.New(bank.New(pool), logger),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("listening", "addr", addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Ctrl+C: stop accepting new requests, let in-flight transfers finish.
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
