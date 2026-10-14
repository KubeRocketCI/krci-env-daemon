package grpcserver

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/KubeRocketCI/krci-env-daemon/internal/externalscaler"
)

// holding is a scaler whose streams never answer: each one holds a slot until its client
// goes away.
type holding struct {
	externalscaler.UnimplementedExternalScalerServer
	entered chan struct{}
}

func (h *holding) IsActive(context.Context, *externalscaler.ScaledObjectRef) (*externalscaler.IsActiveResponse, error) {
	return &externalscaler.IsActiveResponse{}, nil
}

func (h *holding) StreamIsActive(_ *externalscaler.ScaledObjectRef, stream externalscaler.ExternalScaler_StreamIsActiveServer) error {
	h.entered <- struct{}{}
	<-stream.Context().Done()

	return nil
}

func dial(t *testing.T, lis *bufconn.Listener) externalscaler.ExternalScalerClient {
	t.Helper()

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return externalscaler.NewExternalScalerClient(conn)
}

func TestLimits(t *testing.T) {
	svc := &holding{entered: make(chan struct{}, 8)}
	s := New(Limits{MaxMessage: 64, MaxStreams: 2})
	externalscaler.RegisterExternalScalerServer(s.Server, svc)

	lis := bufconn.Listen(1 << 20)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	ref := &externalscaler.ScaledObjectRef{Namespace: "team-a", Name: "portal"}
	probe := dial(t, lis) // its own connection: the per-connection limit never hides the gate

	_, err := probe.IsActive(t.Context(), ref)
	require.NoError(t, err)

	t.Run("streams in flight", func(t *testing.T) {
		holder := dial(t, lis)
		ctx, cancel := context.WithCancel(t.Context())

		for range 2 {
			_, err := holder.StreamIsActive(ctx, ref)
			require.NoError(t, err)
			<-svc.entered
		}

		_, err := probe.IsActive(t.Context(), ref)
		require.Equal(t, codes.ResourceExhausted, status.Code(err), "the third stream is refused before its request is read")

		cancel()
		require.Eventually(t, func() bool {
			_, err := probe.IsActive(t.Context(), ref)
			return err == nil
		}, 5*time.Second, 10*time.Millisecond, "the slots return when the streams end")
	})

	t.Run("unknown methods hold no slot", func(t *testing.T) {
		conn, err := grpc.NewClient("passthrough:///bufconn",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		for range 10 {
			err := conn.Invoke(t.Context(), "/nope.Service/Nope", ref, &externalscaler.IsActiveResponse{})
			require.Equal(t, codes.Unimplemented, status.Code(err))
		}

		_, err = probe.IsActive(t.Context(), ref)
		require.NoError(t, err)
	})

	t.Run("message size", func(t *testing.T) {
		_, err := probe.IsActive(t.Context(), &externalscaler.ScaledObjectRef{Namespace: "team-a", Name: strings.Repeat("x", 100)})
		require.Equal(t, codes.ResourceExhausted, status.Code(err))

		_, err = probe.IsActive(t.Context(), ref)
		require.NoError(t, err)
	})
}
