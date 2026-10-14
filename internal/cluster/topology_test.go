package cluster

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const testGateway = "envoy-gateway-system/main-gateway"

var created = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func scaledObject(ns, name, target, after string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "keda.sh/v1alpha1",
		"kind":       "ScaledObject",
		"spec":       map[string]any{"scaleTargetRef": map[string]any{"name": target}},
	}}
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetLabels(map[string]string{"krci.epam.com/env-daemon": "enabled"})

	if after != "" {
		u.SetAnnotations(map[string]string{WakeAfterAnnotation: after})
	}

	return u
}

// route returns an HTTPRoute that testGateway accepted, as the Gateway writes it: the
// parentRef without a group or kind, next to the other conditions.
func route(ns, name string, at time.Time, hostnames ...string) *unstructured.Unstructured {
	u := unaccepted(ns, name, at, hostnames...)
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{
		"parentRef":      map[string]any{"namespace": "envoy-gateway-system", "name": "main-gateway"},
		"controllerName": "gateway.envoyproxy.io/gatewayclass-controller",
		"conditions": []any{
			map[string]any{"type": "Accepted", "status": "True", "reason": "Accepted"},
			map[string]any{"type": "ResolvedRefs", "status": "True", "reason": "ResolvedRefs"},
		},
	}}, "status", "parents")

	return u
}

// patchParent changes one entry of the route's status.parents in place.
func patchParent(u *unstructured.Unstructured, i int, patch func(parent map[string]any)) {
	parents, _, _ := unstructured.NestedSlice(u.Object, "status", "parents")
	parent, _ := parents[i].(map[string]any)
	patch(parent)
	parents[i] = parent
	_ = unstructured.SetNestedSlice(u.Object, parents, "status", "parents")
}

func unaccepted(ns, name string, at time.Time, hostnames ...string) *unstructured.Unstructured {
	hs := make([]any, 0, len(hostnames))
	for _, h := range hostnames {
		hs = append(hs, h)
	}

	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"spec":       map[string]any{"hostnames": hs},
	}}
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetCreationTimestamp(metav1.NewTime(at))

	return u
}

func TestBuildOrdersTargets(t *testing.T) {
	topo := Build([]*unstructured.Unstructured{
		scaledObject("team-a", "el-gitlab", "el-gitlab", "tekton-interceptor"),
		scaledObject("team-a", "portal", "krci-portal", ""),
		scaledObject("team-a", "tekton-interceptor", "tekton-interceptor", ""),
		scaledObject("team-a", "codebase-operator", "codebase-operator", ""),
	}, nil, "daemon", testGateway)

	require.Equal(t, []Target{
		{ScaledObject: "codebase-operator", Deployment: "codebase-operator"},
		{ScaledObject: "portal", Deployment: "krci-portal"},
		{ScaledObject: "tekton-interceptor", Deployment: "tekton-interceptor"},
		{ScaledObject: "el-gitlab", Deployment: "el-gitlab", After: "tekton-interceptor"},
	}, topo.Envs["team-a"].Targets)
	require.Empty(t, topo.Warnings)
}

func TestBuildSkipsOtherTargets(t *testing.T) {
	sts := scaledObject("team-a", "db", "db", "")
	require.NoError(t, unstructured.SetNestedField(sts.Object, "StatefulSet", "spec", "scaleTargetRef", "kind"))

	topo := Build([]*unstructured.Unstructured{
		sts,
		scaledObject("team-a", "empty", "", ""),
		scaledObject("team-a", "odd", "Not a name", ""),
	}, nil, "daemon", testGateway)

	require.Empty(t, topo.Envs)
	require.Equal(t, []string{
		"ScaledObject team-a/db: ignored, the scale target is not a Deployment",
		"ScaledObject team-a/empty: ignored, the scale target is not a Deployment",
		"ScaledObject team-a/odd: ignored, the scale target name is not a Deployment name",
	}, topo.Warnings)
}

func TestBuildCapsTargets(t *testing.T) {
	var sos []*unstructured.Unstructured
	for i := range MaxTargets + 2 {
		name := fmt.Sprintf("app-%03d", i)
		sos = append(sos, scaledObject("team-a", name, name, ""))
	}

	topo := Build(sos, nil, "daemon", testGateway)
	require.Len(t, topo.Envs["team-a"].Targets, MaxTargets)
	require.Equal(t, []string{
		"ScaledObject team-a/app-064: ignored, the environment already has 64 workloads",
		"ScaledObject team-a/app-065: ignored, the environment already has 64 workloads",
	}, topo.Warnings, "the first ones by name stay, whatever the cache order")
}

func TestEnvHas(t *testing.T) {
	env := &Env{Namespace: "team-a", Targets: []Target{{ScaledObject: "portal", Deployment: "krci-portal"}}}

	require.True(t, env.Has("portal"))
	require.False(t, env.Has("krci-portal"), "the ScaledObject name counts, not the Deployment's")
}

func TestBuildIgnoresInvalidWakeAfter(t *testing.T) {
	topo := Build([]*unstructured.Unstructured{
		scaledObject("team-a", "a", "a", "Not_A_Name"),
		scaledObject("team-a", "b", "b", "b"),
	}, nil, "daemon", testGateway)

	for _, target := range topo.Envs["team-a"].Targets {
		require.Empty(t, target.After)
	}

	require.Len(t, topo.Warnings, 2)
}

