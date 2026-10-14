// Package web serves the waiting page's status and assets, the admin API and the metrics.
package web

import (
	"embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/KubeRocketCI/krci-env-daemon/internal/envstate"
)

// PathPrefix is the path under which Envoy routes the waiting page's requests to the
// daemon, on every host of every environment.
const PathPrefix = "/.krci-wake/"

const (
	// Each open waiting page asks once a second, so the limit allows about 20 of them per
	// environment; the cache below keeps their cost at one lookup per TTL.
	statusRate  rate.Limit = 20 // status requests per second and environment
	statusBurst            = 40
	statusTTL              = 500 * time.Millisecond
)

//go:embed assets
var assetFS embed.FS

// assetTypes is the allowlist of assets the daemon serves, with their content types.
var assetTypes = map[string]string{
	"wake.js":  "text/javascript; charset=utf-8",
	"wake.css": "text/css; charset=utf-8",
	"bg.webp":  "image/webp",
	"logo.svg": "image/svg+xml",
}

// Hosts maps a request host to the namespace of its environment.
type Hosts interface {
	HostEnv(host string) string
}

// Status is what the public endpoints need from the environment state.
type Status interface {
	Touch(ns, why string)
	Status(ns string) envstate.StatusDoc
}

// Public serves /healthz and, under PathPrefix, the progress of the environment that owns
// the request's host, plus the waiting page's assets. The environment comes only from the
// Host header through the HTTPRoute allowlist; nothing takes parameters or reflects input.
func Public(state Status, hosts Hosts) http.Handler {
	return public(state, hosts, time.Now)
}

func public(state Status, hosts Hosts, now func() time.Time) http.Handler {
	limit := newLimiter(statusRate, statusBurst, now)
	cache := &statusCache{ttl: statusTTL, now: now, items: map[string]cached{}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET "+PathPrefix+"status", func(w http.ResponseWriter, r *http.Request) {
		ns := hosts.HostEnv(r.Host)
		if ns == "" {
			http.NotFound(w, r)
			return
		}

		if !limit.allow(ns) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests", http.StatusTooManyRequests)

			return
		}

		state.Touch(ns, "waiting page")

		body := cache.get(ns, func() []byte {
			b, _ := json.Marshal(state.Status(ns)) // StatusDoc holds strings, ints and bools only
			return b
		})

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("GET "+PathPrefix+"assets/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")

		typ, ok := assetTypes[name]
		if !ok {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", typ)
		w.Header().Set("Cache-Control", "public, max-age=3600")

		if strings.HasSuffix(name, ".svg") {
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		}

		http.ServeFileFS(w, r, assetFS, "assets/"+name)
	})

	return hardened(mux)
}

// hardened sets the same security headers on every public answer, the mux's own 404 and
// 405 included. Every route is GET-only, so the mux answers other methods with 405.
func hardened(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")

		next.ServeHTTP(w, r)
	})
}

// statusCache answers every viewer of one environment from the same body for a short
// while, so many open waiting pages cost one lookup.
type statusCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	now   func() time.Time
	items map[string]cached
}

type cached struct {
	at   time.Time
	body []byte
}

func (c *statusCache) get(key string, build func() []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if it, ok := c.items[key]; ok && now.Sub(it.at) < c.ttl {
		return it.body
	}

	b := build()
	c.items[key] = cached{at: now, body: b}

	return b
}
