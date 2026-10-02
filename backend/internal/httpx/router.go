package httpx

import (
	"net/http"
)

// Router is a thin wrapper over http.ServeMux (Go 1.22+ method and wildcard
// patterns) that renders unmatched routes in the standard JSON error format
// instead of ServeMux's plain-text 404/405 bodies.
type Router struct {
	mux *http.ServeMux
}

// NewRouter returns an empty router.
func NewRouter() *Router {
	return &Router{mux: http.NewServeMux()}
}

// Handle registers h for pattern, e.g. "GET /api/v1/health".
func (rt *Router) Handle(pattern string, h http.Handler) {
	rt.mux.Handle(pattern, h)
}

// HandleFunc registers f for pattern.
func (rt *Router) HandleFunc(pattern string, f http.HandlerFunc) {
	rt.mux.HandleFunc(pattern, f)
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h, pattern := rt.mux.Handler(r)
	if pattern != "" {
		// ServeMux.ServeHTTP must do the dispatch so that path wildcards
		// are populated for r.PathValue.
		rt.mux.ServeHTTP(w, r)
		return
	}

	// No route matched. Let ServeMux decide between 404 and 405 (it sets the
	// Allow header for the latter), then render that outcome as JSON.
	cw := &captureWriter{header: make(http.Header)}
	h.ServeHTTP(cw, r)
	switch cw.status {
	case http.StatusMethodNotAllowed:
		if allow := cw.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		WriteError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "Method not allowed for this resource.")
	default:
		WriteError(w, r, http.StatusNotFound, CodeNotFound, "Resource not found.")
	}
}

// captureWriter swallows ServeMux's plain-text fallback response.
type captureWriter struct {
	header http.Header
	status int
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return len(b), nil
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}
