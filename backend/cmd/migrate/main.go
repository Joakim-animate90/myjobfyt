// Command migrate applies database migrations.
//
// Usage:
//
//	migrate up                 apply all pending migrations
//	migrate down               roll back the most recent migration
//	migrate status             show applied and pending migrations
//	migrate version            print the current schema version
//	migrate up-to <version>    migrate up to a specific version
//	migrate validate           verify every migration parses and has a down step
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/telemetry"
	"github.com/Joakim-animate90/myjobfyt/backend/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func main() {
	if err := run(); err != nil {
		// Write to stderr as well as the structured log: a failed migration is read by
		// a human staring at a terminal or a CI log, not by a log aggregator.
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"up"}
	}
	command := args[0]

	// Handled before config.Load because validate needs no database at all: requiring
	// POSTGRES_DSN here would stop CI from checking migrations without a service
	// container, which is the main reason this subcommand exists.
	if command == "validate" {
		return validate()
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := telemetry.NewLogger(cfg.Log, "migrate", cfg.Version)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// database/sql rather than pgxpool: goose needs a *sql.DB, and a migration run is
	// a short-lived single-connection job where a pool buys nothing.
	connCfg, err := pgx.ParseConfig(cfg.Postgres.DSN)
	if err != nil {
		return fmt.Errorf("parse postgres dsn: %w", err)
	}
	db := stdlib.OpenDB(*connCfg)
	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := waitForDatabase(ctx, db.PingContext, 30*time.Second, log); err != nil {
		return err
	}

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(gooseLogger{log: log})
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}

	start := time.Now()
	switch command {
	case "up":
		err = goose.UpContext(ctx, db, ".")
	case "up-by-one":
		err = goose.UpByOneContext(ctx, db, ".")
	case "up-to":
		if len(args) < 2 {
			return errors.New("up-to requires a target version")
		}
		var version int64
		if _, serr := fmt.Sscanf(args[1], "%d", &version); serr != nil {
			return fmt.Errorf("invalid target version %q", args[1])
		}
		err = goose.UpToContext(ctx, db, ".", version)
	case "down":
		err = goose.DownContext(ctx, db, ".")
	case "status":
		err = goose.StatusContext(ctx, db, ".")
	case "version":
		err = goose.VersionContext(ctx, db, ".")
	case "reset":
		// Destructive, so it is refused anywhere that could plausibly hold real
		// candidate data. A dropped production schema is not recoverable from a flag.
		if cfg.Env.IsProduction() || cfg.Env == config.EnvStaging {
			return fmt.Errorf("reset is refused in %s", cfg.Env)
		}
		err = goose.DownToContext(ctx, db, ".", 0)
	default:
		return fmt.Errorf("unknown command %q", command)
	}

	if err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}

	if command == "up" || command == "up-by-one" || command == "up-to" {
		version, verr := goose.GetDBVersionContext(ctx, db)
		if verr == nil {
			log.Info("migrations applied",
				slog.Int64("schema_version", version),
				slog.Duration("duration", time.Since(start)),
			)
		}
	}
	return nil
}

// waitForDatabase retries the ping so `make dev` works without a manual pause between
// starting Postgres and running migrations.
func waitForDatabase(ctx context.Context, ping func(context.Context) error, timeout time.Duration, log *slog.Logger) error {
	deadline := time.Now().Add(timeout)
	attempt := 0
	for {
		attempt++
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := ping(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("database unreachable after %s (%d attempts): %w", timeout, attempt, err)
		}
		log.Warn("database not ready; retrying",
			slog.Int("attempt", attempt),
			slog.String("error", err.Error()),
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// validate parses every migration and checks it declares a down step.
//
// A migration without a down step cannot be rolled back, which turns a bad deploy into
// a restore-from-backup. Catching that in CI is far cheaper than discovering it during
// an incident.
func validate() error {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}

	var problems []string
	count := 0
	for _, e := range entries {
		if e.IsDir() || !hasSuffix(e.Name(), ".sql") {
			continue
		}
		count++
		body, rerr := migrations.FS.ReadFile(e.Name())
		if rerr != nil {
			problems = append(problems, fmt.Sprintf("%s: unreadable: %v", e.Name(), rerr))
			continue
		}
		content := string(body)
		if !contains(content, "-- +goose Up") {
			problems = append(problems, fmt.Sprintf("%s: missing '-- +goose Up'", e.Name()))
		}
		if !contains(content, "-- +goose Down") {
			problems = append(problems, fmt.Sprintf("%s: missing '-- +goose Down'; it could not be rolled back", e.Name()))
		}
	}

	if count == 0 {
		return errors.New("no migrations are embedded; the build is broken")
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "  - "+p)
		}
		return fmt.Errorf("%d migration problem(s) found", len(problems))
	}

	fmt.Printf("validated %d migrations\n", count)
	return nil
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// gooseLogger adapts goose's logger to slog.
type gooseLogger struct{ log *slog.Logger }

func (g gooseLogger) Fatalf(format string, v ...any) {
	g.log.Error(fmt.Sprintf(format, v...))
	os.Exit(1)
}

func (g gooseLogger) Printf(format string, v ...any) {
	g.log.Info(trimNewline(fmt.Sprintf(format, v...)))
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
