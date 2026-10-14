// Package cluster is the daemon's view of the cluster: the environments it manages, the
// hosts that belong to them, and whether their workloads are Ready.
package cluster

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// WakeAfterAnnotation on a ScaledObject names a Deployment of the same environment that
// must be Ready before the ScaledObject's own target starts.
const WakeAfterAnnotation = "krci.epam.com/wake-after"

// MaxTargets is the most workloads an environment can have. The waiting page shows as
// many (MAX_COMPONENTS in wake.js).
const MaxTargets = 64

// Target is one workload of an environment: the Deployment a ScaledObject scales.
type Target struct {
	ScaledObject string
	Deployment   string
	// After is the Deployment that must be Ready before this one starts, or "".
	After string
}

// Waits reports whether the target starts only after another Deployment is Ready.
func (t Target) Waits() bool {
	return t.After != ""
}

// CompareWaits orders the independent targets before the ones that wait.
func CompareWaits(a, b Target) int {
	return cmp.Compare(boolInt(a.Waits()), boolInt(b.Waits()))
}

func boolInt(b bool) int {
	if b {
		return 1
	}

	return 0
}

// Env is an environment: a namespace with ScaledObjects the daemon drives.
type Env struct {
	Namespace string
	// Targets lists the independent workloads first, then the waiting ones, each group by
	// Deployment name.
	Targets []Target
}

// Has reports whether the ScaledObject is one of the environment's targets.
func (e *Env) Has(scaledObject string) bool {
	return slices.ContainsFunc(e.Targets, func(t Target) bool { return t.ScaledObject == scaledObject })
}

// Topology is what the daemon decides with.
type Topology struct {
	// Envs maps a namespace to its environment.
	Envs map[string]*Env
	// Hosts maps a normalized hostname to the namespace that owns it.
	Hosts map[string]string
	// Warnings describe selected objects the topology ignored.
	Warnings []string
}

type hostClaim struct {
	namespace string
	created   time.Time
	key       string
}

func (c hostClaim) precedes(o hostClaim) bool {
	return cmp.Or(c.created.Compare(o.created), strings.Compare(c.key, o.key)) < 0
}

