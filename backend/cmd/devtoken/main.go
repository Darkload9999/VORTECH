// Command devtoken logs a seeded development user in to the local Keycloak
// using the real Authorization Code + PKCE flow and prints the access token,
// for exercising the API with curl before the frontend exists:
//
//	curl -H "Authorization: Bearer $(go run ./cmd/devtoken -user player1)" localhost:8080/api/v1/me
//
// The password comes from DEVTOKEN_PASSWORD or, for seeded users, from the
// development realm file. It only talks to local development hosts.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Darkload9999/VORTECH/backend/internal/devauth"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "devtoken:", err)
		os.Exit(1)
	}
}

func run() error {
	user := flag.String("user", "player1", "username to log in as")
	output := flag.String("output", "access", "what to print: access, id, refresh or json")
	realmFile := flag.String("realm-file", "deployments/local/keycloak/realm/vortech-realm.json", "development realm import file holding seeded passwords")
	redirect := flag.String("redirect-uri", "http://localhost:5173/callback", "redirect URI registered on the client (never contacted)")
	flag.Parse()

	cfg := devauth.Config{
		KeycloakURL: envOr("KEYCLOAK_URL", "http://localhost:8180/auth"),
		Realm:       envOr("KEYCLOAK_REALM", "vortech"),
		ClientID:    envOr("KEYCLOAK_CLIENT_ID", "vortech-web"),
		RedirectURI: *redirect,
	}

	password := os.Getenv("DEVTOKEN_PASSWORD")
	if password == "" {
		pw, err := devauth.DevPassword(*realmFile, *user)
		if err != nil {
			return fmt.Errorf("%w (set DEVTOKEN_PASSWORD for non-seeded users)", err)
		}
		password = pw
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tok, err := devauth.Login(ctx, cfg, *user, password)
	if err != nil {
		return err
	}

	switch *output {
	case "access":
		fmt.Println(tok.AccessToken)
	case "id":
		fmt.Println(tok.IDToken)
	case "refresh":
		fmt.Println(tok.RefreshToken)
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(tok)
	default:
		return fmt.Errorf("unknown -output %q", *output)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
