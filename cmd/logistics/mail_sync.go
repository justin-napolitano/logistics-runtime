package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/mailauth"
	"github.com/justin-napolitano/logistics-runtime/internal/mailsync"
	"github.com/justin-napolitano/logistics-runtime/internal/postgres"
	"golang.org/x/oauth2"
)

func oauthConfig(provider string) (oauth2.Config, error) {
	if provider == "gmail" {
		return mailauth.Config(provider, os.Getenv("MAIL_GOOGLE_CLIENT_ID"), os.Getenv("MAIL_GOOGLE_CLIENT_SECRET"), "")
	}
	return mailauth.Config(provider, os.Getenv("MAIL_MICROSOFT_CLIENT_ID"), os.Getenv("MAIL_MICROSOFT_CLIENT_SECRET"), os.Getenv("MAIL_MICROSOFT_TENANT"))
}
func tokenStore() *mailauth.Infisical {
	return &mailauth.Infisical{ProjectID: os.Getenv("INFISICAL_PROJECT_ID"), Environment: os.Getenv("INFISICAL_ENVIRONMENT"), Path: os.Getenv("INFISICAL_SECRET_PATH"), Token: os.Getenv("INFISICAL_TOKEN"), UniversalClientID: os.Getenv("INFISICAL_UNIVERSAL_AUTH_CLIENT_ID"), UniversalClientSecret: os.Getenv("INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET")}
}
func providerWithToken(c mailsync.Connection, access mailsync.AccessToken) mailsync.Provider {
	api := mailsync.NewAPI(nil, access)
	if c.Provider == "gmail" {
		return mailsync.Gmail{API: api}
	}
	return mailsync.Outlook{API: api}
}
func provider(c mailsync.Connection) (mailsync.Provider, error) {
	config, e := oauthConfig(c.Provider)
	if e != nil {
		return nil, e
	}
	manager := &mailauth.Manager{Config: config, Store: tokenStore(), Ref: c.CredentialRef}
	return providerWithToken(c, manager.Access), nil
}
func runMailbox(logger *slog.Logger, command string, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	connect, cancel := context.WithTimeout(ctx, 10*time.Second)
	s, e := postgres.Open(connect, os.Getenv("DATABASE_URL"))
	cancel()
	if e != nil {
		return e
	}
	defer s.Close()
	switch command {
	case "mail-configure":
		if len(args) != 1 {
			return errors.New("usage: logistics mail-configure CONNECTION.json")
		}
		f, e := os.Open(args[0])
		if e != nil {
			return errors.New("cannot open connection config")
		}
		defer f.Close()
		b, e := io.ReadAll(io.LimitReader(f, 16385))
		if e != nil || len(b) > 16384 {
			return mailsync.ErrInvalid
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var c mailsync.Connection
		if dec.Decode(&c) != nil || dec.Decode(new(any)) != io.EOF {
			return mailsync.ErrInvalid
		}
		if e = s.ConfigureMail(ctx, c); e != nil {
			return errors.New("connection configuration rejected; identifiers/configuration are immutable")
		}
		return nil
	case "mail-status":
		if len(args) != 0 {
			return mailsync.ErrInvalid
		}
		out, e := s.MailStatuses(ctx)
		if e != nil {
			return errors.New("mail sync state unavailable; apply migrations first")
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	case "mail-authorize":
		if len(args) != 1 {
			return errors.New("usage: logistics mail-authorize CONNECTION_ID")
		}
		return authorize(ctx, s, args[0])
	case "mail-reset", "mail-pause", "mail-resume":
		if len(args) != 1 {
			return mailsync.ErrInvalid
		}
		lease, e := s.AcquireMail(ctx, args[0])
		if e != nil {
			return e
		}
		defer lease.Close()
		p := lease.Progress()
		if command == "mail-reset" {
			p.Cursor = mailsync.Cursor{}
		}
		p.Status = "pending"
		p.ErrorCode = ""
		if command == "mail-pause" {
			p.Status = "paused"
		}
		return lease.Save(ctx, p)
	case "mail-sync", "mail-worker":
		flags := flag.NewFlagSet(command, flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		interval := flags.Duration("interval", 15*time.Minute, "worker interval")
		timeout := flags.Duration("timeout", 5*time.Minute, "per-connection timeout")
		pages := flags.Int("max-pages", 20, "bounded pages per run")
		if flags.Parse(args) != nil || len(flags.Args()) > 1 || *timeout < time.Second || *timeout > 30*time.Minute || *pages < 1 || *pages > 100 || *interval < time.Second || *interval > 24*time.Hour {
			return mailsync.ErrInvalid
		}
		run := func() error {
			connections, e := s.MailConnections(ctx)
			if e != nil {
				return errors.New("mail configuration unavailable")
			}
			id := ""
			if len(flags.Args()) == 1 {
				id = flags.Args()[0]
			}
			found := id == ""
			failed := false
			for _, c := range connections {
				if id != "" && c.ID != id {
					continue
				}
				found = true
				work, cancel := context.WithTimeout(ctx, *timeout)
				status, e := (mailsync.Runner{Store: s, MaxPages: *pages}).Run(work, c.ID, provider)
				cancel()
				if errors.Is(e, mailsync.ErrBusy) {
					logger.Info("mail sync already active")
					continue
				}
				if e != nil {
					failed = true
					logger.Error("mail sync failed; inspect mail-status")
				}
				if status.ID != "" {
					if e = json.NewEncoder(os.Stdout).Encode(status); e != nil {
						return e
					}
				}
			}
			if !found {
				return errors.New("connection not found")
			}
			if failed {
				return errors.New("one or more mail syncs failed; inspect mail-status")
			}
			return nil
		}
		if command == "mail-sync" {
			return run()
		}
		ticker := time.NewTicker(*interval)
		defer ticker.Stop()
		for {
			if e = run(); e != nil {
				logger.Error("mail worker cycle failed; inspect mail-status")
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	}
	return errors.New("unknown mailbox command")
}
func authorize(ctx context.Context, s *postgres.Store, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	lease, e := s.AcquireMail(ctx, id)
	if e != nil {
		return e
	}
	defer lease.Close()
	c := lease.Connection()
	config, e := oauthConfig(c.Provider)
	if e != nil {
		return e
	}
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return errors.New("cannot open loopback OAuth listener")
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	host := "127.0.0.1"
	if c.Provider == "outlook" {
		host = "localhost"
	}
	redirect := "http://" + host + ":" + strconv.Itoa(port) + "/callback"
	flow := mailauth.NewFlow(config, redirect, nil)
	server := &http.Server{Handler: flow, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 40 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer server.Close()
	_, _ = fmt.Fprintln(os.Stdout, "Open this provider authorization URL in your browser. Do not paste passwords or authorization codes into chat:")
	_, _ = fmt.Fprintln(os.Stdout, flow.URL())
	select {
	case <-ctx.Done():
		return errors.New("authorization timed out or cancelled")
	case <-done:
		return errors.New("authorization listener stopped")
	case result := <-flow.Result:
		if result.Err != nil {
			return result.Err
		}
		access := func(context.Context, bool) (string, error) { return result.Token.AccessToken, nil }
		identity, e := providerWithToken(c, access).Identity(ctx)
		if e != nil {
			return e
		}
		p := lease.Progress()
		hash := mailsync.IdentityHash(c.Provider, identity)
		if p.IdentityHash != "" && p.IdentityHash != hash {
			return errors.New("different mailbox selected; use a new connection ID")
		}
		if e = tokenStore().Save(ctx, c.CredentialRef, result.Token); e != nil {
			return e
		}
		p.IdentityHash = hash
		if p.Status != "paused" {
			p.Status = "pending"
		}
		p.ErrorCode = ""
		if e = lease.Save(ctx, p); e != nil {
			return e
		}
		_, _ = fmt.Fprintln(os.Stdout, "Authorization saved in Infisical. Run mail-sync to start importing.")
		return nil
	}
}
