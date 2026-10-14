package envstate

import (
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/KubeRocketCI/krci-env-daemon/internal/cluster"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

type fakeCluster struct {
	mu        sync.Mutex
	envs      map[string]*cluster.Env
	ready     map[string]bool // "ns/deployment"
	up        map[string]bool // "ns/deployment"
	pipelines map[string]bool
}

func (f *fakeCluster) Env(ns string) *cluster.Env {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.envs[ns]
}

func (f *fakeCluster) EnvNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Sorted(maps.Keys(f.envs))
}

func (f *fakeCluster) Ready(ns, d string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.ready[ns+"/"+d]
}

func (f *fakeCluster) ScaledUp(ns, d string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.up[ns+"/"+d]
}

func (f *fakeCluster) PipelineRunning(ns string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.pipelines[ns]
}

func (f *fakeCluster) set(m map[string]bool, value bool, keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, k := range keys {
		m[k] = value
	}
}

func (f *fakeCluster) setEnv(ns string, env *cluster.Env) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if env == nil {
		delete(f.envs, ns)
		return
	}

	f.envs[ns] = env
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// teamA is an environment whose EventListener waits for the interceptor.
func teamA() *cluster.Env {
	return &cluster.Env{Namespace: "team-a", Targets: []cluster.Target{
		{ScaledObject: "codebase-operator", Deployment: "codebase-operator"},
		{ScaledObject: "krci-portal", Deployment: "krci-portal"},
		{ScaledObject: "tekton-interceptor", Deployment: "tekton-interceptor"},
		{ScaledObject: "el-gitlab", Deployment: "el-gitlab", After: "tekton-interceptor"},
	}}
}

func newTestManager(t *testing.T, envs ...*cluster.Env) (*Manager, *fakeCluster, *clock) {
	t.Helper()

	fc := &fakeCluster{envs: map[string]*cluster.Env{}, ready: map[string]bool{}, up: map[string]bool{}, pipelines: map[string]bool{}}
	for _, e := range envs {
		fc.envs[e.Namespace] = e
	}

	clk := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	m := New(fc, fake.NewClientset().CoordinationV1().Leases("daemon"), Options{Idle: 15 * time.Minute, WakeAfterTimeout: 2 * time.Minute})
	m.now = clk.now
	m.startProgress = func(string, *envState) {}

	return m, fc, clk
}

