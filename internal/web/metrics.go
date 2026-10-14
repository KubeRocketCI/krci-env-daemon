package web

import (
	"fmt"
	"net/http"

	"github.com/KubeRocketCI/krci-env-daemon/internal/envstate"
)

// Snapshotter lists the environments.
type Snapshotter interface {
	Snapshot() []envstate.EnvView
}

type metric struct {
	name, help, kind string
	value            func(envstate.EnvView) float64
}

var metrics = []metric{
	{"krci_env_active", "1 while the environment is in use.", "gauge", func(v envstate.EnvView) float64 { return boolValue(v.Active) }},
	{"krci_env_components_ready", "Workloads of the environment that are Ready.", "gauge", func(v envstate.EnvView) float64 { return float64(v.Ready) }},
	{"krci_env_components", "Workloads of the environment.", "gauge", func(v envstate.EnvView) float64 { return float64(v.Total) }},
	{"krci_env_last_activity_timestamp_seconds", "Last use of the environment.", "gauge", func(v envstate.EnvView) float64 { return v.LastSeenUnix }},
	{"krci_env_wakeups_total", "Wakes since the daemon started.", "counter", func(v envstate.EnvView) float64 { return float64(v.Wakeups) }},
	{"krci_env_cold_start_seconds", "Time from the last wake until every workload was Ready.", "gauge", func(v envstate.EnvView) float64 { return v.ColdStartSeconds }},
}

// MetricsHandler serves the state of every environment in the Prometheus text format.
func MetricsHandler(state Snapshotter) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		envs := state.Snapshot()
		for _, m := range metrics {
			_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.kind)

			for _, v := range envs {
				_, _ = fmt.Fprintf(w, "%s{env=%q} %g\n", m.name, v.Env, m.value(v))
			}
		}
	})

	return mux
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}

	return 0
}