func TestBuildMapsHosts(t *testing.T) {
	topo := Build(
		[]*unstructured.Unstructured{scaledObject("team-a", "portal", "krci-portal", "")},
		[]*unstructured.Unstructured{
			route("team-a", "portal", created, "Portal.Team-A.example.com.", "*.team-a.example.com"),
			route("daemon", "waiting-page", created, "portal.team-a.example.com", "own.example.com"),
			route("no-env", "other", created, "other.example.com"),
		},
		"daemon", testGateway,
	)

	require.Equal(t, map[string]string{"portal.team-a.example.com": "team-a"}, topo.Hosts)
}

func TestBuildHonoursAcceptedRoutesOnly(t *testing.T) {
	sos := []*unstructured.Unstructured{scaledObject("team-a", "portal", "krci-portal", "")}

	other := route("team-a", "elsewhere", created, "other.team-a.example.com")
	patchParent(other, 0, func(parent map[string]any) {
		_ = unstructured.SetNestedField(parent, "other-gateway", "parentRef", "name")
	})

	t.Run("unattached route", func(t *testing.T) {
		topo := Build(sos, []*unstructured.Unstructured{
			unaccepted("team-a", "squat", created, "portal.team-a.example.com"),
			unaccepted("team-a", "empty", created),
		}, "daemon", testGateway)
		require.Empty(t, topo.Hosts)
		require.Equal(t, []string{"HTTPRoute team-a/squat: ignored, the Gateway has not accepted it"}, topo.Warnings)
	})

	t.Run("another gateway", func(t *testing.T) {
		topo := Build(sos, []*unstructured.Unstructured{other}, "daemon", testGateway)
		require.Empty(t, topo.Hosts)

		lenient := Build(sos, []*unstructured.Unstructured{other}, "daemon", "")
		require.Equal(t, map[string]string{"other.team-a.example.com": "team-a"}, lenient.Hosts, "without a configured Gateway every accepted route counts")
	})
}

func TestAcceptedBy(t *testing.T) {
	u := unaccepted("team-a", "portal", created, "portal.team-a.example.com")
	require.Empty(t, AcceptedBy(u))

	require.NoError(t, unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{ // accepted, parent in the route's namespace
			"parentRef":  map[string]any{"name": "local"},
			"conditions": []any{map[string]any{"type": "Accepted", "status": "True"}},
		},
		map[string]any{ // not accepted
			"parentRef":  map[string]any{"namespace": "envoy-gateway-system", "name": "main-gateway"},
			"conditions": []any{map[string]any{"type": "Accepted", "status": "False", "reason": "NotAllowedByListeners"}},
		},
		map[string]any{ // not a Gateway
			"parentRef":  map[string]any{"kind": "Service", "name": "svc"},
			"conditions": []any{map[string]any{"type": "Accepted", "status": "True"}},
		},
		map[string]any{ // accepted elsewhere
			"parentRef":  map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "namespace": "infra", "name": "edge"},
			"conditions": []any{map[string]any{"type": "ResolvedRefs", "status": "True"}, map[string]any{"type": "Accepted", "status": "True"}},
		},
	}, "status", "parents"))

	require.Equal(t, []string{"team-a/local", "infra/edge"}, AcceptedBy(u))
}

func TestBuildGivesConflictingHostToOldestRoute(t *testing.T) {
	sos := []*unstructured.Unstructured{
		scaledObject("team-a", "portal", "krci-portal", ""),
		scaledObject("team-b", "portal", "krci-portal", ""),
	}

	topo := Build(sos, []*unstructured.Unstructured{
		route("team-b", "hijack", created.Add(time.Hour), "portal.team-a.example.com"),
		route("team-a", "portal", created, "portal.team-a.example.com"),
	}, "daemon", testGateway)
	require.Equal(t, "team-a", topo.Hosts["portal.team-a.example.com"])
	require.Equal(t, []string{"hostname portal.team-a.example.com is claimed by several environments; team-a/portal keeps it"}, topo.Warnings)

	tie := Build(sos, []*unstructured.Unstructured{
		route("team-b", "portal", created, "shared.example.com"),
		route("team-a", "portal", created, "shared.example.com"),
	}, "daemon", testGateway)
	require.Equal(t, "team-a", tie.Hosts["shared.example.com"])
}

func TestBuildWarningsStayShort(t *testing.T) {
	so := scaledObject("team-a", "portal", "krci-portal", strings.Repeat("x", 300))

	topo := Build([]*unstructured.Unstructured{so}, nil, "daemon", testGateway)
	require.Len(t, topo.Warnings, 1)
	require.Less(t, len(topo.Warnings[0]), 400, "a warning quotes at most a cached value, never a raw annotation")
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"Portal.Example.com":      "portal.example.com",
		"portal.example.com:8443": "portal.example.com",
		"portal.example.com.":     "portal.example.com",
		" portal.example.com ":    "portal.example.com",
		"[::1]:8443":              "[::1]",
		"[::1]":                   "[::1]",
		"":                        "",
	} {
		require.Equal(t, want, NormalizeHost(in), in)
	}
}
