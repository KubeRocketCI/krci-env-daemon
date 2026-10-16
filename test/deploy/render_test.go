// Package deploy_test renders the Helm chart with `helm template` and checks the guarantees
// that live in the chart. Without the helm binary the tests skip.
package deploy_test

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/KubeRocketCI/krci-env-daemon/internal/cluster"
	"github.com/KubeRocketCI/krci-env-daemon/internal/config"
	"github.com/KubeRocketCI/krci-env-daemon/internal/web"
)

const (
	releaseNamespace = "krci-env-daemon"
	chartDir         = "../../deploy-templates"
)

func helmTemplate(t *testing.T, values ...string) ([]byte, error) {
	t.Helper()

	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed, skipping chart render tests")
	}

	args := append([]string{"template", "krci-env-daemon", chartDir, "--namespace", releaseNamespace}, values...)

	return exec.Command("helm", args...).CombinedOutput()
}

type rendered []*unstructured.Unstructured

func render(t *testing.T, values ...string) rendered {
	t.Helper()

	out, err := helmTemplate(t, values...)
	require.NoErrorf(t, err, "helm template failed: %s", out)

	var objs rendered

	for doc := range strings.SplitSeq(string(out), "\n---") {
		var m map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(doc), &m))

		if m != nil {
			objs = append(objs, &unstructured.Unstructured{Object: m})
		}
	}

	return objs
}

func (r rendered) find(kind, namespace, name string) *unstructured.Unstructured {
	for _, o := range r {
		if o.GetKind() == kind && namespaceOf(o) == namespace && o.GetName() == name {
			return o
		}
	}

	return nil
}

func (r rendered) kinds(kind string) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured

	for _, o := range r {
		if o.GetKind() == kind {
			out = append(out, o)
		}
	}

	return out
}

func namespaceOf(o *unstructured.Unstructured) string {
	if ns := o.GetNamespace(); ns != "" {
		return ns
	}

	return releaseNamespace
}

// first returns the first element of the list at the path, which must be a map.
func first(t *testing.T, obj map[string]any, path ...string) map[string]any {
	t.Helper()

	list, _, err := unstructured.NestedSlice(obj, path...)
	require.NoError(t, err)
	require.NotEmpty(t, list, path)

	m, ok := list[0].(map[string]any)
	require.True(t, ok, path)

	return m
}

func container(t *testing.T, objs rendered) map[string]any {
	t.Helper()

	deploy := objs.find("Deployment", releaseNamespace, "krci-env-daemon")
	require.NotNil(t, deploy)

	return first(t, deploy.Object, "spec", "template", "spec", "containers")
}

func envVar(t *testing.T, objs rendered, name string) (string, bool) {
	t.Helper()

	env, _, err := unstructured.NestedSlice(container(t, objs), "env")
	require.NoError(t, err)

	for _, e := range env {
		m, _ := e.(map[string]any)
		if m["name"] == name {
			v, _ := m["value"].(string)
			return v, true
		}
	}

	return "", false
}

// ports maps the port names of a list of container or Service ports to the given field.
func ports(t *testing.T, list []any, field string) map[string]int {
	t.Helper()

	out := map[string]int{}

	for _, p := range list {
		m, _ := p.(map[string]any)
		name, _ := m["name"].(string)
		number, ok := m[field].(float64)
		require.True(t, ok, "%s of port %s", field, name)

		out[name] = int(number)
	}

	return out
}

// memoryBytes parses a Kubernetes quantity ("256Mi") or a GOMEMLIMIT value ("230MiB").
func memoryBytes(t *testing.T, v string) int64 {
	t.Helper()

	q, err := resource.ParseQuantity(strings.TrimSuffix(v, "B"))
	require.NoError(t, err, v)

	return q.Value()
}

func mustYAML(t *testing.T, o *unstructured.Unstructured) string {
	t.Helper()
	require.NotNil(t, o)

	b, err := yaml.Marshal(o.Object)
	require.NoError(t, err)

	return string(b)
}

var teamValues = []string{
	"--set", "environments[0].namespace=team-a",
	"--set", "environments[0].route=krci-portal",
	"--set", "environments[0].hostnames[0]=portal.team-a.example.com",
	"--set", "environments[1].namespace=team-b",
	"--set", "environments[1].hostnames[0]=portal.team-b.example.com",
}

