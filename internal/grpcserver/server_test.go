package grpcserver

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
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

// stubborn is a scaler whose IsActive ignores its context and returns only once done is
// closed.
type stubborn struct {
	externalscaler.UnimplementedExternalScalerServer
	entered, done chan struct{}
}

func (s *stubborn) IsActive(context.Context, *externalscaler.ScaledObjectRef) (*externalscaler.IsActiveResponse, error) {
	s.entered <- struct{}{}
	<-s.done

	return &externalscaler.IsActiveResponse{}, nil
}

// serve starts a server with the limits and svc on an in-memory listener.
func serve(t *testing.T, l Limits, svc externalscaler.ExternalScalerServer) (*Server, *bufconn.Listener) {
	t.Helper()

	s := New(l)
	externalscaler.RegisterExternalScalerServer(s.Server, svc)

	lis := bufconn.Listen(1 << 20)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	return s, lis
}

func dial(t *testing.T, lis *bufconn.Listener) *grpc.ClientConn {
	t.Helper()

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

func TestLimits(t *testing.T) {
	svc := &holding{entered: make(chan struct{}, 8)}
	_, lis := serve(t, Limits{MaxMessage: 64, MaxStreams: 2}, svc)

	ref := &externalscaler.ScaledObjectRef{Namespace: "team-a", Name: "portal"}
	probe := externalscaler.NewExternalScalerClient(dial(t, lis)) // its own connection: the per-connection limit never hides the gate

	_, err := probe.IsActive(t.Context(), ref)
	require.NoError(t, err)

	t.Run("streams in flight", func(t *testing.T) {
		holder := externalscaler.NewExternalScalerClient(dial(t, lis))
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
		conn := dial(t, lis)

		for range 10 {
			err := conn.Invoke(t.Context(), "/nope.Service/Nope", ref, &externalscaler.IsActiveResponse{})
			require.Equal(t, codes.Unimplemented, status.Code(err))
		}

		_, err := probe.IsActive(t.Context(), ref)
		require.NoError(t, err)
	})

	// grpc answers a stream whose deadline has passed on arrival after admit, without an
	// End event.
	t.Run("streams grpc aborts after admission", func(t *testing.T) {
		tr := &http.Transport{Protocols: new(http.Protocols)}
		tr.Protocols.SetUnencryptedHTTP2(true)
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return lis.DialContext(ctx) }
		t.Cleanup(tr.CloseIdleConnections)

		for range 5 {
			req, err := http.NewRequest(http.MethodPost, "http://bufconn/externalscaler.ExternalScaler/IsActive", bytes.NewReader(make([]byte, 5)))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/grpc")
			req.Header.Set("Te", "trailers")
			req.Header.Set("Grpc-Timeout", "0n")

			resp, err := tr.RoundTrip(req)
			require.NoError(t, err)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			require.Equal(t, "4", resp.Header.Get("Grpc-Status")+resp.Trailer.Get("Grpc-Status"), "deadline exceeded, not refused by the gate")
		}
	})

	t.Run("message size", func(t *testing.T) {
		_, err := probe.IsActive(t.Context(), &externalscaler.ScaledObjectRef{Namespace: "team-a", Name: strings.Repeat("x", 100)})
		require.Equal(t, codes.ResourceExhausted, status.Code(err))

		_, err = probe.IsActive(t.Context(), ref)
		require.NoError(t, err)
	})
}

func TestConnectionLimit(t *testing.T) {
	_, lis := serve(t, Limits{MaxMessage: 64, MaxStreams: 2, MaxConns: 1}, &holding{entered: make(chan struct{}, 8)})

	idle, err := lis.Dial() // takes the only connection and never speaks
	require.NoError(t, err)

	ref := &externalscaler.ScaledObjectRef{Namespace: "team-a", Name: "portal"}
	client := externalscaler.NewExternalScalerClient(dial(t, lis))

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	_, err = client.IsActive(ctx, ref)
	require.Equal(t, codes.DeadlineExceeded, status.Code(err), "a connection above the limit waits to be accepted")

	require.NoError(t, idle.Close())
	require.Eventually(t, func() bool {
		_, err := client.IsActive(t.Context(), ref)
		return err == nil
	}, 5*time.Second, 50*time.Millisecond)
}

func TestHandlerPastItsDeadlineKeepsItsSlot(t *testing.T) {
	svc := &stubborn{entered: make(chan struct{}, 1), done: make(chan struct{})}
	s, lis := serve(t, Limits{MaxMessage: 64, MaxStreams: 1}, svc)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	go func() {
		_, _ = externalscaler.NewExternalScalerClient(dial(t, lis)).IsActive(ctx, &externalscaler.ScaledObjectRef{Namespace: "team-a", Name: "portal"})
	}()

	<-svc.entered
	require.Never(t, func() bool { return s.gate.open.Load() == 0 }, 400*time.Millisecond, 20*time.Millisecond, "its handler still runs")

	close(svc.done)
	require.Eventually(t, func() bool { return s.gate.open.Load() == 0 }, time.Second, 10*time.Millisecond)
}

func TestSilentConnectionIsClosed(t *testing.T) {
	defer func(d time.Duration) { handshakeTimeout = d }(handshakeTimeout)
	handshakeTimeout = 200 * time.Millisecond

	_, lis := serve(t, Limits{MaxMessage: 64, MaxStreams: 2, MaxConns: 1}, &holding{entered: make(chan struct{}, 8)})

	silent, err := lis.Dial()
	require.NoError(t, err)
	t.Cleanup(func() { _ = silent.Close() })
	require.NoError(t, silent.SetReadDeadline(time.Now().Add(5*time.Second)))

	start := time.Now()
	_, err = io.ReadAll(silent)
	require.NoError(t, err, "the server closes a connection that never sends the HTTP/2 preface")
	require.Less(t, time.Since(start), 3*time.Second)
}
