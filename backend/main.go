// Command labyrinth is the Labyrinth 26 backend: a Go API server over plain
// Postgres, with Google sign-in and in-memory game state.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"labyrinth/internal/auth"
	"labyrinth/internal/platform/config"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/server"
)

// main runs the backend, or one of its maintenance subcommands.
//
// Usage:
//
//	labyrinth          run migrations, then serve
//	labyrinth migrate  run migrations and exit
//	labyrinth seed     run migrations, load the 2026 content, and exit
//
// Serving, migrate and seed all take the single-instance lock first, so they
// refuse to run while a backend is serving from the same database.
//
//	labyrinth keygen   print a new JWT_PRIVATE_KEY / JWT_PUBLIC_KEY pair
//	labyrinth pubkey   print JWT_PUBLIC_KEY for the configured JWT_PRIVATE_KEY
func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(log) // for packages with no logger of their own (httpx)
	if err := run(log, os.Args[1:]); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// run executes the subcommand in args, or serves until SIGINT or SIGTERM.
//
// It returns an error for bad configuration, an unreachable database, a failed
// migration, or another instance already running.
func run(log *slog.Logger, args []string) error {
	if len(args) > 0 && args[0] == "keygen" {
		priv, pub, err := config.GenerateKeyPair()
		if err != nil {
			return err
		}
		qr, err := config.GenerateQRSecret()
		if err != nil {
			return err
		}
		fmt.Printf("# backend/.env\nJWT_PRIVATE_KEY=\"%s\"\nQR_SECRET=\"%s\"\n\n# frontend/.env\nJWT_PUBLIC_KEY=\"%s\"\n", priv, qr, pub)
		return nil
	}

	// .env is optional; real deployments set environment variables directly.
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load .env: %w", err)
	}

	if len(args) > 0 && args[0] == "pubkey" {
		key, err := config.ParsePrivateKey(os.Getenv("JWT_PRIVATE_KEY"))
		if err != nil {
			return fmt.Errorf("JWT_PRIVATE_KEY: %w", err)
		}
		pub, err := config.PublicKeyBase64(key)
		if err != nil {
			return err
		}
		fmt.Printf("JWT_PUBLIC_KEY=\"%s\"\n", pub)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer db.Close()

	set := cfg.Settings

	// Game state lives in this process's memory; refuse to run alongside
	// another instance that would hold its own diverging copy. Taken before
	// migrating or seeding, so neither can touch a database a running backend
	// is serving from.
	lock, err := db.AcquireInstanceLock(ctx, set.Storage.InstanceLockCheck.Duration)
	if err != nil {
		return err
	}
	defer lock.Release()

	if err := db.Migrate(ctx, log); err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "migrate" {
		return nil
	}
	if len(args) > 0 && args[0] == "seed" {
		// Content is read at startup, which is why seeding needs the lock.
		if err := db.Seed2026(ctx); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
		log.Info("seeded 2026 content")
		return nil
	}

	app, err := server.Build(ctx, cfg, db, auth.DefaultGoogleEndpoints, log)
	if err != nil {
		return err
	}

	go cleanupLoginCodes(ctx, app.LoginCodes, set.Auth.LoginCodeSweep.Duration, log)
	go app.Leaderboard.Run(ctx) // broadcasts the top teams whenever it changes

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           app.Handler,
		ReadHeaderTimeout: set.Server.ReadHeaderTimeout.Duration,
		ReadTimeout:       set.Server.ReadTimeout.Duration,
		IdleTimeout:       set.Server.IdleTimeout.Duration,
	}
	// Event streams never end on their own; close them so Shutdown can finish.
	srv.RegisterOnShutdown(app.Hub.Close)
	// No boss attacks once shutting down: they'd publish to closed streams.
	srv.RegisterOnShutdown(app.TeamState.Close)

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		errCh <- srv.ListenAndServe()
	}()

	var lockErr error
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	case <-lock.Lost():
		// Another instance could now start; stop serving this copy of the state.
		lockErr = postgres.ErrInstanceLockLost
		log.Error("instance lock lost; shutting down")
		stop() // cancels ctx, ending background work like the login-code sweep
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), set.Server.ShutdownTimeout.Duration)
	defer cancel()
	return errors.Join(lockErr, srv.Shutdown(shutdownCtx))
}

// cleanupLoginCodes deletes expired, unredeemed login codes every interval
// (config.toml auth.login_code_sweep) until ctx is done.
func cleanupLoginCodes(ctx context.Context, codes *auth.LoginCodes, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := codes.DeleteExpired(ctx); err != nil && ctx.Err() == nil {
				log.Warn("delete expired login codes", "error", err)
			}
		}
	}
}