func TestDefaults(t *testing.T) {
	objs := render(t)
	deploy := objs.find("Deployment", releaseNamespace, "krci-env-daemon")

	replicas, _, _ := unstructured.NestedNumberAsFloat64(deploy.Object, "spec", "replicas")
	require.EqualValues(t, 1, replicas)

	strategy, _, _ := unstructured.NestedString(deploy.Object, "spec", "strategy", "type")
	require.Equal(t, "Recreate", strategy, "two replicas would split the in-memory state")

	watch, _ := envVar(t, objs, "WATCH_NAMESPACES")
	require.Equal(t, releaseNamespace, watch, "without environments the daemon reads only its own namespace")

	selector, _ := envVar(t, objs, "SCALEDOBJECT_SELECTOR")
	require.Equal(t, config.DefaultSelector, selector)

	for name, want := range map[string]time.Duration{"IDLE": config.DefaultIdle, "WAKE_AFTER_TIMEOUT": config.DefaultWakeAfterTimeout} {
		got, _ := envVar(t, objs, name)

		d, err := time.ParseDuration(got)
		require.NoError(t, err, name)
		require.Equal(t, want, d, name)
	}

	gateway, _ := envVar(t, objs, "GATEWAY")
	require.Equal(t, "envoy-gateway-system/main-gateway", gateway, "the waiting page route's Gateway is the one whose routes count")

	c := container(t, objs)

	goMemLimit, _ := envVar(t, objs, "GOMEMLIMIT")
	limit, _, _ := unstructured.NestedString(c, "resources", "limits", "memory")
	require.Less(t, memoryBytes(t, goMemLimit), memoryBytes(t, limit), "GOMEMLIMIT stays below the container's memory limit")

	escalation, _, _ := unstructured.NestedBool(c, "securityContext", "allowPrivilegeEscalation")
	require.False(t, escalation)

	readOnly, _, _ := unstructured.NestedBool(c, "securityContext", "readOnlyRootFilesystem")
	require.True(t, readOnly)

	nonRoot, _, _ := unstructured.NestedBool(deploy.Object, "spec", "template", "spec", "securityContext", "runAsNonRoot")
	require.True(t, nonRoot)

	period, _, _ := unstructured.NestedNumberAsFloat64(c, "startupProbe", "periodSeconds")
	failures, _, _ := unstructured.NestedNumberAsFloat64(c, "startupProbe", "failureThreshold")
	require.Greater(t, time.Duration(period*failures)*time.Second, cluster.SyncTimeout,
		"the daemon must give up waiting for its caches, and say why, before the startupProbe restarts it")

	require.Empty(t, objs.kinds("ClusterRole"))
	require.Empty(t, objs.kinds("HTTPRoute"))
	require.Empty(t, objs.kinds("BackendTrafficPolicy"))
	require.Empty(t, objs.kinds("ServiceMonitor"))
	require.NotNil(t, objs.find("ServiceAccount", releaseNamespace, "krci-env-daemon"))
	require.NotNil(t, objs.find("Role", releaseNamespace, "krci-env-daemon-read"))
	require.NotNil(t, objs.find("NetworkPolicy", releaseNamespace, "krci-env-daemon"))
}

func TestPortsMatchTheDaemon(t *testing.T) {
	objs := render(t, teamValues...)
	want := map[string]int{
		"http":      config.PublicPort,
		"metrics":   config.MetricsPort,
		"grpc-keda": config.ScalerPort,
		"grpc-otlp": config.OTLPPort,
	}

	containerPorts, _, _ := unstructured.NestedSlice(container(t, objs), "ports")
	require.Equal(t, want, ports(t, containerPorts, "containerPort"))

	servicePorts, _, _ := unstructured.NestedSlice(objs.find("Service", releaseNamespace, "krci-env-daemon").Object, "spec", "ports")
	require.Equal(t, want, ports(t, servicePorts, "port"))

	rule := first(t, objs.find("HTTPRoute", releaseNamespace, "krci-env-daemon").Object, "spec", "rules")
	require.EqualValues(t, config.PublicPort, first(t, rule, "backendRefs")["port"])

	prefix, _, _ := unstructured.NestedString(first(t, rule, "matches"), "path", "value")
	require.Equal(t, web.PathPrefix, prefix)
}

func TestEnvironments(t *testing.T) {
	objs := render(t, teamValues...)

	watch, _ := envVar(t, objs, "WATCH_NAMESPACES")
	require.Equal(t, "team-a,team-b", watch)

	for _, ns := range []string{"team-a", "team-b"} {
		require.NotNil(t, objs.find("Role", ns, "krci-env-daemon-read"), ns)
		require.NotNil(t, objs.find("RoleBinding", ns, "krci-env-daemon-read"), ns)
	}

	require.Nil(t, objs.find("Role", releaseNamespace, "krci-env-daemon-read"))

	route := objs.find("HTTPRoute", releaseNamespace, "krci-env-daemon")
	hostnames, _, _ := unstructured.NestedStringSlice(route.Object, "spec", "hostnames")
	require.Equal(t, []string{"portal.team-a.example.com", "portal.team-b.example.com"}, hostnames)

	btp := objs.find("BackendTrafficPolicy", "team-a", "krci-env-daemon-waiting-page")
	require.NotNil(t, btp)

	require.Equal(t, "krci-portal", first(t, btp.Object, "spec", "targetRefs")["name"])

	merge, _, _ := unstructured.NestedString(btp.Object, "spec", "mergeType")
	require.Equal(t, "StrategicMerge", merge)
	require.Nil(t, objs.find("BackendTrafficPolicy", "team-b", "krci-env-daemon-waiting-page"), "no route, no waiting page")

	page := objs.find("ConfigMap", "team-a", "krci-env-daemon-waiting-page")
	require.NotNil(t, page)

	body, _, _ := unstructured.NestedString(page.Object, "data", "response.body")
	require.Contains(t, body, web.PathPrefix+"assets/wake.js")
	require.NotContains(t, body, "%", "Envoy reads the body as a format string")
}

