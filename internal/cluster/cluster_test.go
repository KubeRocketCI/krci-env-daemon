package cluster

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

const selector = "krci.epam.com/env-daemon=enabled"

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

func deployment(ns, name string, replicas, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(replicas),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "main",
				Env:  []corev1.EnvVar{{Name: "TOKEN", Value: "kept out of the cache"}},
			}}}},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

func taskPod(ns, name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{pipelinePodLabel: name}},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

// withSecrets adds what the daemon must not cache.
func withSecrets(u *unstructured.Unstructured) *unstructured.Unstructured {
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{
		"type": "external-push", "metadata": map[string]any{"password": "secret"},
	}}, "spec", "triggers")
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{
		"filters": []any{map[string]any{"requestHeaderModifier": map[string]any{"set": []any{
			map[string]any{"name": "Authorization", "value": "secret"},
		}}}},
	}}, "spec", "rules")

	if _, found, _ := unstructured.NestedSlice(u.Object, "status", "parents"); found {
		patchParent(u, 0, longMessage)
	}

	_ = unstructured.SetNestedField(u.Object, strings.Repeat("{", 1024), "metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")

	return u
}

// longMessage gives the parent's first condition the longest message the CRD allows.
func longMessage(parent map[string]any) {
	conditions, _, _ := unstructured.NestedSlice(parent, "conditions")
	condition, _ := conditions[0].(map[string]any)
	condition["message"] = strings.Repeat("m", 32768)
	conditions[0] = condition
	_ = unstructured.SetNestedSlice(parent, conditions, "conditions")
}

func newFakeDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		scaledObjectGVR: "ScaledObjectList",
		httpRouteGVR:    "HTTPRouteList",
	}, objs...)
}

func fixtures() (*fake.Clientset, *dynamicfake.FakeDynamicClient) {
	unlabelled := scaledObject("team-c", "portal", "krci-portal", "")
	unlabelled.SetLabels(nil)

	cs := fake.NewClientset(
		deployment("team-a", "krci-portal", 1, 1),
		deployment("team-b", "krci-portal", 0, 0),
		taskPod("team-a", "build-1", corev1.PodRunning),
		taskPod("team-b", "build-2", corev1.PodSucceeded),
	)
	dyn := newFakeDynamic(
		withSecrets(scaledObject("team-a", "portal", "krci-portal", "")),
		scaledObject("team-a", "el-gitlab", "el-gitlab", "krci-portal"),
		scaledObject("team-b", "portal", "krci-portal", ""),
		unlabelled,
		withSecrets(route("team-a", "portal", created, "portal.team-a.example.com", "shared.example.com")),
		route("team-b", "portal", created, "portal.team-b.example.com"),
		route("team-b", "shared", created.Add(-time.Hour), "shared.example.com"),
		unaccepted("team-b", "squat", created.Add(-2*time.Hour), "portal.team-a.example.com"),
	)

	return cs, dyn
}

func options() Options {
	return Options{Namespace: "daemon", Selector: selector, Gateway: testGateway}
}

// newCluster starts a Cluster over the fixtures, and returns the dynamic client they live in.
func newCluster(t *testing.T, opts Options) (*Cluster, *dynamicfake.FakeDynamicClient) {
	t.Helper()

	cs, dyn := fixtures()

	c, err := New(t.Context(), cs, dyn, opts)
	require.NoError(t, err)

	return c, dyn
}

func TestClusterWatchesAllNamespaces(t *testing.T) {
	c, _ := newCluster(t, options())

	require.Equal(t, []string{"team-a", "team-b"}, c.EnvNames())
	require.Equal(t, "team-b", c.HostEnv("PORTAL.team-b.example.com:443"))
	require.Equal(t, "team-b", c.HostEnv("shared.example.com"), "the older route keeps the host")
	require.Equal(t, "team-a", c.HostEnv("portal.team-a.example.com"), "an older route the Gateway did not accept claims nothing")
	require.Equal(t, "krci-portal", c.Env("team-a").Targets[1].After, "the wake-after annotation survives the cache")
	require.True(t, c.Ready("team-a", "krci-portal"))
	require.True(t, c.ScaledUp("team-a", "krci-portal"))
	require.False(t, c.Ready("team-b", "krci-portal"))
	require.False(t, c.ScaledUp("team-b", "krci-portal"))
	require.True(t, c.Exists("team-b", "krci-portal"))
	require.False(t, c.Exists("team-a", "el-gitlab"), "a ScaledObject without its Deployment")
	require.True(t, c.PipelineRunning("team-a"))
	require.False(t, c.PipelineRunning("team-b"))
}

func TestClusterWatchesOnlyListedNamespaces(t *testing.T) {
	opts := options()
	opts.WatchNamespaces = []string{"team-a"}

	c, _ := newCluster(t, opts)

	require.Equal(t, []string{"team-a"}, c.EnvNames())
	require.Equal(t, "team-a", c.HostEnv("portal.team-a.example.com"))
	require.Equal(t, "team-a", c.HostEnv("shared.example.com"), "the older claim is outside the watched namespaces")
	require.Empty(t, c.HostEnv("portal.team-b.example.com"))
	require.False(t, c.ScaledUp("team-b", "krci-portal"))
	require.False(t, c.PipelineRunning("team-b"))
}