// signalled reports whether a signal is waiting on ch. Pushes happen synchronously, so
// nothing needs to be awaited.
func signalled(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func active(m *Manager, ns, so string) bool {
	a, _ := m.IsActive(ns, so)
	return a
}

func TestTouchWakesIndependentWorkloadsFirst(t *testing.T) {
	m, _, _ := newTestManager(t, teamA())
	m.Sync()

	portal, stopPortal := m.Subscribe("team-a", "krci-portal")
	defer stopPortal()

	listener, stopListener := m.Subscribe("team-a", "el-gitlab")
	defer stopListener()

	m.Touch("team-a", "request 503")

	require.True(t, active(m, "team-a", "krci-portal"))
	require.False(t, active(m, "team-a", "el-gitlab"))
	require.True(t, signalled(portal))
	require.False(t, signalled(listener))
}

func TestAdvanceReleasesWhenDependencyIsReady(t *testing.T) {
	m, fc, clk := newTestManager(t, teamA())
	m.Sync()

	listener, stop := m.Subscribe("team-a", "el-gitlab")
	defer stop()

	m.Touch("team-a", "request 503")
	es := m.envs["team-a"]

	clk.advance(3 * time.Second)
	fc.set(fc.ready, true, "team-a/tekton-interceptor", "team-a/codebase-operator")
	require.False(t, m.advance("team-a", es))
	require.True(t, active(m, "team-a", "el-gitlab"))
	require.True(t, signalled(listener))

	clk.advance(5 * time.Second)
	fc.set(fc.ready, true, "team-a/krci-portal", "team-a/el-gitlab")
	require.True(t, m.advance("team-a", es))
	require.Equal(t, "8s", m.Snapshot()[0].LastColdStart)
}

func TestAdvanceReleasesAfterTimeout(t *testing.T) {
	m, _, clk := newTestManager(t, teamA())
	m.Sync()
	m.Touch("team-a", "request 503")
	es := m.envs["team-a"]

	require.False(t, m.advance("team-a", es))
	require.False(t, active(m, "team-a", "el-gitlab"))

	clk.advance(2*time.Minute + time.Second)
	require.False(t, m.advance("team-a", es))
	require.True(t, active(m, "team-a", "el-gitlab"))
}

func TestAdvanceStopsForAReplacedState(t *testing.T) {
	m, fc, clk := newTestManager(t, teamA())
	m.Sync()
	m.Touch("team-a", "request 503")
	stale := m.envs["team-a"]

	fc.setEnv("team-a", nil)
	m.Sync()
	fc.setEnv("team-a", teamA())
	fc.set(fc.up, true, "team-a/krci-portal")
	m.Sync()

	clk.advance(time.Second)
	fc.set(fc.ready, true, "team-a/krci-portal")
	require.NotPanics(t, func() { require.True(t, m.advance("team-a", stale)) })
	require.True(t, active(m, "team-a", "krci-portal"), "the re-adopted state is untouched")
}

func TestTouchKeepsAnEnvironmentThatIsStillUp(t *testing.T) {
	m, fc, _ := newTestManager(t, teamA())
	m.Sync()

	fc.set(fc.up, true, "team-a/codebase-operator", "team-a/krci-portal", "team-a/tekton-interceptor", "team-a/el-gitlab")
	m.Touch("team-a", "request 200")

	require.True(t, active(m, "team-a", "el-gitlab"))
	require.Zero(t, m.Snapshot()[0].Wakeups)
}

func TestScaledObjectAddedToAnAwakeEnvironmentRuns(t *testing.T) {
	m, fc, _ := newTestManager(t, teamA())
	fc.set(fc.up, true, "team-a/krci-portal")
	m.Sync()
	require.True(t, active(m, "team-a", "krci-portal"))

	added, stop := m.Subscribe("team-a", "new-component")
	defer stop()

	_, managed := m.IsActive("team-a", "new-component")
	require.False(t, managed, "unknown until the topology has it")

	grown := teamA()
	grown.Targets = append(grown.Targets, cluster.Target{ScaledObject: "new-component", Deployment: "new-component"})
	fc.setEnv("team-a", grown)
	m.Sync()

	require.True(t, active(m, "team-a", "new-component"))
	require.True(t, signalled(added), "Sync signals KEDA for an awake environment")
}

func TestTickPutsIdleEnvironmentToSleep(t *testing.T) {
	m, fc, clk := newTestManager(t, teamA())
	m.Sync()
	m.Touch("team-a", "request 503")

	clk.advance(10 * time.Minute)
	fc.set(fc.pipelines, true, "team-a")
	m.tick(t.Context())
	require.True(t, active(m, "team-a", "krci-portal"), "a running pipeline counts as use")

	fc.set(fc.pipelines, false, "team-a")
	clk.advance(15*time.Minute + time.Second)
	m.tick(t.Context())
	require.False(t, active(m, "team-a", "krci-portal"))
}

func TestSleepNowIgnoresLateRequests(t *testing.T) {
	m, _, clk := newTestManager(t, teamA())
	m.Sync()
	m.Touch("team-a", "request 503")

	require.True(t, m.SleepNow("team-a"))
	m.Touch("team-a", "request 503")
	require.False(t, active(m, "team-a", "krci-portal"))

	clk.advance(quietAfterSleep + time.Second)
	m.Touch("team-a", "request 503")
	require.True(t, active(m, "team-a", "krci-portal"))
	require.False(t, m.SleepNow("unknown"))
}

func TestWakeNowIgnoresTheQuietWindow(t *testing.T) {
	m, _, _ := newTestManager(t, teamA())
	m.Sync()
	m.Touch("team-a", "request 503")
	require.True(t, m.SleepNow("team-a"))

	require.True(t, m.WakeNow("team-a"))
	require.True(t, active(m, "team-a", "krci-portal"))
	require.False(t, m.WakeNow("unknown"))
}

func TestUnknownEnvironmentIsIgnored(t *testing.T) {
	m, _, _ := newTestManager(t, teamA())
	m.Sync()

	m.Touch("unknown", "request 503")
	m.Touch("", "request 503")

	a, managed := m.IsActive("unknown", "krci-portal")
	require.False(t, a)
	require.False(t, managed)

	_, managed = m.IsActive("team-a", "not-a-target")
	require.False(t, managed)
	require.Equal(t, StatusDoc{Components: []Component{}}, m.Status("unknown"))
}

func TestSyncFollowsTopology(t *testing.T) {
	m, fc, _ := newTestManager(t, teamA())
	m.Sync()

	_, managed := m.IsActive("team-a", "krci-portal")
	require.True(t, managed)

	fc.setEnv("team-a", nil)
	m.Sync()
	require.Empty(t, m.Snapshot())
}

func lease(t *testing.T, m *Manager, ns string, renew time.Time, active string) {
	t.Helper()

	_, err := m.leases.Create(t.Context(), &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "env-" + ns, Annotations: map[string]string{activeAnnotation: active}},
		Spec:       coordinationv1.LeaseSpec{RenewTime: new(metav1.NewMicroTime(renew))},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func TestBootstrapRestoresTheSavedState(t *testing.T) {
	teamB := &cluster.Env{Namespace: "team-b", Targets: []cluster.Target{{ScaledObject: "krci-portal", Deployment: "krci-portal"}}}
	teamC := &cluster.Env{Namespace: "team-c", Targets: []cluster.Target{{ScaledObject: "krci-portal", Deployment: "krci-portal"}}}
	teamD := &cluster.Env{Namespace: "team-d", Targets: []cluster.Target{{ScaledObject: "krci-portal", Deployment: "krci-portal"}}}
	m, fc, clk := newTestManager(t, teamA(), teamB, teamC, teamD)

	lease(t, m, "team-a", clk.now().Add(-5*time.Minute), "true")
	lease(t, m, "team-d", clk.now().Add(-time.Minute), "false")
	lease(t, m, "gone", clk.now(), "true")
	fc.set(fc.up, true, "team-a/krci-portal", "team-c/krci-portal", "team-d/krci-portal")

	m.Bootstrap(t.Context())

	views := m.Snapshot()
	require.Len(t, views, 4)
	require.True(t, views[0].Active)
	require.Equal(t, "10m0s", views[0].SleepsIn, "the idle window counts from the Lease")
	require.True(t, active(m, "team-a", "el-gitlab"), "an awake environment keeps all of its workloads")
	require.False(t, views[1].Active, "an environment at zero stays asleep")
	require.Equal(t, "15m0s", views[2].SleepsIn, "an awake environment without a Lease gets a full idle window")
	require.False(t, views[3].Active, "a sleep the Lease recorded survives a restart before KEDA scales down")

	_, err := m.leases.Get(t.Context(), "env-gone", metav1.GetOptions{})
	require.Error(t, err, "the Lease of a removed environment is deleted")
}

func TestRestartKeepsAnAdminSleep(t *testing.T) {
	m, fc, clk := newTestManager(t, teamA())
	m.Sync()
	m.Touch("team-a", "request 503")
	fc.set(fc.up, true, "team-a/codebase-operator", "team-a/krci-portal", "team-a/tekton-interceptor")
	m.tick(t.Context())

	require.True(t, m.SleepNow("team-a"))
	m.tick(t.Context())

	restarted := New(fc, m.leases, m.opts)
	restarted.now = clk.now
	restarted.startProgress = func(string, *envState) {}
	restarted.Bootstrap(t.Context())

	require.False(t, active(restarted, "team-a", "krci-portal"))
}

func TestTickSavesLeases(t *testing.T) {
	m, _, clk := newTestManager(t, teamA(), &cluster.Env{Namespace: "team-b", Targets: []cluster.Target{{ScaledObject: "p", Deployment: "p"}}})
	m.Sync()
	m.Touch("team-a", "request 503")
	m.tick(t.Context())

	l, err := m.leases.Get(t.Context(), "env-team-a", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "true", l.Annotations[activeAnnotation])
	require.True(t, l.Spec.RenewTime.Time.Equal(clk.now()))

	_, err = m.leases.Get(t.Context(), "env-team-b", metav1.GetOptions{})
	require.Error(t, err, "an environment that never woke gets no Lease")

	clk.advance(16 * time.Minute)
	m.tick(t.Context())

	l, err = m.leases.Get(t.Context(), "env-team-a", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "false", l.Annotations[activeAnnotation])
}

func TestStatusFollowsTheLastWake(t *testing.T) {
	m, fc, clk := newTestManager(t, teamA())
	m.Sync()
	m.Touch("team-a", "request 503")
	es := m.envs["team-a"]

	clk.advance(2 * time.Second)
	fc.set(fc.ready, true, "team-a/krci-portal", "team-a/tekton-interceptor")
	m.advance("team-a", es)

	clk.advance(4 * time.Second)
	fc.set(fc.ready, true, "team-a/codebase-operator")
	m.advance("team-a", es)

	require.Equal(t, StatusDoc{Components: []Component{
		{Name: "krci-portal", Ready: true},
		{Name: "tekton-interceptor", Ready: true},
		{Name: "codebase-operator", Ready: true},
		{Name: "el-gitlab"},
	}}, m.Status("team-a"))
}
