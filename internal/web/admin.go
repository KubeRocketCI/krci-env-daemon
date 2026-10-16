package web

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/KubeRocketCI/krci-env-daemon/internal/envstate"
)

// Admin is what the admin API needs from the environment state.
type Admin interface {
	WakeNow(ns string) bool
	SleepNow(ns string) bool
	Snapshot() []envstate.EnvView
}

// AdminHandler serves the admin API. Bind it to the pod's loopback: then only
// kubectl port-forward reaches it, which needs pods/portforward in the daemon's namespace.
// A web page cannot use an open port-forward on the administrator's machine: requests
// that carry an Origin header are refused, and so are requests for any Host but the
// loopback names, which a DNS-rebinding page would send.
func AdminHandler(state Admin) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/envs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(state.Snapshot())
	})
	mux.HandleFunc("POST /admin/sleep", envAction(state.SleepNow, "asleep: KEDA scales the workloads to zero after the cooldownPeriod"))
	mux.HandleFunc("POST /admin/wake", envAction(state.WakeNow, "waking"))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			http.Error(w, "cross-origin requests are refused", http.StatusForbidden)
			return
		}

		if !loopback(r.Host) {
			http.Error(w, "the admin API answers requests for localhost only", http.StatusMisdirectedRequest)
			return
		}

		mux.ServeHTTP(w, r)
	})
}

// envAction applies do to the ?env= environment and answers done, or 404 for an unknown one.
func envAction(do func(ns string) bool, done string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !do(r.URL.Query().Get("env")) {
			http.NotFound(w, r)
			return
		}

		_, _ = fmt.Fprintln(w, done)
	}
}

func loopback(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}

	host = strings.Trim(host, "[]")

	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
