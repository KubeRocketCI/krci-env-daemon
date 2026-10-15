package envstate

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/KubeRocketCI/krci-env-daemon/internal/cluster"
)

// Component is one workload as the waiting page shows it.
type Component struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
}

// StatusDoc is everything the public waiting page learns about an environment: component
// names and which of them are Ready. No namespace, pod, image, IP or error.
type StatusDoc struct {
	Components []Component `json:"components"`
}

// Status lists the components in the order they are expected to come up: the ones that
// wait for another last, the rest by how fast they became Ready in the last wake.
func (m *Manager) Status(ns string) StatusDoc {
	doc := StatusDoc{Components: []Component{}}

	env := m.cluster.Env(ns)
	if env == nil {
		return doc
	}

	var took map[string]time.Duration

	m.mu.Lock()
	if es := m.envs[ns]; es != nil {
		took = maps.Clone(es.readyAfter)
	}
	m.mu.Unlock()

	// A component that was not Ready in the last wake sorts after every one that was.
	rank := func(t cluster.Target) time.Duration {
		if d, ok := took[t.Deployment]; ok {
			return d
		}

		return math.MaxInt64
	}

	order := slices.Clone(env.Targets)
	slices.SortStableFunc(order, func(a, b cluster.Target) int {
		return cmp.Or(cluster.CompareWaits(a, b), cmp.Compare(rank(a), rank(b)))
	})

	for _, t := range order {
		doc.Components = append(doc.Components, Component{Name: t.Deployment, Ready: m.cluster.Ready(ns, t.Deployment)})
	}

	return doc
}

// EnvView is an environment as the admin API and the metrics show it.
type EnvView struct {
	Env              string   `json:"env"`
	Active           bool     `json:"active"`
	Ready            int      `json:"ready"`
	Total            int      `json:"total"`
	Waiting          []string `json:"waitingFor,omitempty"`
	LastSeen         string   `json:"lastSeen,omitempty"`
	SleepsIn         string   `json:"sleepsIn,omitempty"`
	PipelinesRunning bool     `json:"pipelinesRunning"`
	Wakeups          int      `json:"wakeups"`
	LastWakeReason   string   `json:"lastWakeReason,omitempty"`
	LastColdStart    string   `json:"lastColdStart,omitempty"`
	LastSeenUnix     float64  `json:"-"`
	ColdStartSeconds float64  `json:"-"`
}

// Snapshot describes every environment, sorted by namespace.
func (m *Manager) Snapshot() []EnvView {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	out := []EnvView{}

	for _, ns := range m.cluster.EnvNames() {
		es, env := m.envs[ns], m.cluster.Env(ns)
		if es == nil || env == nil {
			continue
		}

		out = append(out, m.view(ns, es, env, now))
	}

	return out
}

// view describes one environment. The caller holds m.mu.
func (m *Manager) view(ns string, es *envState, env *cluster.Env, now time.Time) EnvView {
	v := EnvView{
		Env:              ns,
		Active:           es.active,
		Total:            len(env.Targets),
		PipelinesRunning: m.cluster.PipelineRunning(ns),
		Wakeups:          es.wakeups,
		LastWakeReason:   es.why,
	}

	for _, t := range env.Targets {
		if m.cluster.Ready(ns, t.Deployment) {
			v.Ready++
		}

		if es.active && es.waiting[t.ScaledObject] {
			v.Waiting = append(v.Waiting, t.Deployment+" (after "+t.After+")")
		}
	}

	if !es.lastSeen.IsZero() {
		v.LastSeen = es.lastSeen.UTC().Format(time.RFC3339)
		v.LastSeenUnix = float64(es.lastSeen.UnixMilli()) / 1000

		if es.active {
			v.SleepsIn = (m.opts.Idle - now.Sub(es.lastSeen)).Round(time.Second).String()
		}
	}

	if es.coldStart > 0 {
		v.LastColdStart = es.coldStart.Round(100 * time.Millisecond).String()
		v.ColdStartSeconds = es.coldStart.Seconds()
	}

	return v
}
