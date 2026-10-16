// Package config reads the daemon's settings from the environment and holds its fixed
// ports.
package config

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Defaults of SCALEDOBJECT_SELECTOR, IDLE and WAKE_AFTER_TIMEOUT. deploy-templates/values.yaml
// declares the same values; test/deploy keeps the two in step.
const (
	DefaultSelector         = "krci.epam.com/env-daemon=enabled"
	DefaultIdle             = 15 * time.Minute
	DefaultWakeAfterTimeout = 2 * time.Minute
)

// Ports the daemon listens on. The chart's container and Service ports must match them;
// test/deploy checks that.
const (
	// PublicPort serves the waiting page's status and assets through Envoy.
	PublicPort = 8080
	// MetricsPort serves the Prometheus metrics.
	MetricsPort = 8081
	// AdminPort serves the admin API on the pod's loopback only.
	AdminPort = 8082
	// ScalerPort serves KEDA's external scaler protocol.
	ScalerPort = 9090
	// OTLPPort receives Envoy's access log.
	OTLPPort = 4317
)

// Limits of the gRPC listeners. A client can make the daemon hold at most MaxStreams
// requests of MaxMessage bytes at a time; decoding an OTLP batch takes about ten times its
// wire size in memory for Envoy's records, up to a hundred times for empty ones. Envoy
// flushes its access log every 16 KiB or every second (buffer_size_bytes,
// buffer_flush_interval), and KEDA's requests are a few bytes. A connection holds transport
// buffers before it sends anything; Envoy opens one per worker thread, KEDA one in all.
const (
	OTLPMaxMessage   = 64 << 10
	OTLPMaxStreams   = 16
	OTLPMaxConns     = 256
	ScalerMaxMessage = 64 << 10
	// ScalerMaxStreams bounds the StreamIsActive streams: KEDA holds one per ScaledObject.
	ScalerMaxStreams = 4096
	ScalerMaxConns   = 64
)

// Config is the daemon's runtime configuration.
type Config struct {
	// Namespace holds the daemon's Leases (POD_NAMESPACE).
	Namespace string
	// Idle is how long an environment stays active after its last use (IDLE).
	Idle time.Duration
	// WakeAfterTimeout is the longest wait for a wake-after dependency before the
	// dependent workload starts anyway (WAKE_AFTER_TIMEOUT).
	WakeAfterTimeout time.Duration
	// ScaledObjectSelector is the label selector of the ScaledObjects the daemon drives
	// (SCALEDOBJECT_SELECTOR).
	ScaledObjectSelector string
	// WatchNamespaces limits the daemon to these namespaces (WATCH_NAMESPACES, comma
	// separated). Empty means every namespace.
	WatchNamespaces []string
	// Gateway is the Gateway whose accepted HTTPRoutes give the environments their
	// hostnames, as namespace/name (GATEWAY). Empty accepts the routes of any Gateway.
	Gateway string
}

// Load reads the configuration from the process environment.
func Load() (*Config, error) {
	return load(os.Getenv)
}

func load(getenv func(string) string) (*Config, error) {
	cfg := &Config{
		Namespace:            getenv("POD_NAMESPACE"),
		ScaledObjectSelector: cmp.Or(getenv("SCALEDOBJECT_SELECTOR"), DefaultSelector),
	}
	if cfg.Namespace == "" {
		return nil, errors.New("POD_NAMESPACE is not set")
	}

	if _, err := labels.Parse(cfg.ScaledObjectSelector); err != nil {
		return nil, fmt.Errorf("SCALEDOBJECT_SELECTOR: %w", err)
	}

	var err error
	// The Leases keep the idle window in whole seconds.
	if cfg.Idle, err = duration(getenv, "IDLE", DefaultIdle, time.Second); err != nil {
		return nil, err
	}

	if cfg.WakeAfterTimeout, err = duration(getenv, "WAKE_AFTER_TIMEOUT", DefaultWakeAfterTimeout, 0); err != nil {
		return nil, err
	}

	for ns := range strings.SplitSeq(getenv("WATCH_NAMESPACES"), ",") {
		ns = strings.TrimSpace(ns)
		if ns == "" || slices.Contains(cfg.WatchNamespaces, ns) {
			continue
		}

		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return nil, fmt.Errorf("WATCH_NAMESPACES: %q: %s", ns, strings.Join(errs, "; "))
		}

		cfg.WatchNamespaces = append(cfg.WatchNamespaces, ns)
	}

	if cfg.Gateway = getenv("GATEWAY"); cfg.Gateway != "" {
		ns, name, ok := strings.Cut(cfg.Gateway, "/")
		if !ok || len(validation.IsDNS1123Label(ns)) > 0 || len(validation.IsDNS1123Subdomain(name)) > 0 {
			return nil, fmt.Errorf("GATEWAY: %q is not namespace/name", cfg.Gateway)
		}
	}

	return cfg, nil
}

func duration(getenv func(string) string, key string, def, least time.Duration) (time.Duration, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}

	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}

	if d <= 0 {
		return 0, fmt.Errorf("%s: must be positive, got %s", key, v)
	}

	if d < least {
		return 0, fmt.Errorf("%s: must be at least %s, got %s", key, least, v)
	}

	return d, nil
}
