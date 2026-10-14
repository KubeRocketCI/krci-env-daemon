// Command krci-env-daemon wakes KubeRocketCI environments that KEDA scaled to zero on the
// first request to them. Envoy Gateway streams its access log to the daemon over OTLP. The
// daemon answers KEDA's external-push trigger, so KEDA stays the only writer of replicas,
// and serves the progress that the waiting page shows.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/KubeRocketCI/krci-env-daemon/internal/cluster"
	"github.com/KubeRocketCI/krci-env-daemon/internal/config"
	"github.com/KubeRocketCI/krci-env-daemon/internal/envstate"
	"github.com/KubeRocketCI/krci-env-daemon/internal/externalscaler"
	"github.com/KubeRocketCI/krci-env-daemon/internal/grpcserver"
	"github.com/KubeRocketCI/krci-env-daemon/internal/otlp"
	"github.com/KubeRocketCI/krci-env-daemon/internal/scaler"
	"github.com/KubeRocketCI/krci-env-daemon/internal/web"
)

const stopTimeout = 5 * time.Second

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	klog.SetSlogLogger(slog.Default())

	if err := run(); err != nil {
		slog.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rc, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}

	rc.QPS, rc.Burst = 20, 40

	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}

	cl, err := cluster.New(ctx, cs, dyn, cluster.Options{
		Namespace:       cfg.Namespace,
		Selector:        cfg.ScaledObjectSelector,
		WatchNamespaces: cfg.WatchNamespaces,
		Gateway:         cfg.Gateway,
	})
	if err != nil {
		return fmt.Errorf("watch the cluster: %w", err)
	}

	state := envstate.New(cl, cs.CoordinationV1().Leases(cfg.Namespace), envstate.Options{
		Idle:             cfg.Idle,
		WakeAfterTimeout: cfg.WakeAfterTimeout,
	})
	state.Bootstrap(ctx)

	// The first server to fail cancels ctx, which stops the others.
	g, ctx := errgroup.WithContext(ctx)

	go cl.Run(ctx, state.Sync)
	go state.Run(ctx)

	g.Go(func() error {
		return serveGRPC(ctx, config.ScalerPort, grpcserver.Limits{MaxMessage: config.ScalerMaxMessage, MaxStreams: config.ScalerMaxStreams},
			func(s *grpc.Server) {
				externalscaler.RegisterExternalScalerServer(s, &scaler.Server{State: state, Stopping: ctx.Done()})
			})
	})
	g.Go(func() error {
		return serveGRPC(ctx, config.OTLPPort, grpcserver.Limits{MaxMessage: config.OTLPMaxMessage, MaxStreams: config.OTLPMaxStreams},
			func(s *grpc.Server) {
				collogs.RegisterLogsServiceServer(s, &otlp.Receiver{Hosts: cl, State: state})
			})
	})
	g.Go(func() error { return serveHTTP(ctx, listenAddr(config.PublicPort), web.Public(state, cl)) })
	g.Go(func() error { return serveHTTP(ctx, listenAddr(config.MetricsPort), web.MetricsHandler(state)) })
	g.Go(func() error { return serveHTTP(ctx, "127.0.0.1"+listenAddr(config.AdminPort), web.AdminHandler(state)) })

	slog.Info("started", "idle", cfg.Idle.String(), "selector", cfg.ScaledObjectSelector,
		"watchNamespaces", cfg.WatchNamespaces, "gateway", cfg.Gateway, "envs", len(cl.EnvNames()))

	return g.Wait()
}

// listenAddr is the address that listens on a port on every interface.
func listenAddr(port int) string {
	return fmt.Sprintf(":%d", port)
}

func serveGRPC(ctx context.Context, port int, limits grpcserver.Limits, register func(*grpc.Server)) error {
	lis, err := net.Listen("tcp", listenAddr(port))
	if err != nil {
		return err
	}

	s := grpcserver.New(limits)
	register(s.Server)

	go func() {
		<-ctx.Done()

		force := time.AfterFunc(stopTimeout, s.Stop)
		s.GracefulStop()
		force.Stop()
	}()

	if err := s.Serve(lis); err != nil {
		return fmt.Errorf("grpc :%d: %w", port, err)
	}

	return nil
}

func serveHTTP(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		<-ctx.Done()

		sctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()

		_ = srv.Shutdown(sctx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http %s: %w", addr, err)
	}

	<-stopped

	return nil
}
