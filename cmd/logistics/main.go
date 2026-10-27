// Command logistics runs the private inventory API, explicit migrations, or proposal import.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/httpapi"
	"github.com/justin-napolitano/logistics-runtime/internal/inventory"
	"github.com/justin-napolitano/logistics-runtime/internal/postgres"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	mailCommands := map[string]bool{"mail-configure": true, "mail-status": true, "mail-authorize": true, "mail-sync": true, "mail-worker": true, "mail-reset": true, "mail-pause": true, "mail-resume": true}
	if mailCommands[command] {
		return runMailbox(logger, command, os.Args[2:])
	}
	if command == "basket-preview" {
		return preview(os.Args[2:])
	}
	if command == "openapi" {
		return json.NewEncoder(os.Stdout).Encode(httpapi.Spec())
	}
	extra := map[string]bool{"save-template": true, "prepare-basket": true, "basket-export": true, "import-mail": true, "import-mail-dir": true, "orders": true, "mail-exceptions": true}
	if command != "serve" && command != "migrate" && command != "import-proposals" && !extra[command] {
		return errors.New("usage: logistics [serve|migrate|import-proposals|openapi|basket-preview|save-template|prepare-basket|basket-export|import-mail|import-mail-dir|orders|mail-exceptions|mail-configure|mail-authorize|mail-sync|mail-worker|mail-status|mail-pause|mail-resume|mail-reset]")
	}
	if command == "import-proposals" && len(os.Args) != 3 {
		return errors.New("usage: logistics import-proposals CSV")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL is required")
	}
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	store, err := postgres.Open(connectCtx, url)
	cancel()
	if err != nil {
		return err
	}
	defer store.Close()
	if command == "migrate" {
		mctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := store.Migrate(mctx); err != nil {
			return errors.New("migration failed; verify schema ownership and immutable migration history")
		}
		logger.Info("migrations applied")
		return nil
	}
	if command == "import-proposals" {
		file, err := os.Open(os.Args[2])
		if err != nil {
			return errors.New("cannot open proposal CSV")
		}
		defer file.Close()
		rows, err := inventory.ParseCatalog(file)
		if err != nil {
			return errors.New("invalid proposal CSV; check headers, IDs and unconfirmed quantities")
		}
		ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		added, err := store.ImportProposals(ictx, rows)
		if err != nil {
			return errors.New("proposal import failed")
		}
		logger.Info("proposals imported", "inserted", added, "skipped_existing", int64(len(rows))-added)
		return nil
	}
	if extra[command] {
		cmdctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return shoppingCommand(cmdctx, store, command, os.Args[2:])
	}
	handler, err := httpapi.New(store, os.Getenv("API_TOKEN"), logger)
	if err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = store.Ready(readyCtx)
	if err == nil {
		err = store.ShoppingReady(readyCtx)
	}
	cancel()
	if err != nil {
		return errors.New("inventory schema unavailable; run migrations using the schema owner first")
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	logger.Info("inventory API starting")
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("HTTP server failed")
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			return fmt.Errorf("HTTP shutdown failed")
		}
	}
	return nil
}
