// Package envstate decides when an environment is active. It wakes an environment on
// use, starts its workloads in their wake-after order, puts it to sleep after the idle
// window, and keeps its state in a Lease so that a restart changes nothing.
package envstate

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"

	"github.com/KubeRocketCI/krci-env-daemon/internal/cluster"
)

const (
	tickEvery     = 5 * time.Second
	progressEvery = time.Second
	// quietAfterSleep keeps late access-log records from waking an environment that an
	// administrator has just put to sleep.
	quietAfterSleep = 5 * time.Second
)

// Cluster is what the Manager reads from the cluster.
type Cluster interface {
	Env(ns string) *cluster.Env
	EnvNames() []string
	// Exists reports whether the Deployment is in the cluster. A target without one is
	// neither waited for nor shown: a ScaledObject can outlive its workload.
	Exists(ns, deployment string) bool
	Ready(ns, deployment string) bool
	ScaledUp(ns, deployment string) bool
	PipelineRunning(ns string) bool
}

// Options configures a Manager.
type Options struct {
	// Idle is how long an environment stays active after its last use. KEDA scales its
	// workloads to zero a cooldownPeriod after that.
	Idle time.Duration
	// WakeAfterTimeout is the longest wait for a wake-after dependency.
	WakeAfterTimeout time.Duration
}

type envState struct {
	active bool
	// waiting holds the ScaledObjects of this wake whose wake-after dependency is not
	// Ready yet. Every other target of an active environment runs.
	waiting     map[string]bool
	lastSeen    time.Time
	quietUntil  time.Time
	wokeAt      time.Time
	coldStart   time.Duration
	readyAfter  map[string]time.Duration // per Deployment: time to Ready in the last wake
	seenReady   map[string]bool          // per Deployment: Ready already in this wake
	progressing bool
	wakeups     int
	why         string
	saved       time.Time // last Lease write
	savedActive bool
}

func newEnvState() *envState {
	return &envState{waiting: map[string]bool{}, readyAfter: map[string]time.Duration{}, seenReady: map[string]bool{}}
}

type soKey struct{ ns, so string }

// Manager tracks the activity of every environment.
type Manager struct {
	cluster       Cluster
	leases        coordinationv1client.LeaseInterface
	opts          Options
	now           func() time.Time
	startProgress func(ns string, es *envState)

	mu   sync.Mutex
	envs map[string]*envState
	subs map[soKey]map[chan struct{}]struct{}
}

// New returns a Manager that keeps its Leases through leases.
func New(c Cluster, leases coordinationv1client.LeaseInterface, opts Options) *Manager {
	m := &Manager{
		cluster: c,
		leases:  leases,
		opts:    opts,
		now:     time.Now,
		envs:    map[string]*envState{},
		subs:    map[soKey]map[chan struct{}]struct{}{},
	}
	m.startProgress = func(ns string, es *envState) { go m.progress(ns, es) }

	return m
}

// Sync follows a topology change: it adopts new environments, forgets removed ones, and
// signals KEDA for the targets of active environments, so that a ScaledObject added to an
// awake environment starts at once.
func (m *Manager) Sync() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, ns := range m.cluster.EnvNames() {
		m.adopt(ns, nil)
	}

	for ns, es := range m.envs {
		env := m.cluster.Env(ns)
		if env == nil {
			delete(m.envs, ns)
			continue
		}

		if es.active {
			m.push(env, es)
		}
	}
}

// adopt starts tracking an environment in the state the cluster has it in: awake when any
// of its workloads is scaled up, unless its saved Lease says it was put to sleep.
func (m *Manager) adopt(ns string, saved *coordinationv1.Lease) {
	env := m.cluster.Env(ns)
	if env == nil || len(env.Targets) == 0 || m.envs[ns] != nil {
		return
	}

	es := newEnvState()

	if saved != nil && saved.Spec.RenewTime != nil {
		es.lastSeen = saved.Spec.RenewTime.Time
	}

	asleep := saved != nil && saved.Annotations[activeAnnotation] == "false"
	es.active = !asleep && slices.ContainsFunc(env.Targets, func(t cluster.Target) bool {
		return m.cluster.ScaledUp(ns, t.Deployment)
	})

	if es.active && es.lastSeen.IsZero() {
		es.lastSeen = m.now()
	}

	m.envs[ns] = es
	slog.Info("adopted", "env", ns, "active", es.active, "lastSeen", es.lastSeen)
}