func TestInvalidEnvironmentsFail(t *testing.T) {
	for name, tc := range map[string]struct {
		values []string
		want   string
	}{
		"repeated namespace": {
			[]string{"--set", "environments[0].namespace=team-a", "--set", "environments[1].namespace=team-a"},
			"listed twice",
		},
		"route without hostnames": {
			[]string{"--set", "watchAllNamespaces=true", "--set", "environments[0].namespace=team-a", "--set", "environments[0].route=krci-portal"},
			"needs hostnames",
		},
		"no namespace": {
			[]string{"--set", "environments[0].route=krci-portal"},
			"needs a namespace",
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := helmTemplate(t, tc.values...)
			require.Error(t, err)
			require.Contains(t, string(out), tc.want)
		})
	}
}

func TestWatchAllNamespaces(t *testing.T) {
	objs := render(t, append([]string{"--set", "watchAllNamespaces=true"}, teamValues...)...)

	_, ok := envVar(t, objs, "WATCH_NAMESPACES")
	require.False(t, ok)
	require.NotNil(t, objs.find("ClusterRole", releaseNamespace, "krci-env-daemon-read"))
	require.NotNil(t, objs.find("ClusterRoleBinding", releaseNamespace, "krci-env-daemon-read"))
	require.Nil(t, objs.find("Role", "team-a", "krci-env-daemon-read"))
	require.NotNil(t, objs.find("BackendTrafficPolicy", "team-a", "krci-env-daemon-waiting-page"))
}

func TestExistingServiceAccount(t *testing.T) {
	objs := render(t, append([]string{"--set", "serviceAccount.create=false", "--set", "serviceAccount.name=custom"}, teamValues...)...)

	require.Empty(t, objs.kinds("ServiceAccount"))

	sa, _, _ := unstructured.NestedString(objs.find("Deployment", releaseNamespace, "krci-env-daemon").Object, "spec", "template", "spec", "serviceAccountName")
	require.Equal(t, "custom", sa)

	for _, binding := range objs.kinds("RoleBinding") {
		require.Equal(t, "custom", first(t, binding.Object, "subjects")["name"], binding.GetName())
	}
}

func TestRBACIsReadOnly(t *testing.T) {
	for _, values := range [][]string{teamValues, append([]string{"--set", "watchAllNamespaces=true"}, teamValues...)} {
		objs := render(t, values...)

		for _, role := range append(objs.kinds("Role"), objs.kinds("ClusterRole")...) {
			rules, _, _ := unstructured.NestedSlice(role.Object, "rules")

			for _, r := range rules {
				rule, _ := r.(map[string]any)
				names, _, _ := unstructured.NestedStringSlice(rule, "resources")
				verbs, _, _ := unstructured.NestedStringSlice(rule, "verbs")
				resources := strings.Join(names, ",")

				for _, n := range names {
					require.NotContains(t, []string{"secrets", "configmaps", "deployments/scale", "pods/exec"}, n, role.GetName())
				}

				if resources == "leases" {
					require.Equal(t, releaseNamespace, namespaceOf(role))
					require.ElementsMatch(t, []string{"list", "create", "patch", "delete"}, verbs)

					continue
				}

				require.ElementsMatch(t, []string{"list", "watch"}, verbs, role.GetName()+" "+resources)
			}
		}
	}
}

func TestMetricsAccess(t *testing.T) {
	closed := render(t)
	require.NotContains(t, mustYAML(t, closed.find("NetworkPolicy", releaseNamespace, "krci-env-daemon")), "port: metrics")

	open := render(t, "--set", "networkPolicy.metricsNamespace=monitoring", "--set", "serviceMonitor.enabled=true")
	policy := mustYAML(t, open.find("NetworkPolicy", releaseNamespace, "krci-env-daemon"))
	require.Contains(t, policy, "kubernetes.io/metadata.name: monitoring")
	require.Contains(t, policy, "port: metrics")
	require.NotNil(t, open.find("ServiceMonitor", releaseNamespace, "krci-env-daemon"))

	none := render(t, "--set", "networkPolicy.enabled=false")
	require.Empty(t, none.kinds("NetworkPolicy"))
}
