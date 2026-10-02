package devauth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsLocalHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost":         true,
		"LOCALHOST.":        true,
		"127.0.0.1":         true,
		"::1":               true,
		"auth.vortech.test": true,
		"kc.localhost":      true,
		"vortech.example":   false,
		"10.0.0.5":          false,
		"localhost.evil.io": false,
		"":                  false,
	} {
		if got := IsLocalHost(host); got != want {
			t.Errorf("IsLocalHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestLoginRefusesRemoteHosts(t *testing.T) {
	_, err := Login(context.Background(), Config{KeycloakURL: "https://sso.example.com/auth", Realm: "r", ClientID: "c"}, "u", "p")
	if err == nil || !strings.Contains(err.Error(), "local development hosts") {
		t.Fatalf("expected refusal for remote host, got %v", err)
	}
}

func TestDevPassword(t *testing.T) {
	f := filepath.Join(t.TempDir(), "realm.json")
	content := `{"users":[{"username":"player1","credentials":[{"type":"password","value":"pw-1"}]},{"username":"nopw","credentials":[]}]}`
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if pw, err := DevPassword(f, "player1"); err != nil || pw != "pw-1" {
		t.Fatalf("got %q %v", pw, err)
	}
	for _, u := range []string{"nopw", "ghost"} {
		if _, err := DevPassword(f, u); err == nil {
			t.Errorf("expected error for %q", u)
		}
	}
}