// Touch records a use of the environment and wakes it when it sleeps. It does nothing
// for quietAfterSleep after SleepNow.
func (m *Manager) Touch(ns, why string) {
	m.touch(ns, why, false)
}

// WakeNow wakes the environment, also during the quiet window after SleepNow. It reports
// whether the namespace is a managed environment.
func (m *Manager) WakeNow(ns string) bool {
	return m.touch(ns, "admin", true)
}

func (m *Manager) touch(ns, why string, force bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	es, env := m.envs[ns], m.cluster.Env(ns)
	if es == nil || env == nil {
		return false
	}

	now := m.now()
	if !force && now.Before(es.quietUntil) {
		return true
	}

	es.quietUntil = time.Time{}
	es.lastSeen = now

	if es.active {
		return true
	}

	es.active = true
	es.why = why
	clear(es.waiting)

	if m.allScaledUp(env) {
		// KEDA has not scaled it down yet: no cold start, all of it runs.
		slog.Info("keep", "env", ns, "why", why)
		m.push(env, es)

		return true
	}

	for _, t := range env.Targets {
		if t.Waits() {
			es.waiting[t.ScaledObject] = true
		}
	}

	es.wokeAt, es.coldStart = now, 0
	clear(es.seenReady)
	es.wakeups++
	slog.Info("wake", "env", ns, "why", why, "starting", len(env.Targets)-len(es.waiting), "waiting", len(es.waiting))
	m.push(env, es)

	if !es.progressing {
		es.progressing = true
		m.startProgress(ns, es)
	}

	return true
}

func (m *Manager) allScaledUp(env *cluster.Env) bool {
	return !slices.ContainsFunc(env.Targets, func(t cluster.Target) bool {
		return !m.cluster.ScaledUp(env.Namespace, t.Deployment)
	})
}

