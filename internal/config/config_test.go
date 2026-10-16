package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(env(map[string]string{"POD_NAMESPACE": "krci-env-daemon"}))
	require.NoError(t, err)
	require.Equal(t, "krci-env-daemon", cfg.Namespace)
	require.Equal(t, DefaultIdle, cfg.Idle)
	require.Equal(t, DefaultWakeAfterTimeout, cfg.WakeAfterTimeout)
	require.Equal(t, DefaultSelector, cfg.ScaledObjectSelector)
	require.Empty(t, cfg.WatchNamespaces)
	require.Empty(t, cfg.Gateway)
}

func TestLoadValues(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"POD_NAMESPACE":         "daemon",
		"IDLE":                  "30m",
		"WAKE_AFTER_TIMEOUT":    "90s",
		"SCALEDOBJECT_SELECTOR": "team=a",
		"WATCH_NAMESPACES":      " team-a, team-b,,team-a ",
		"GATEWAY":               "envoy-gateway-system/main-gateway",
	}))
	require.NoError(t, err)
	require.Equal(t, 30*time.Minute, cfg.Idle)
	require.Equal(t, 90*time.Second, cfg.WakeAfterTimeout)
	require.Equal(t, "team=a", cfg.ScaledObjectSelector)
	require.Equal(t, []string{"team-a", "team-b"}, cfg.WatchNamespaces)
	require.Equal(t, "envoy-gateway-system/main-gateway", cfg.Gateway)
}

func TestLoadRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		vars map[string]string
		want string
	}{
		"no namespace":              {map[string]string{}, "POD_NAMESPACE"},
		"bad duration":              {map[string]string{"POD_NAMESPACE": "d", "IDLE": "soon"}, "IDLE"},
		"zero duration":             {map[string]string{"POD_NAMESPACE": "d", "WAKE_AFTER_TIMEOUT": "0s"}, "WAKE_AFTER_TIMEOUT"},
		"negative duration":         {map[string]string{"POD_NAMESPACE": "d", "IDLE": "-1m"}, "IDLE"},
		"idle below a second":       {map[string]string{"POD_NAMESPACE": "d", "IDLE": "500ms"}, "IDLE"},
		"bad selector":              {map[string]string{"POD_NAMESPACE": "d", "SCALEDOBJECT_SELECTOR": "a=b=c"}, "SCALEDOBJECT_SELECTOR"},
		"bad namespace name":        {map[string]string{"POD_NAMESPACE": "d", "WATCH_NAMESPACES": "Team_A"}, "WATCH_NAMESPACES"},
		"gateway without namespace": {map[string]string{"POD_NAMESPACE": "d", "GATEWAY": "main-gateway"}, "GATEWAY"},
		"bad gateway name":          {map[string]string{"POD_NAMESPACE": "d", "GATEWAY": "envoy/Main Gateway"}, "GATEWAY"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(env(tc.vars))
			require.ErrorContains(t, err, tc.want)
		})
	}
}
