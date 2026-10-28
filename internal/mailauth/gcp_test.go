package mailauth

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestGCPInfisicalIdentityExchangeAndSecretRead(t *testing.T) {
	metadataCalls, loginCalls := 0, 0
	s := &Infisical{URL: "https://app.infisical.com", ProjectID: "synthetic-project", Environment: "prod", Path: "/logistics", GCPIdentityID: "synthetic-identity"}
	s.Client = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "metadata.google.internal":
			metadataCalls++
			if r.URL.Scheme != "http" || r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/identity" || r.URL.Query().Get("audience") != s.GCPIdentityID || r.Header.Get("Metadata-Flavor") != "Google" || r.Header.Get("Authorization") != "" {
				t.Fatal("unexpected metadata request")
			}
			res := reply(200, "synthetic-google-jwt")
			res.Header.Set("Metadata-Flavor", "Google")
			return res, nil
		case "app.infisical.com":
			if r.URL.Path == "/api/v1/auth/gcp-auth/login" {
				loginCalls++
				var body map[string]string
				if json.NewDecoder(r.Body).Decode(&body) != nil || body["identityId"] != s.GCPIdentityID || body["jwt"] != "synthetic-google-jwt" {
					t.Fatal("incorrect GCP identity exchange")
				}
				return reply(200, `{"accessToken":"synthetic-machine-access","expiresIn":3600}`), nil
			}
			if r.Header.Get("Authorization") != "Bearer synthetic-machine-access" || r.URL.Query().Get("viewSecretValue") != "true" || r.URL.Query().Get("secretPath") != "/logistics" {
				t.Fatal("wrong secret scope or token")
			}
			return reply(200, `{"secret":{"secretValue":"synthetic-db","secretValueHidden":false}}`), nil
		}
		t.Fatal("unexpected destination")
		return nil, nil
	})}
	for range 2 {
		value, err := s.Secret(context.Background(), "DATABASE_URL")
		if err != nil || value != "synthetic-db" {
			t.Fatal("secret retrieval failed", err)
		}
	}
	if metadataCalls != 1 || loginCalls != 1 {
		t.Fatal("machine token not cached")
	}
}

func TestInfisicalOriginAndAuthValidation(t *testing.T) {
	for _, origin := range []string{"http://app.infisical.com", "https://user:password@app.infisical.com", "https://app.infisical.com/api", "https://app.infisical.com?redirect=bad", "https://app.infisical.com/#bad"} {
		s := &Infisical{URL: origin, ProjectID: "synthetic", Environment: "prod", Token: "synthetic"}
		if _, err := s.endpoint("SYNTHETIC"); err == nil {
			t.Fatal("unsafe origin accepted", origin)
		}
	}
	s := &Infisical{URL: "https://secrets.example.invalid/", ProjectID: "synthetic", Environment: "prod", Token: "synthetic"}
	if endpoint, err := s.endpoint("SYNTHETIC"); err != nil || endpoint != "https://secrets.example.invalid/api/v4/secrets/SYNTHETIC" {
		t.Fatal("configured origin not used", err)
	}
	s.GCPIdentityID = "synthetic"
	if _, err := s.endpoint("SYNTHETIC"); err == nil {
		t.Fatal("ambiguous auth modes accepted")
	}
}

func TestGCPMetadataFailureDoesNotSendCredentials(t *testing.T) {
	s := &Infisical{ProjectID: "synthetic", Environment: "prod", GCPIdentityID: "synthetic", Client: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "metadata.google.internal" {
			t.Fatal("identity exchange attempted after invalid metadata")
		}
		return reply(200, "synthetic-untrusted-response"), nil
	})}}
	if _, err := s.Secret(context.Background(), "SYNTHETIC"); err == nil {
		t.Fatal("missing metadata response header accepted")
	}
}