// push tells KEDA over the open streams that the running ScaledObjects are active. It
// never pushes false: KEDA 2.17 scales the workload to zero on a pushed false even while
// another trigger is active (kedacore/keda#6986, fixed in 2.18.0). Sleep goes through the
// polls instead.
func (m *Manager) push(env *cluster.Env, es *envState) {
	for _, t := range env.Targets {
		if es.waiting[t.ScaledObject] {
			continue
		}

		for ch := range m.subs[soKey{env.Namespace, t.ScaledObject}] {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

func (m *Manager) progress(ns string, es *envState) {
	tick := time.NewTicker(progressEvery)
	defer tick.Stop()

	for range tick.C {
		if m.advance(ns, es) {
			return
		}
	}
}

// advance starts the workloads whose wake-after dependency is Ready or whose wait timed
// out, and records when each workload became Ready. It reports whether the wake of es is
// over; it is also over when es no longer belongs to the namespace.
func (m *Manager) advance(ns string, es *envState) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	env := m.cluster.Env(ns)
	if m.envs[ns] != es || env == nil || !es.active {
		es.progressing = false
		return true
	}

	now := m.now()
	changed, allReady := false, true

	for _, t := range env.Targets {
		ready, released := m.advanceTarget(ns, es, t, now)
		allReady = allReady && ready
		changed = changed || released
	}

	if changed {
		m.push(env, es)
	}

	if !allReady {
		return false
	}

	if es.coldStart == 0 && !es.wokeAt.IsZero() {
		es.coldStart = now.Sub(es.wokeAt)
		slog.Info("awake", "env", ns, "coldStart", es.coldStart.Round(100*time.Millisecond).String())
	}

	es.progressing = false

	return true
}

// advanceTarget records when the target became Ready and releases it from waiting once
// its dependency is Ready or the wait timed out. It reports whether the target is Ready
// and whether it was released. A target without a Deployment never waits and counts as
// Ready: the wake does not depend on it. The caller holds m.mu.
func (m *Manager) advanceTarget(ns string, es *envState, t cluster.Target, now time.Time) (ready, released bool) {
	if !m.cluster.Exists(ns, t.Deployment) {
		delete(es.waiting, t.ScaledObject)
		return true, false
	}

	ready = m.cluster.Ready(ns, t.Deployment)
	if ready && !es.seenReady[t.Deployment] {
		es.seenReady[t.Deployment] = true
		es.readyAfter[t.Deployment] = now.Sub(es.wokeAt)
	}

	if es.waiting[t.ScaledObject] && (m.cluster.Ready(ns, t.After) || now.Sub(es.wokeAt) > m.opts.WakeAfterTimeout) {
		delete(es.waiting, t.ScaledObject)
		slog.Info("release", "env", ns, "target", t.Deployment, "after", t.After)

		return ready, true
	}

	return ready, false
}

// IsActive reports whether the ScaledObject's workload should run: its environment is in
// use and the workload does not wait for a dependency. managed is false for a
// ScaledObject that is not a target of a managed environment.
func (m *Manager) IsActive(ns, so string) (active, managed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.isActive(ns, so)
}

// isActive is IsActive for a caller that holds m.mu.
func (m *Manager) isActive(ns, so string) (active, managed bool) {
	es, env := m.envs[ns], m.cluster.Env(ns)
	if es == nil || env == nil || !env.Has(so) {
		return false, false
	}

	return es.active && !es.waiting[so], true
}

// Subscribe returns a channel that receives a signal whenever the ScaledObject becomes
// active, at once if it already is, and a function that ends the subscription.
func (m *Manager) Subscribe(ns, so string) (<-chan struct{}, func()) {
	ch, key := make(chan struct{}, 1), soKey{ns, so}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.subs[key] == nil {
		m.subs[key] = map[chan struct{}]struct{}{}
	}

	m.subs[key][ch] = struct{}{}

	if active, _ := m.isActive(ns, so); active {
		ch <- struct{}{}
	}

	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		delete(m.subs[key], ch)

		if len(m.subs[key]) == 0 {
			delete(m.subs, key)
		}
	}
}

// Run counts running pipelines as use (a pipeline created through the API wakes a sleeping
// environment), puts idle environments to sleep and saves their state into Leases, until
// ctx ends.
func (m *Manager) Run(ctx context.Context) {
	tick := time.NewTicker(tickEvery)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		m.tick(ctx)
	}
}

func (m *Manager) tick(ctx context.Context) {
	now := m.now()

	var wakes, saves []string

	m.mu.Lock()

	for ns, es := range m.envs {
		wake, save := m.tickEnv(ns, es, now)

		if wake {
			wakes = append(wakes, ns)
		}

		if wake || save {
			saves = append(saves, ns)
		}
	}

	m.mu.Unlock()

	for _, ns := range wakes {
		m.touch(ns, "pipeline", false)
	}

	for _, ns := range saves {
		m.saveLease(ctx, ns)
	}
}

// tickEnv applies one tick to an environment: a running pipeline keeps it awake, the idle
// window puts it to sleep. It reports whether a pipeline should wake the sleeping
// environment and whether its Lease is due. The caller holds m.mu.
func (m *Manager) tickEnv(ns string, es *envState, now time.Time) (wake, save bool) {
	running := m.cluster.PipelineRunning(ns)
	if running && es.active {
		es.lastSeen = now
	}

	if es.active && now.Sub(es.lastSeen) > m.opts.Idle {
		es.active = false
		slog.Info("sleep", "env", ns, "idleFor", now.Sub(es.lastSeen).Round(time.Second).String())
	}

	wake = running && !es.active
	save = es.active != es.savedActive || (es.active && now.Sub(es.saved) > leaseEvery)

	return wake, save
}

// SleepNow puts an environment to sleep at once; KEDA scales its workloads to zero after
// the ScaledObjects' cooldownPeriod. It reports whether the namespace is a managed
// environment.
func (m *Manager) SleepNow(ns string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	es := m.envs[ns]
	if es == nil {
		return false
	}

	es.active = false
	es.quietUntil = m.now().Add(quietAfterSleep)
	slog.Info("sleep", "env", ns, "why", "admin")

	return true
}
