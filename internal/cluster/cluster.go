package cluster

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

var (
	scaledObjectGVR = schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"}
	httpRouteGVR    = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
)

const (
	pipelinePodLabel = "tekton.dev/taskRun"
	debounce         = 300 * time.Millisecond
	// SyncTimeout stays below the chart's startupProbe window, so a missing CRD or missing
	// access ends the process with a reason instead of a probe restart without one.
	// test/deploy checks the window.
	SyncTimeout = 45 * time.Second
)

// Options selects what the Cluster watches.
type Options struct {
	// Namespace is the daemon's own namespace. Its HTTPRoutes are not environment routes.
	Namespace string
	// Selector selects the ScaledObjects the daemon drives.
	Selector string
	// WatchNamespaces limits every watch to these namespaces. Empty watches all of them.
	WatchNamespaces []string
	// Gateway, as namespace/name, is the Gateway whose accepted HTTPRoutes give the
	// environments their hostnames. Empty accepts the routes of any Gateway.
	Gateway string
}

// Cluster keeps informer caches of the objects the daemon decides on and the topology
// derived from them.
type Cluster struct {
	selfNamespace string
	gateway       string
	sources       map[string]*source // by watched namespace; metav1.NamespaceAll when every namespace is watched

	mu   sync.RWMutex
	topo Topology

	kick chan struct{}
}

// source holds the listers of one watched namespace, or of all of them.
type source struct {
	deployments   appslisters.DeploymentLister
	pods          corelisters.PodLister
	scaledObjects cache.GenericLister
	routes        cache.GenericLister
}

// New starts the informers and returns once their caches are in sync.
func New(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, opts Options) (*Cluster, error) {
	c := &Cluster{selfNamespace: opts.Namespace, gateway: opts.Gateway, sources: map[string]*source{}, kick: make(chan struct{}, 1)}

	namespaces := opts.WatchNamespaces
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}

	var synced []cache.InformerSynced

	for _, ns := range namespaces {
		src, hasSynced, err := c.watch(ctx, cs, dyn, ns, opts.Selector)
		if err != nil {
			return nil, err
		}

		c.sources[ns] = src
		synced = append(synced, hasSynced...)
	}

	sctx, cancel := context.WithTimeout(ctx, SyncTimeout)
	defer cancel()

	if !cache.WaitForCacheSync(sctx.Done(), synced...) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		return nil, fmt.Errorf("informer caches did not sync within %s: check the KEDA and Gateway API CRDs "+
			"and the daemon's list and watch access in the watched namespaces", SyncTimeout)
	}

	c.rebuild()

	return c, nil
}

func (c *Cluster) watch(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, ns, selector string) (*source, []cache.InformerSynced, error) {
	// No resync: every read goes through the listers, so a resync would only replay
	// the caches into the change handler.
	typed := informers.NewSharedInformerFactoryWithOptions(cs, 0, informers.WithNamespace(ns))
	deployments := typed.Apps().V1().Deployments()

	pipelines := informers.NewSharedInformerFactoryWithOptions(cs, 0, informers.WithNamespace(ns),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = pipelinePodLabel }))
	pods := pipelines.Core().V1().Pods()

	soFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, ns,
		func(o *metav1.ListOptions) { o.LabelSelector = selector })
	scaledObjects := soFactory.ForResource(scaledObjectGVR)

	routeFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, ns, nil)
	routes := routeFactory.ForResource(httpRouteGVR)

	transforms := []struct {
		informer  cache.SharedIndexInformer
		transform cache.TransformFunc
	}{
		{deployments.Informer(), slimDeployment},
		{pods.Informer(), slimPod},
		{scaledObjects.Informer(), slimScaledObject},
		{routes.Informer(), slimRoute},
	}

	for _, t := range transforms {
		if err := t.informer.SetTransform(t.transform); err != nil {
			return nil, nil, err
		}
	}

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.poke() },
		UpdateFunc: func(oldObj, newObj any) { c.pokeOnChange(oldObj, newObj) },
		DeleteFunc: func(any) { c.poke() },
	}

	for _, inf := range []cache.SharedIndexInformer{scaledObjects.Informer(), routes.Informer()} {
		if _, err := inf.AddEventHandler(handler); err != nil {
			return nil, nil, err
		}
	}

	typed.Start(ctx.Done())
	pipelines.Start(ctx.Done())
	soFactory.Start(ctx.Done())
	routeFactory.Start(ctx.Done())

	src := &source{
		deployments:   deployments.Lister(),
		pods:          pods.Lister(),
		scaledObjects: scaledObjects.Lister(),
		routes:        routes.Lister(),
	}

	synced := []cache.InformerSynced{
		deployments.Informer().HasSynced,
		pods.Informer().HasSynced,
		scaledObjects.Informer().HasSynced,
		routes.Informer().HasSynced,
	}

	return src, synced, nil
}

