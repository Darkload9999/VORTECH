// Package devauth performs a real OIDC Authorization Code + PKCE login
// against a local development Keycloak by submitting its login form, the
// way a browser would. It exists so integration tests and the devtoken CLI
// can obtain genuine tokens without a frontend.
//
// It is development tooling: it refuses any Keycloak that is not on a local
// development host, and it is never linked into the api or worker binaries.
package devauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Config describes the realm and client to log in to.
type Config struct {
	KeycloakURL string // public base URL, e.g. http://localhost:8180/auth
	Realm       string
	ClientID    string
	RedirectURI string // must be registered on the client; never contacted
}

// Tokens are the endpoint's token response.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// ErrLoginRejected means Keycloak did not accept the credentials or
// requires an interactive step (required action, OTP).
var ErrLoginRejected = errors.New("login rejected")

var formAction = regexp.MustCompile(`action="([^"]*login-actions/authenticate[^"]*)"`)

// Login authenticates username/password and returns tokens.
func Login(ctx context.Context, cfg Config, username, password string) (*Tokens, error) {
	return login(ctx, cfg, username, password, "S256")
}

// LoginWithChallengeMethod is Login with an explicit PKCE method; tests use
// it to prove "plain" is refused.
func LoginWithChallengeMethod(ctx context.Context, cfg Config, username, password, method string) (*Tokens, error) {
	return login(ctx, cfg, username, password, method)
}

func login(ctx context.Context, cfg Config, username, password, method string) (*Tokens, error) {
	base, err := url.Parse(strings.TrimRight(cfg.KeycloakURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse keycloak url: %w", err)
	}
	if !IsLocalHost(base.Hostname()) {
		return nil, fmt.Errorf("devauth only runs against local development hosts, not %q", base.Hostname())
	}

	verifier := randomString(48)
	challenge := verifier
	if method == "S256" {
		sum := sha256.Sum256([]byte(verifier))
		challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	}
	state := randomString(16)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:     jar,
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if strings.HasPrefix(req.URL.String(), cfg.RedirectURI) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	realmURL := base.String() + "/realms/" + url.PathEscape(cfg.Realm) + "/protocol/openid-connect"

	authURL := realmURL + "/auth?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {cfg.ClientID},
		"redirect_uri":          {cfg.RedirectURI},
		"scope":                 {"openid"},
		"state":                 {state},
		"nonce":                 {randomString(16)},
		"code_challenge":        {challenge},
		"code_challenge_method": {method},
	}.Encode()
	page, status, err := get(ctx, client, authURL)
	if err != nil {
		return nil, err
	}
	m := formAction.FindStringSubmatch(page)
	if status != http.StatusOK || m == nil {
		return nil, fmt.Errorf("%w: authorization request refused (status %d)", ErrLoginRejected, status)
	}

	form := url.Values{"username": {username}, "password": {password}, "credentialId": {""}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, html.UnescapeString(m[1]), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("submit login form: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		return nil, fmt.Errorf("%w: login form returned status %d", ErrLoginRejected, resp.StatusCode)
	}
	cb, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || !strings.HasPrefix(cb.String(), cfg.RedirectURI) {
		return nil, fmt.Errorf("%w: unexpected redirect", ErrLoginRejected)
	}
	q := cb.Query()
	if e := q.Get("error"); e != "" {
		return nil, fmt.Errorf("%w: %s", ErrLoginRejected, e)
	}
	if q.Get("state") != state {
		return nil, errors.New("state mismatch in authorization response")
	}

	tokenResp, err := client.PostForm(realmURL+"/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {q.Get("code")},
		"redirect_uri":  {cfg.RedirectURI},
		"client_id":     {cfg.ClientID},
		"code_verifier": {verifier},
	})
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}
	defer tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: token endpoint returned status %d", ErrLoginRejected, tokenResp.StatusCode)
	}
	var t Tokens
	if err := json.NewDecoder(tokenResp.Body).Decode(&t); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	return &t, nil
}

func get(ctx context.Context, c *http.Client, u string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("authorization request: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), resp.StatusCode, err
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// IsLocalHost reports whether host is a local development host: localhost,
// a loopback address, or a *.localhost / *.test name.
func IsLocalHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".test") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// DevPassword looks up a seeded development user's password in the realm
// import file, so the file stays the single source of dev credentials.
func DevPassword(realmFile, username string) (string, error) {
	b, err := os.ReadFile(realmFile)
	if err != nil {
		return "", err
	}
	var realm struct {
		Users []struct {
			Username    string `json:"username"`
			Credentials []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"credentials"`
		} `json:"users"`
	}
	if err := json.Unmarshal(b, &realm); err != nil {
		return "", fmt.Errorf("parse realm file: %w", err)
	}
	for _, u := range realm.Users {
		if u.Username != username {
			continue
		}
		for _, c := range u.Credentials {
			if c.Type == "password" {
				return c.Value, nil
			}
		}
	}
	return "", fmt.Errorf("no seeded password for user %q in %s", username, realmFile)
}
