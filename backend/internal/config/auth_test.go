package config

import (
	"strings"
	"testing"
	"time"
)

func authEnv() map[string]string {
	return map[string]string{
		"KEYCLOAK_URL":       "http://localhost:8180/auth/",
		"KEYCLOAK_REALM":     "vortech",
		"KEYCLOAK_CLIENT_ID": "vortech-web",
	}
}

func TestLoadAuthDefaultsAndDerivedURLs(t *testing.T) {
	cfg, err := LoadAuth(env(authEnv()), EnvDevelopment)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Issuer(); got != "http://localhost:8180/auth/realms/vortech" {
		t.Errorf("Issuer() = %q", got)
	}
	if got := cfg.JWKSURL(); got != "http://localhost:8180/auth/realms/vortech/protocol/openid-connect/certs" {
		t.Errorf("JWKSURL() = %q", got)
	}
	if cfg.Audience != "vortech-api" || cfg.ClockSkew != 30*time.Second || cfg.IdentityCacheTTL != 2*time.Minute {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadAuthInternalURL(t *testing.T) {
	e := authEnv()
	e["KEYCLOAK_INTERNAL_URL"] = "http://keycloak:8080/auth"
	cfg, err := LoadAuth(env(e), EnvDevelopment)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Issuer() != "http://localhost:8180/auth/realms/vortech" {
		t.Errorf("issuer must stay on the public URL, got %q", cfg.Issuer())
	}
	if cfg.JWKSURL() != "http://keycloak:8080/auth/realms/vortech/protocol/openid-connect/certs" {
		t.Errorf("JWKS must use the internal URL, got %q", cfg.JWKSURL())
	}
}

func TestLoadAuthValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		envName string
		wantErr string
	}{
		{"missing url", func(e map[string]string) { delete(e, "KEYCLOAK_URL") }, EnvDevelopment, "KEYCLOAK_URL: required"},
		{"missing realm", func(e map[string]string) { delete(e, "KEYCLOAK_REALM") }, EnvDevelopment, "KEYCLOAK_REALM: required"},
		{"missing client", func(e map[string]string) { delete(e, "KEYCLOAK_CLIENT_ID") }, EnvDevelopment, "KEYCLOAK_CLIENT_ID: required"},
		{"relative url", func(e map[string]string) { e["KEYCLOAK_URL"] = "/auth" }, EnvDevelopment, "absolute URL"},
		{"bad scheme", func(e map[string]string) { e["KEYCLOAK_URL"] = "ftp://kc/auth" }, EnvDevelopment, "scheme"},
		{"http in production", func(e map[string]string) {}, EnvProduction, "https in production"},
		{"credentials in url", func(e map[string]string) { e["KEYCLOAK_URL"] = "https://u:p@kc/auth" }, EnvDevelopment, "credentials"},
		{"excessive skew", func(e map[string]string) { e["AUTH_CLOCK_SKEW"] = "10m" }, EnvDevelopment, "AUTH_CLOCK_SKEW"},
		{"refresh too frequent", func(e map[string]string) { e["AUTH_JWKS_REFRESH_INTERVAL"] = "5s" }, EnvDevelopment, "AUTH_JWKS_REFRESH_INTERVAL"},
		{"cache too long", func(e map[string]string) { e["AUTH_IDENTITY_CACHE_TTL"] = "1h" }, EnvDevelopment, "AUTH_IDENTITY_CACHE_TTL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := authEnv()
			tt.mutate(e)
			_, err := LoadAuth(env(e), tt.envName)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadAuthHTTPSInProduction(t *testing.T) {
	e := authEnv()
	e["KEYCLOAK_URL"] = "https://vortech.example/auth"
	e["KEYCLOAK_INTERNAL_URL"] = "http://keycloak.platform.svc:8080/auth"
	if _, err := LoadAuth(env(e), EnvProduction); err != nil {
		t.Fatalf("https public URL with in-cluster http internal URL should be valid: %v", err)
	}
}
