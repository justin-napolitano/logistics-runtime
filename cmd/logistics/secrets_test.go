package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeSecrets struct {
	values map[string]string
	calls  []string
}

func (s *fakeSecrets) Secret(_ context.Context, name string) (string, error) {
	s.calls = append(s.calls, name)
	if value := s.values[name]; value != "" {
		return value, nil
	}
	return "", errors.New("private provider details")
}

func TestRuntimeSecretBootstrap(t *testing.T) {
	store := &fakeSecrets{values: map[string]string{"DATABASE_URL": "synthetic-db", "MAIL_GOOGLE_CLIENT_ID": "synthetic-client"}}
	env := map[string]string{}
	set := func(k, v string) error { env[k] = v; return nil }
	for _, list := range []string{"PATH", "DATABASE_URL,PATH", "DATABASE_URL,DATABASE_URL", "DATABASE_URL,"} {
		if bootstrapSecrets(context.Background(), store, list, set) == nil || len(store.calls) != 0 || len(env) != 0 {
			t.Fatal("invalid list performed secret access or env mutation")
		}
	}
	if err := bootstrapSecrets(context.Background(), store, "DATABASE_URL,API_TOKEN", set); err == nil || strings.Contains(err.Error(), "private") || len(env) != 0 {
		t.Fatal("partial bootstrap or provider details exposed", err)
	}
	if err := bootstrapSecrets(context.Background(), store, "DATABASE_URL,MAIL_GOOGLE_CLIENT_ID", set); err != nil || env["DATABASE_URL"] != "synthetic-db" || env["MAIL_GOOGLE_CLIENT_ID"] != "synthetic-client" {
		t.Fatal("bootstrap did not populate application settings", err)
	}
}
