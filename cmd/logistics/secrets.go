package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
)

type runtimeSecrets interface {
	Secret(context.Context, string) (string, error)
}

// Bootstrap only explicitly named application settings, never arbitrary env names.
// Deployment configuration contains references; values come from Infisical.
func bootstrapSecrets(ctx context.Context, store runtimeSecrets, names string, set func(string, string) error) error {
	if names == "" {
		return nil
	}
	allowed := map[string]bool{"DATABASE_URL": true, "API_TOKEN": true, "MAIL_GOOGLE_CLIENT_ID": true, "MAIL_GOOGLE_CLIENT_SECRET": true, "MAIL_MICROSOFT_CLIENT_ID": true, "MAIL_MICROSOFT_CLIENT_SECRET": true}
	keys := strings.Split(names, ",")
	seen := map[string]bool{}
	for _, key := range keys {
		if !allowed[key] || seen[key] {
			return errors.New("invalid Infisical bootstrap secret list")
		}
		seen[key] = true
	}
	values := make([]string, len(keys))
	for i, key := range keys {
		value, err := store.Secret(ctx, key)
		if err != nil || value == "" || strings.ContainsRune(value, '\x00') {
			return errors.New("runtime secret bootstrap failed; check scoped Infisical access")
		}
		values[i] = value
	}
	for i, key := range keys {
		if set(key, values[i]) != nil {
			return errors.New("runtime secret configuration failed")
		}
	}
	return nil
}

func loadRuntimeSecrets() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := tokenStore()
	if path := os.Getenv("INFISICAL_CONFIG_PATH"); path != "" {
		store.Path = path
	}
	return bootstrapSecrets(ctx, store, os.Getenv("INFISICAL_BOOTSTRAP_SECRETS"), os.Setenv)
}
