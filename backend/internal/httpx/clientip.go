package httpx

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

type clientIPKey struct{}

// ClientIPResolver determines the real client address. X-Forwarded-For is
// honoured only when the direct peer is a trusted proxy, and the chain is
// walked right-to-left so that client-supplied (spoofable) entries on the
// left are never preferred over what our own proxies appended.
type ClientIPResolver struct {
	trusted []netip.Prefix
}

// NewClientIPResolver returns a resolver trusting the given proxy prefixes.
func NewClientIPResolver(trusted []netip.Prefix) *ClientIPResolver {
	return &ClientIPResolver{trusted: trusted}
}

func (c *ClientIPResolver) isTrusted(a netip.Addr) bool {
	for _, p := range c.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Resolve returns the client IP for r, or the zero Addr if the peer address
// cannot be parsed.
func (c *ClientIPResolver) Resolve(r *http.Request) netip.Addr {
	peer := parseHostAddr(r.RemoteAddr)
	if !peer.IsValid() || !c.isTrusted(peer) {
		return peer
	}

	// Combine repeated headers in order; each may itself be a list.
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(h, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			// A malformed hop means the chain cannot be trusted beyond it.
			return peer
		}
		a = a.Unmap()
		if !c.isTrusted(a) {
			return a
		}
		peer = a
	}
	return peer
}

func parseHostAddr(hostport string) netip.Addr {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// ClientIP returns the client address resolved by the ClientIP middleware.
func ClientIP(ctx context.Context) netip.Addr {
	a, _ := ctx.Value(clientIPKey{}).(netip.Addr)
	return a
}

// ClientIPMiddleware stores the resolved client address in the request context.
func ClientIPMiddleware(res *ClientIPResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), clientIPKey{}, res.Resolve(r))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
