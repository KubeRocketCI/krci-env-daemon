package envstate

import (
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
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

const (
	testIdle      = 15 * time.Minute
	testWakeAfter = 2 * time.Minute
)

// fakeCluster is read and written on the test goroutine only: the tests stub out the
// progress goroutine and never call Run.
type fakeCluster struct {
	envs      map[string]*cluster.Env
	ready     map[string]bool // "ns/deployment"
	up        map[string]bool // "ns/deployment"
	missing   map[string]bool // "ns/deployment": no such Deployment
	pipelines map[string]bool
}

func (f *fakeCluster) Env(ns string) *cluster.Env     { return f.envs[ns] }
func (f *fakeCluster) EnvNames() []string             { return slices.Sorted(maps.Keys(f.envs)) }
func (f *fakeCluster) Exists(ns, d string) bool       { return !f.missing[ns+"/"+d] }
func (f *fakeCluster) Ready(ns, d string) bool        { return f.ready[ns+"/"+d] }
func (f *fakeCluster) ScaledUp(ns, d string) bool     { return f.up[ns+"/"+d] }
func (f *fakeCluster) PipelineRunning(ns string) bool { return f.pipelines[ns] }

func (f *fakeCluster) set(m map[string]bool, value bool, keys ...string) {
	for _, k := range keys {
		m[k] = value
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// teamA is an environment whose EventListener waits for the interceptor.
// teamA is an environment of four workloads, el-gitlab waiting for tekton-interceptor, and
// the extra targets.
func teamA(extra ...cluster.Target) *cluster.Env {
	return &cluster.Env{Namespace: "team-a", Targets: append([]cluster.Target{
		{ScaledObject: "codebase-operator", Deployment: "codebase-operator"},
		{ScaledObject: "krci-portal", Deployment: "krci-portal"},
		{ScaledObject: "tekton-interceptor", Deployment: "tekton-interceptor"},
		{ScaledObject: "el-gitlab", Deployment: "el-gitlab", After: "tekton-interceptor"},
	}, extra...)}
}

func newTestManager(t *testing.T, envs ...*cluster.Env) (*Manager, *fakeCluster, *clock) {
	t.Helper()

	fc := &fakeCluster{envs: map[string]*cluster.Env{}, ready: map[string]bool{}, up: map[string]bool{}, missing: map[string]bool{}, pipelines: map[string]bool{}}
	for _, e := range envs {
		fc.envs[e.Namespace] = e
	}

	clk := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	m := New(fc, fake.NewClientset().CoordinationV1().Leases("daemon"), Options{Idle: testIdle, WakeAfterTimeout: testWakeAfter})
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

	clk.advance(testWakeAfter + time.Second)
	require.False(t, m.advance("team-a", es))
	require.True(t, active(m, "team-a", "el-gitlab"))
}

func TestAdvanceStopsForAReplacedState(t *testing.T) {
	m, fc, clk := newTestManager(t, teamA())
	m.Sync()
	m.Touch("team-a", "request 503")
	stale := m.envs["team-a"]

	delete(fc.envs, "team-a")
	m.Sync()
	fc.envs["team-a"] = teamA()
	fc.set(fc.up, true, "team-a/krci-portal")
	m.Sync()

	clk.advance(time.Second)
	fc.set(fc.ready, true, "team-a/krci-portal")
	require.True(t, m.advance("team-a", stale))
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

	running, stopRunning := m.Subscribe("team-a", "krci-portal")
	defer stopRunning()
	require.True(t, signalled(running), "a stream of a running workload starts with its state")

	_, managed := m.IsActive("team-a", "new-component")
	require.False(t, managed, "unknown until the topology has it")

	grown := teamA(cluster.Target{ScaledObject: "new-component", Deployment: "new-component"})
	fc.envs["team-a"] = grown
	m.Sync()

	require.True(t, active(m, "team-a", "new-component"))
	require.True(t, signalled(added), "Sync signals KEDA for a ScaledObject that joined an awake environment")
	require.False(t, signalled(running), "a workload that already runs gets no signal")
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
	clk.advance(testIdle + time.Second)
	m.tick(t.Context())
	require.False(t, active(m, "team-a", "krci-portal"))
}

func TestTickWakesASleepingEnvironmentForAPipeline(t *testing.T) {
	m, fc, _ := newTestManager(t, teamA())
	m.Sync()

	portal, stop := m.Subscribe("team-a", "krci-portal")
	defer stop()

	fc.set(fc.pipelines, true, "team-a")
	m.tick(t.Context())

	require.True(t, active(m, "team-a", "krci-portal"), "a pipeline created through the API wakes the environment")
	require.True(t, signalled(portal))
	require.Equal(t, "pipeline", m.Snapshot()[0].LastWakeReason)
}

func TestMissingDeploymentIsNotWaitedFor(t *testing.T) {
	env := teamA(cluster.Target{ScaledObject: "gerrit", Deployment: "gerrit"})

	m, fc, clk := newTestManager(t, env)
	fc.set(fc.missing, true, "team-a/gerrit")
	m.Sync()
	m.Touch("team-a", "request 503")
	es := m.envs["team-a"]

	clk.advance(4 * time.Second)
	fc.set(fc.ready, true, "team-a/codebase-operator", "team-a/krci-portal", "team-a/tekton-interceptor", "team-a/el-gitlab")
	require.True(t, m.advance("team-a", es), "the wake is over without the Deployment that does not exist")
	require.Equal(t, "4s", m.Snapshot()[0].LastColdStart)
	require.Equal(t, 4, m.Snapshot()[0].Total)

	var names []string
	for _, c := range m.Status("team-a").Components {
		names = append(names, c.Name)
	}

	require.Len(t, names, 4)
	require.NotContains(t, names, "gerrit")
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

	delete(fc.envs, "team-a")
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
	require.Equal(t, (testIdle - 5*time.Minute).String(), views[0].SleepsIn, "the idle window counts from the Lease")
	require.True(t, active(m, "team-a", "el-gitlab"), "an awake environment keeps all of its workloads")
	require.False(t, views[1].Active, "an environment at zero stays asleep")
	require.Equal(t, testIdle.String(), views[2].SleepsIn, "an awake environment without a Lease gets a full idle window")
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

	clk.advance(testIdle + time.Minute)
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

func TestMissingDeploymentDoesNotForceAColdStart(t *testing.T) {
	env := teamA(cluster.Target{ScaledObject: "gerrit", Deployment: "gerrit"})

	m, fc, _ := newTestManager(t, env)
	fc.set(fc.missing, true, "team-a/gerrit")
	m.Sync()

	fc.set(fc.up, true, "team-a/codebase-operator", "team-a/krci-portal", "team-a/tekton-interceptor", "team-a/el-gitlab")
	m.Touch("team-a", "request 200")

	require.Zero(t, m.Snapshot()[0].Wakeups, "every workload that exists still runs: no wake")
	require.True(t, active(m, "team-a", "el-gitlab"))
}

func TestDependentOfAMissingDeploymentStartsAtOnce(t *testing.T) {
	env := teamA(cluster.Target{ScaledObject: "el-gerrit", Deployment: "el-gerrit", After: "gerrit"})

	m, fc, _ := newTestManager(t, env)
	fc.set(fc.missing, true, "team-a/gerrit")
	m.Sync()
	m.Touch("team-a", "request 503")

	require.True(t, active(m, "team-a", "el-gerrit"), "there is no Deployment to wait for")
}

func TestWaitingWorkloadThatIsAlreadyUpIsReleased(t *testing.T) {
	env := teamA(cluster.Target{ScaledObject: "el-gerrit", Deployment: "el-gerrit", After: "gerrit-db"})

	m, fc, clk := newTestManager(t, env)
	m.Sync()
	fc.set(fc.up, true, "team-a/el-gerrit") // still up from before the sleep
	fc.set(fc.ready, true, "team-a/el-gerrit")
	m.Touch("team-a", "request 503")
	es := m.envs["team-a"]

	fc.set(fc.ready, true, "team-a/codebase-operator", "team-a/krci-portal", "team-a/tekton-interceptor", "team-a/el-gitlab")
	require.False(t, m.advance("team-a", es), "el-gerrit still waits for gerrit-db, which never gets Ready")
	require.False(t, active(m, "team-a", "el-gerrit"))

	clk.advance(testWakeAfter + time.Second)
	require.True(t, m.advance("team-a", es))
	require.True(t, active(m, "team-a", "el-gerrit"), "the wait times out and releases it")
}

func TestLeaseWritesAreOneApply(t *testing.T) {
	cs := fake.NewClientset()
	m, _, clk := newTestManager(t, teamA())
	m.leases = cs.CoordinationV1().Leases("daemon")
	m.Sync()
	m.Touch("team-a", "request 503")

	m.saveLease(t.Context(), "team-a")
	clk.advance(time.Minute)
	m.Touch("team-a", "request 200")
	m.saveLease(t.Context(), "team-a")

	var verbs []string
	for _, a := range cs.Actions() {
		verbs = append(verbs, a.GetVerb())
	}

	require.Equal(t, []string{"patch", "patch"}, verbs, "each write is one server-side apply, never a read")

	l, err := cs.CoordinationV1().Leases("daemon").Get(t.Context(), "env-team-a", metav1.GetOptions{})
	require.NoError(t, err)
	require.True(t, l.Spec.RenewTime.Time.Equal(clk.now()))

	require.NoError(t, cs.CoordinationV1().Leases("daemon").Delete(t.Context(), "env-team-a", metav1.DeleteOptions{}))
	m.saveLease(t.Context(), "team-a")

	_, err = cs.CoordinationV1().Leases("daemon").Get(t.Context(), "env-team-a", metav1.GetOptions{})
	require.NoError(t, err, "a Lease deleted behind the daemon's back is created again")
}

func TestScaledObjectThatReturnsDuringAWakeRuns(t *testing.T) {
	m, fc, _ := newTestManager(t, teamA())
	m.Sync()
	m.Touch("team-a", "request 503")
	require.False(t, active(m, "team-a", "el-gitlab"), "it waits for tekton-interceptor")

	shrunk := teamA()
	shrunk.Targets = slices.DeleteFunc(shrunk.Targets, func(t cluster.Target) bool { return t.ScaledObject == "el-gitlab" })
	fc.envs["team-a"] = shrunk
	m.Sync()

	fc.set(fc.ready, true, "team-a/codebase-operator", "team-a/krci-portal", "team-a/tekton-interceptor")
	require.True(t, m.advance("team-a", m.envs["team-a"]), "the wake is over")

	fc.envs["team-a"] = teamA()
	m.Sync()
	require.True(t, active(m, "team-a", "el-gitlab"), "it comes back to an awake environment and runs")
}
