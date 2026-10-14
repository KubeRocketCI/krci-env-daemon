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

	spec := coordinationv1.LeaseSpec{
		HolderIdentity:       new(leaseHolder),
		LeaseDurationSeconds: new(int32(m.opts.Idle.Seconds())),
	}

	if !last.IsZero() {
		spec.RenewTime = new(metav1.NewMicroTime(last))
	}

	name := leasePrefix + ns

	l, err := m.leases.Get(ctx, name, metav1.GetOptions{})

	switch {
	case apierrors.IsNotFound(err):
		l = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
		metav1.SetMetaDataAnnotation(&l.ObjectMeta, activeAnnotation, strconv.FormatBool(active))
		_, err = m.leases.Create(ctx, l, metav1.CreateOptions{})
	case err == nil:
		metav1.SetMetaDataAnnotation(&l.ObjectMeta, activeAnnotation, strconv.FormatBool(active))
		l.Spec = spec
		_, err = m.leases.Update(ctx, l, metav1.UpdateOptions{})
	}

	if err != nil {
		slog.Warn("save lease", "env", ns, "error", err)
		return
	}

	m.mu.Lock()
	if es := m.envs[ns]; es != nil {
		es.saved, es.savedActive = m.now(), active
	}
	m.mu.Unlock()
}