// Run rebuilds the topology 300 ms after a ScaledObject or HTTPRoute change, at most once
// per such window, and then calls onChange, until ctx ends.
func (c *Cluster) Run(ctx context.Context, onChange func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.kick:
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(debounce):
		}

		c.rebuild()
		onChange()
	}
}

func (c *Cluster) poke() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// pokeOnChange ignores updates that change nothing the transforms kept, such as KEDA's
// status writes and resource version bumps.
func (c *Cluster) pokeOnChange(oldObj, newObj any) {
	o, okOld := oldObj.(*unstructured.Unstructured)
	n, okNew := newObj.(*unstructured.Unstructured)

	if okOld && okNew && equality.Semantic.DeepEqual(o.Object["spec"], n.Object["spec"]) &&
		equality.Semantic.DeepEqual(o.Object["status"], n.Object["status"]) &&
		maps.Equal(o.GetAnnotations(), n.GetAnnotations()) {
		return
	}

	c.poke()
}

// rebuild derives the topology from the caches and logs its warnings unless they equal
// the previous ones.
func (c *Cluster) rebuild() {
	var scaledObjects, routes []*unstructured.Unstructured

	for _, src := range c.sources {
		scaledObjects = appendUnstructured(scaledObjects, src.scaledObjects)
		routes = appendUnstructured(routes, src.routes)
	}

	t := Build(scaledObjects, routes, c.selfNamespace, c.gateway)

	c.mu.Lock()
	previous := c.topo.Warnings
	c.topo = t
	c.mu.Unlock()

	if !slices.Equal(t.Warnings, previous) {
		for _, w := range t.Warnings {
			slog.Warn("topology", "warning", w)
		}
	}
}

// source returns the listers that cover a namespace, or nil.
func (c *Cluster) source(ns string) *source {
	return cmp.Or(c.sources[ns], c.sources[metav1.NamespaceAll])
}

func appendUnstructured(out []*unstructured.Unstructured, l cache.GenericLister) []*unstructured.Unstructured {
	objs, err := l.List(labels.Everything())
	if err != nil {
		slog.Error("list from cache", "error", err)
		return out
	}

	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok {
			out = append(out, u)
		}
	}

	return out
}

// Env returns the environment of a namespace, or nil. The result must not be modified.
func (c *Cluster) Env(ns string) *Env {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.topo.Envs[ns]
}

// EnvNames lists the namespaces of all environments, sorted.
func (c *Cluster) EnvNames() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return slices.Sorted(maps.Keys(c.topo.Envs))
}

// HostEnv maps a request host to the namespace of the environment that owns it, or "".
func (c *Cluster) HostEnv(host string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.topo.Hosts[NormalizeHost(host)]
}

// Ready reports whether the Deployment asks for replicas and runs at least one Ready
// replica. A Deployment that scales down with Ready pods left is not Ready.
func (c *Cluster) Ready(ns, deployment string) bool {
	d := c.deployment(ns, deployment)
	return scaledUp(d) && d.Status.ReadyReplicas > 0
}

// ScaledUp reports whether the Deployment asks for any replicas.
func (c *Cluster) ScaledUp(ns, deployment string) bool {
	return scaledUp(c.deployment(ns, deployment))
}

// Exists reports whether the Deployment is in the cluster.
func (c *Cluster) Exists(ns, deployment string) bool {
	return c.deployment(ns, deployment) != nil
}

func scaledUp(d *appsv1.Deployment) bool {
	return d != nil && ptr.Deref(d.Spec.Replicas, 0) > 0
}

// PipelineRunning reports whether a Tekton TaskRun pod of the namespace is Pending or
// Running.
func (c *Cluster) PipelineRunning(ns string) bool {
	src := c.source(ns)
	if src == nil {
		return false
	}

	pods, err := src.pods.Pods(ns).List(labels.Everything())
	if err != nil {
		return false
	}

	return slices.ContainsFunc(pods, func(p *corev1.Pod) bool {
		return p.Status.Phase == corev1.PodPending || p.Status.Phase == corev1.PodRunning
	})
}