// Build derives the topology from the selected ScaledObjects and the HTTPRoutes. A route
// counts only when a Gateway accepted it: gateway (namespace/name), or any Gateway when
// gateway is empty. Routes in selfNamespace (the daemon's own waiting-page route) and in
// namespaces without an environment are ignored, and so are wildcard hostnames. A
// hostname that routes of several environments claim goes to the oldest route, then to
// the first by namespace/name: the order in which Gateway API resolves conflicting routes.
func Build(scaledObjects, routes []*unstructured.Unstructured, selfNamespace, gateway string) Topology {
	t := Topology{Envs: map[string]*Env{}, Hosts: map[string]string{}}

	scaledObjects = slices.SortedFunc(slices.Values(scaledObjects), func(a, b *unstructured.Unstructured) int {
		return cmp.Or(strings.Compare(a.GetNamespace(), b.GetNamespace()), strings.Compare(a.GetName(), b.GetName()))
	})

	for _, u := range scaledObjects {
		ns, name := u.GetNamespace(), u.GetName()
		kind, _, _ := unstructured.NestedString(u.Object, "spec", "scaleTargetRef", "kind")
		target, _, _ := unstructured.NestedString(u.Object, "spec", "scaleTargetRef", "name")

		if target == "" || (kind != "" && kind != "Deployment") {
			t.Warnings = append(t.Warnings, fmt.Sprintf("ScaledObject %s/%s: ignored, the scale target is not a Deployment", ns, name))
			continue
		}

		if len(validation.IsDNS1123Subdomain(target)) > 0 {
			t.Warnings = append(t.Warnings, fmt.Sprintf("ScaledObject %s/%s: ignored, the scale target name is not a Deployment name", ns, name))
			continue
		}

		after := u.GetAnnotations()[WakeAfterAnnotation]
		if after != "" && (after == target || len(validation.IsDNS1123Label(after)) > 0) {
			t.Warnings = append(t.Warnings, fmt.Sprintf("ScaledObject %s/%s: ignoring %s=%q", ns, name, WakeAfterAnnotation, after))
			after = ""
		}

		env := t.Envs[ns]
		if env == nil {
			env = &Env{Namespace: ns}
			t.Envs[ns] = env
		}

		if len(env.Targets) == MaxTargets {
			t.Warnings = append(t.Warnings, fmt.Sprintf("ScaledObject %s/%s: ignored, the environment already has %d workloads", ns, name, MaxTargets))
			continue
		}

		env.Targets = append(env.Targets, Target{ScaledObject: name, Deployment: target, After: after})
	}

	for _, e := range t.Envs {
		slices.SortFunc(e.Targets, func(a, b Target) int {
			return cmp.Or(CompareWaits(a, b), strings.Compare(a.Deployment, b.Deployment))
		})
	}

	claims := map[string]hostClaim{}
	conflicts := map[string]bool{}

	for _, u := range routes {
		ns := u.GetNamespace()
		if ns == selfNamespace || t.Envs[ns] == nil {
			continue
		}

		hostnames, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "hostnames")

		if !slices.ContainsFunc(AcceptedBy(u), func(g string) bool { return gateway == "" || g == gateway }) {
			if len(hostnames) > 0 {
				t.Warnings = append(t.Warnings, fmt.Sprintf("HTTPRoute %s/%s: ignored, the Gateway has not accepted it", ns, u.GetName()))
			}

			continue
		}

		c := hostClaim{namespace: ns, created: u.GetCreationTimestamp().Time, key: ns + "/" + u.GetName()}

		for _, h := range hostnames {
			h = NormalizeHost(h)
			if h == "" || strings.HasPrefix(h, "*") {
				continue
			}

			prev, claimed := claims[h]
			if claimed && prev.namespace != ns {
				conflicts[h] = true
			}

			if !claimed || c.precedes(prev) {
				claims[h] = c
			}
		}
	}

	for h, c := range claims {
		t.Hosts[h] = c.namespace
	}

	for h := range conflicts {
		t.Warnings = append(t.Warnings, fmt.Sprintf("hostname %s is claimed by several environments; %s keeps it", h, claims[h].key))
	}

	slices.Sort(t.Warnings)

	return t
}

// AcceptedBy lists the Gateways, as namespace/name, whose status entry on the HTTPRoute
// carries Accepted=True. A parent without a namespace is in the route's namespace.
func AcceptedBy(route *unstructured.Unstructured) []string {
	var gateways []string

	parents, _, _ := unstructured.NestedSlice(route.Object, "status", "parents")

	for _, p := range parents {
		parent, _ := p.(map[string]any)
		group, _, _ := unstructured.NestedString(parent, "parentRef", "group")
		kind, _, _ := unstructured.NestedString(parent, "parentRef", "kind")
		ns, _, _ := unstructured.NestedString(parent, "parentRef", "namespace")
		name, _, _ := unstructured.NestedString(parent, "parentRef", "name")

		if (group != "" && group != "gateway.networking.k8s.io") || (kind != "" && kind != "Gateway") || name == "" {
			continue
		}

		conditions, _, _ := unstructured.NestedSlice(parent, "conditions")
		if !slices.ContainsFunc(conditions, func(c any) bool {
			cond, _ := c.(map[string]any)
			return cond["type"] == "Accepted" && cond["status"] == "True"
		}) {
			continue
		}

		gateways = append(gateways, cmp.Or(ns, route.GetNamespace())+"/"+name)
	}

	return gateways
}

// NormalizeHost lower-cases a host and drops its port and trailing dot.
func NormalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}

	return strings.TrimSuffix(h, ".")
}
