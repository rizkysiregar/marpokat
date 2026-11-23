// Package httpx holds the HTTP plumbing shared by the public and management servers.
package httpx

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
)

type ctxKey int

const requestIDKey ctxKey = iota

// HeaderRequestID is read from the client when valid and always echoed in the response.
const HeaderRequestID = "X-Request-Id"

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// RequestID returns the id attached by the RequestID middleware, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares so that the first one is the outermost.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// WithCleanInput answers 400 to a request whose path or query holds a NUL character (%00): Postgres rejects such text,
// which would otherwise come back as a 500 from whichever query the value reaches first.
func WithCleanInput() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.ContainsRune(r.URL.Path, 0) || strings.Contains(r.URL.RawQuery, "%00") || strings.ContainsRune(r.URL.RawQuery, 0) {
				WriteError(w, r, http.StatusBadRequest, CodeBadRequest, "the address must not contain the NUL character")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// WithRequestID attaches a request id: the client's X-Request-Id when it looks sane, a new UUIDv7 otherwise.
func WithRequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(HeaderRequestID)
			if !validRequestID.MatchString(id) {
				id = uuid.Must(uuid.NewV7()).String()
			}
			w.Header().Set(HeaderRequestID, id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
		})
	}
}

// WithRecover turns a panic into a 500 with the standard body and logs the stack.
func WithRecover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(v)
					}
					log.Error("panic in handler", "panic", v, "path", r.URL.Path,
						"request_id", RequestID(r.Context()), "stack", string(debug.Stack()))
					WriteError(w, r, http.StatusInternalServerError, CodeInternal, "internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// WithObservability logs each request and records Prometheus metrics by route pattern (not raw path,
// which would explode label cardinality with ids). Health probes are not logged.
func WithObservability(log *slog.Logger, m *metrics.Registry) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)

			route := r.Pattern // set by http.ServeMux on this same request
			if route == "" {
				route = "unmatched"
			}
			elapsed := time.Since(start)
			if m != nil {
				m.HTTPRequests.WithLabelValues(route, r.Method, strconv.Itoa(sw.status)).Inc()
				m.HTTPDuration.WithLabelValues(route, r.Method).Observe(elapsed.Seconds())
			}
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				return
			}
			level := slog.LevelInfo
			if sw.status >= 500 {
				level = slog.LevelError
			}
			log.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("route", route),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.status),
				slog.Int64("bytes", sw.bytes),
				slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
				slog.String("request_id", RequestID(r.Context())),
				slog.String("remote_ip", ClientIP(r)),
			)
		})
	}
}

// ClientIP prefers Cloudflare's header (every public request comes through the tunnel), then the proxy chain.
func ClientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Real-Ip"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// statusWriter records the status code. It keeps Hijack and Flush working, which the WebSocket upgrade needs.
type statusWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("httpx: underlying ResponseWriter does not support hijacking")
	}
	w.status = http.StatusSwitchingProtocols
	w.wroteHeader = true
	return h.Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// WithCORS lets browser apps of any origin call the API. This is safe because requests are authenticated by
// headers (X-Api-Key and a bearer token) that a malicious page cannot obtain, never by cookies, so no credentials
// are allowed. Preflight requests are answered here, before authentication.
func WithCORS() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Origin") == "" {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", "*")
			h.Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, X-Request-Id")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