func (c *Cluster) deployment(ns, name string) *appsv1.Deployment {
	src := c.source(ns)
	if src == nil || name == "" {
		return nil
	}

	d, err := src.deployments.Deployments(ns).Get(name)
	if err != nil {
		return nil
	}

	return d
}

// The daemon reads Deployment replicas and readiness, Pod phase, the ScaledObject scale
// target and wake-after annotation, and the HTTPRoute hostnames and accepting Gateways.
// The transforms below keep nothing else in the caches: less memory, no pod templates
// (with their environment variables), trigger metadata or route filters inside the
// process, and no tenant-set value longer than a Kubernetes name, so that no number of
// objects in a watched namespace can grow the caches beyond their count.

// maxValue is the longest Deployment name or wake-after value the caches keep.
const maxValue = validation.DNS1123SubdomainMaxLength

func slimDeployment(obj any) (any, error) {
	d, ok := obj.(*appsv1.Deployment)
	if !ok {
		return obj, nil
	}

	return &appsv1.Deployment{
		ObjectMeta: slimMeta(d.ObjectMeta),
		Spec:       appsv1.DeploymentSpec{Replicas: d.Spec.Replicas},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: d.Status.ReadyReplicas},
	}, nil
}

func slimPod(obj any) (any, error) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}

	return &corev1.Pod{
		ObjectMeta: slimMeta(p.ObjectMeta),
		Status:     corev1.PodStatus{Phase: p.Status.Phase},
	}, nil
}

// slimScaledObject keeps the scale target's kind and name and the wake-after annotation.
// A value longer than a name is dropped, and Build then ignores the ScaledObject.
func slimScaledObject(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}

	out := slimUnstructured(u)

	if v, ok := u.GetAnnotations()[WakeAfterAnnotation]; ok && len(v) <= maxValue {
		out.SetAnnotations(map[string]string{WakeAfterAnnotation: v})
	}

	ref := map[string]any{}

	if kind, _, _ := unstructured.NestedString(u.Object, "spec", "scaleTargetRef", "kind"); kind != "" {
		ref["kind"] = kind[:min(len(kind), validation.DNS1035LabelMaxLength)]
	}

	if name, _, _ := unstructured.NestedString(u.Object, "spec", "scaleTargetRef", "name"); name != "" && len(name) <= maxValue {
		ref["name"] = name
	}

	return out, unstructured.SetNestedMap(out.Object, ref, "spec", "scaleTargetRef")
}

// slimRoute keeps the hostnames and the Gateways that accepted the route. The Gateway API
// CRD bounds the hostnames (16 of at most 253 characters) and the parents (32).
func slimRoute(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}

	out := slimUnstructured(u)

	if hostnames, found, err := unstructured.NestedStringSlice(u.Object, "spec", "hostnames"); err == nil && found {
		if err := unstructured.SetNestedStringSlice(out.Object, hostnames, "spec", "hostnames"); err != nil {
			return nil, err
		}
	}

	var parents []any

	for _, g := range AcceptedBy(u) {
		ns, name, _ := strings.Cut(g, "/")
		parents = append(parents, map[string]any{
			"parentRef":  map[string]any{"namespace": ns, "name": name},
			"conditions": []any{map[string]any{"type": "Accepted", "status": "True"}},
		})
	}

	if parents != nil {
		return out, unstructured.SetNestedSlice(out.Object, parents, "status", "parents")
	}

	return out, nil
}

// slimUnstructured copies the metadata the daemon reads: identity, resource version
// (the informers need it) and creation time (route precedence).
func slimUnstructured(u *unstructured.Unstructured) *unstructured.Unstructured {
	out := &unstructured.Unstructured{Object: map[string]any{}}
	out.SetAPIVersion(u.GetAPIVersion())
	out.SetKind(u.GetKind())
	out.SetName(u.GetName())
	out.SetNamespace(u.GetNamespace())
	out.SetUID(u.GetUID())
	out.SetResourceVersion(u.GetResourceVersion())
	out.SetCreationTimestamp(u.GetCreationTimestamp())

	return out
}

func slimMeta(m metav1.ObjectMeta) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: m.Name, Namespace: m.Namespace, UID: m.UID, ResourceVersion: m.ResourceVersion}
}