func TestClusterKeepsOnlyWhatItReads(t *testing.T) {
	c, _ := newCluster(t, options())

	d, err := c.source("team-a").deployments.Deployments("team-a").Get("krci-portal")
	require.NoError(t, err)
	require.Empty(t, d.Spec.Template.Spec.Containers)
	require.Equal(t, int32(1), *d.Spec.Replicas)
	require.Equal(t, int32(1), d.Status.ReadyReplicas)

	for _, l := range []cache.GenericLister{c.source("team-a").scaledObjects, c.source("team-a").routes} {
		objs, err := l.List(labels.Everything())
		require.NoError(t, err)

		for _, o := range objs {
			u := o.(*unstructured.Unstructured)
			spec, _, _ := unstructured.NestedMap(u.Object, "spec")
			require.Len(t, spec, 1, "one spec field per object: scaleTargetRef or hostnames")
			require.NotContains(t, spec, "triggers")
			require.NotContains(t, spec, "rules")
			require.NotContains(t, u.GetAnnotations(), "kubectl.kubernetes.io/last-applied-configuration")
			require.Empty(t, u.GetLabels(), "labels are not read")

			b, err := u.MarshalJSON()
			require.NoError(t, err)
			require.Less(t, len(b), 1024, "%s %s: %s", u.GetKind(), u.GetName(), b)
		}
	}
}

func TestTransformsBoundTenantValues(t *testing.T) {
	long := strings.Repeat("x", 300)

	so, err := slimScaledObject(scaledObject("team-a", "big", long, long))
	require.NoError(t, err)

	u := so.(*unstructured.Unstructured)
	require.Len(t, u.GetAnnotations()[WakeAfterAnnotation], MaxNameLength+1)

	name, _, _ := unstructured.NestedString(u.Object, "spec", "scaleTargetRef", "name")
	require.Len(t, name, MaxNameLength+1)

	topo := Build([]*unstructured.Unstructured{u}, nil, "daemon", testGateway)
	require.Empty(t, topo.Envs)
	require.Equal(t, []string{"ScaledObject team-a/big: ignored, the scale target name is not a Deployment name"}, topo.Warnings)

	kept, err := slimScaledObject(scaledObject("team-a", "ok", "krci-portal", "tekton-interceptor"))
	require.NoError(t, err)
	require.Equal(t, map[string]string{WakeAfterAnnotation: "tekton-interceptor"}, kept.(*unstructured.Unstructured).GetAnnotations())

	r := route("team-a", "portal", created, "portal.team-a.example.com")
	r.SetAnnotations(map[string]string{WakeAfterAnnotation: long})
	patchParent(r, 0, longMessage)

	rt, err := slimRoute(r)
	require.NoError(t, err)

	u = rt.(*unstructured.Unstructured)
	require.Empty(t, u.GetAnnotations(), "routes carry no annotation the daemon reads")
	require.Equal(t, []string{testGateway}, AcceptedBy(u))

	b, err := u.MarshalJSON()
	require.NoError(t, err)
	require.Less(t, len(b), 1024)

	squat, err := slimRoute(unaccepted("team-a", "squat", created, "portal.team-b.example.com"))
	require.NoError(t, err)
	require.Empty(t, AcceptedBy(squat.(*unstructured.Unstructured)))
}

func TestClusterRebuildsOnChange(t *testing.T) {
	c, dyn := newCluster(t, options())

	var notified atomic.Bool

	go c.Run(t.Context(), func() {
		if c.Env("team-d") != nil {
			notified.Store(true)
		}
	})

	_, err := dyn.Resource(scaledObjectGVR).Namespace("team-d").Create(t.Context(),
		scaledObject("team-d", "portal", "krci-portal", ""), metav1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, notified.Load, 5*time.Second, 20*time.Millisecond)
}

func TestClusterIgnoresStatusOnlyUpdates(t *testing.T) {
	c := &Cluster{kick: make(chan struct{}, 1)}

	before := scaledObject("team-a", "portal", "krci-portal", "")
	after := before.DeepCopy()
	after.SetResourceVersion("2")
	c.pokeOnChange(before, after)
	require.Empty(t, c.kick, "a status write reaches the cache as a resource version bump only")

	after.SetAnnotations(map[string]string{WakeAfterAnnotation: "tekton-interceptor"})
	c.pokeOnChange(before, after)
	require.Len(t, c.kick, 1)
	<-c.kick

	// A Gateway accepting a route changes the kept status, so it rebuilds.
	attached, _ := slimRoute(route("team-a", "portal", created, "portal.team-a.example.com"))
	detached, _ := slimRoute(unaccepted("team-a", "portal", created, "portal.team-a.example.com"))
	c.pokeOnChange(detached, attached)
	require.Len(t, c.kick, 1)
}
