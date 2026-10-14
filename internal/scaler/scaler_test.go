package scaler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/KubeRocketCI/krci-env-daemon/internal/externalscaler"
)

type fakeState struct {
	mu      sync.Mutex
	active  map[string]bool
	managed map[string]bool
	subs    map[string]chan struct{}
}

func (f *fakeState) IsActive(ns, so string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.active[ns+"/"+so], f.managed[ns+"/"+so]
}

func (f *fakeState) Subscribe(ns, so string) (<-chan struct{}, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()

	ch := make(chan struct{}, 1)
	f.subs[ns+"/"+so] = ch

	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()

		delete(f.subs, ns+"/"+so)
	}
}

func (f *fakeState) sub(key string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.subs[key]
}

// fakeStream records what the server sends. Only the methods StreamIsActive calls are
// implemented.
type fakeStream struct {
	grpc.ServerStream
	ctx     context.Context
	sent    chan bool
	sendErr error
}

func (f *fakeStream) Context() context.Context { return f.ctx }

func (f *fakeStream) Send(r *externalscaler.IsActiveResponse) error {
	if f.sendErr != nil {
		return f.sendErr
	}

	f.sent <- r.GetResult()

	return nil
}

func ref(so string) *externalscaler.ScaledObjectRef {
	return &externalscaler.ScaledObjectRef{Namespace: "team-a", Name: so}
}

func newState() *fakeState {
	return &fakeState{
		active:  map[string]bool{"team-a/portal": true},
		managed: map[string]bool{"team-a/portal": true, "team-a/idle": true},
		subs:    map[string]chan struct{}{},
	}
}

func TestPolls(t *testing.T) {
	s := &Server{State: newState()}

	active, err := s.IsActive(t.Context(), ref("portal"))
	require.NoError(t, err)
	require.True(t, active.GetResult())

	idle, err := s.IsActive(t.Context(), ref("idle"))
	require.NoError(t, err)
	require.False(t, idle.GetResult())

	spec, err := s.GetMetricSpec(t.Context(), ref("portal"))
	require.NoError(t, err)
	require.Equal(t, metricName, spec.GetMetricSpecs()[0].GetMetricName())
	require.Equal(t, int64(1), spec.GetMetricSpecs()[0].GetTargetSize())

	for so, want := range map[string]int64{"portal": 1, "idle": 0} {
		m, err := s.GetMetrics(t.Context(), &externalscaler.GetMetricsRequest{ScaledObjectRef: ref(so), MetricName: metricName})
		require.NoError(t, err)
		require.Equal(t, want, m.GetMetricValues()[0].GetMetricValue(), so)
	}
}

func TestUnmanagedScaledObjectIsAnError(t *testing.T) {
	s := &Server{State: newState()}

	_, err := s.IsActive(t.Context(), ref("unknown"))
	require.Equal(t, codes.NotFound, status.Code(err))

	_, err = s.GetMetrics(t.Context(), &externalscaler.GetMetricsRequest{ScaledObjectRef: ref("unknown")})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestInvalidReferenceIsRefused(t *testing.T) {
	st := newState()
	s := &Server{State: st}

	for name, bad := range map[string]*externalscaler.ScaledObjectRef{
		"empty":          {},
		"no namespace":   {Name: "portal"},
		"bad namespace":  {Namespace: "Team_A", Name: "portal"},
		"long name":      {Namespace: "team-a", Name: strings.Repeat("x", 254)},
		"name with path": {Namespace: "team-a", Name: "portal/../admin"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.IsActive(t.Context(), bad)
			require.Equal(t, codes.InvalidArgument, status.Code(err))

			_, err = s.GetMetrics(t.Context(), &externalscaler.GetMetricsRequest{ScaledObjectRef: bad})
			require.Equal(t, codes.InvalidArgument, status.Code(err))

			err = s.StreamIsActive(bad, &fakeStream{ctx: t.Context()})
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			require.Empty(t, st.subs, "nothing is stored under a reference KEDA could not have sent")
		})
	}
}

func TestStreamPushesActivation(t *testing.T) {
	st := newState()
	s := &Server{State: st}

	ctx, cancel := context.WithCancel(t.Context())
	stream := &fakeStream{ctx: ctx, sent: make(chan bool, 1)}

	done := make(chan error, 1)
	go func() { done <- s.StreamIsActive(ref("portal"), stream) }()

	require.Eventually(t, func() bool { return st.sub("team-a/portal") != nil }, time.Second, 5*time.Millisecond)
	st.sub("team-a/portal") <- struct{}{}
	require.True(t, <-stream.sent)

	cancel()
	require.NoError(t, <-done)
	require.Nil(t, st.sub("team-a/portal"), "the stream unsubscribes when KEDA goes away")
}

func TestStreamEnds(t *testing.T) {
	t.Run("on shutdown", func(t *testing.T) {
		stopping := make(chan struct{})
		s := &Server{State: newState(), Stopping: stopping}

		done := make(chan error, 1)
		go func() { done <- s.StreamIsActive(ref("portal"), &fakeStream{ctx: t.Context()}) }()

		close(stopping)
		require.Equal(t, codes.Unavailable, status.Code(<-done))
	})

	t.Run("on a failed send", func(t *testing.T) {
		st := newState()
		s := &Server{State: st}
		boom := errors.New("connection lost")

		done := make(chan error, 1)
		go func() { done <- s.StreamIsActive(ref("portal"), &fakeStream{ctx: t.Context(), sendErr: boom}) }()

		require.Eventually(t, func() bool { return st.sub("team-a/portal") != nil }, time.Second, 5*time.Millisecond)
		st.sub("team-a/portal") <- struct{}{}
		require.ErrorIs(t, <-done, boom)
	})
}
