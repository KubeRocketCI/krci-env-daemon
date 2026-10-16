package envstate

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1ac "k8s.io/client-go/applyconfigurations/coordination/v1"
)

const (
	leasePrefix      = "env-"
	leaseHolder      = "krci-env-daemon"
	activeAnnotation = "krci.epam.com/active"
	leaseEvery       = 30 * time.Second
	leaseTimeout     = 10 * time.Second
)

// Bootstrap restores every environment from its Lease and the cluster, so a restart
// neither puts an environment in use to sleep nor wakes one that was put to sleep. It
// deletes the Leases of namespaces that are no longer environments.
func (m *Manager) Bootstrap(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, leaseTimeout)
	defer cancel()

	saved := map[string]*coordinationv1.Lease{}

	list, err := m.leases.List(ctx, metav1.ListOptions{})
	if err != nil {
		slog.Warn("list leases", "error", err)
	} else {
		for i := range list.Items {
			if ns, ok := strings.CutPrefix(list.Items[i].Name, leasePrefix); ok {
				saved[ns] = &list.Items[i]
			}
		}
	}

	m.mu.Lock()

	for _, ns := range m.cluster.EnvNames() {
		m.adopt(ns, saved[ns])
	}

	m.mu.Unlock()

	for ns := range saved {
		if m.cluster.Env(ns) != nil {
			continue
		}

		if err := m.leases.Delete(ctx, leasePrefix+ns, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			slog.Warn("delete lease", "env", ns, "error", err)
		}
	}
}

func (m *Manager) saveLease(ctx context.Context, ns string) {
	m.mu.Lock()

	es := m.envs[ns]
	if es == nil {
		m.mu.Unlock()
		return
	}

	last, active := es.lastSeen, es.active
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, leaseTimeout)
	defer cancel()

	spec := coordinationv1ac.LeaseSpec().
		WithHolderIdentity(leaseHolder).
		WithLeaseDurationSeconds(int32(m.opts.Idle.Seconds()))

	if !last.IsZero() {
		spec.WithRenewTime(metav1.NewMicroTime(last))
	}

	lease := coordinationv1ac.Lease(leasePrefix+ns, "").
		WithAnnotations(map[string]string{activeAnnotation: strconv.FormatBool(active)}).
		WithSpec(spec)

	if _, err := m.leases.Apply(ctx, lease, metav1.ApplyOptions{FieldManager: leaseHolder, Force: true}); err != nil {
		slog.Warn("save lease", "env", ns, "error", err)
		return
	}

	m.mu.Lock()
	if es := m.envs[ns]; es != nil {
		es.saved, es.savedActive = m.now(), active
	}
	m.mu.Unlock()
}
