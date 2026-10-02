package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Probe performs a local HTTP GET against path on the server bound to
// listenAddr. It backs the binaries' -healthcheck flag, which lets
// distroless images (no shell, no curl) define a container HEALTHCHECK.
func Probe(ctx context.Context, listenAddr, path string) error {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", listenAddr, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", port)+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: %s returned %d", path, resp.StatusCode)
	}
	return nil
}
