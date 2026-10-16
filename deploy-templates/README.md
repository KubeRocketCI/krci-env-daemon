# krci-env-daemon




![Version: 0.1.0-SNAPSHOT](https://img.shields.io/badge/Version-0.1.0--SNAPSHOT-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.1.0-SNAPSHOT](https://img.shields.io/badge/AppVersion-0.1.0--SNAPSHOT-informational?style=flat-square) 

A Helm chart for krci-env-daemon, which wakes KubeRocketCI environments that KEDA scaled to zero when they are accessed

**Homepage:** <https://docs.kuberocketci.io/>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| epmd-edp | <SupportEPMD-EDP@epam.com> | <https://solutionshub.epam.com/solution/kuberocketci> |

## Source Code

* <https://github.com/KubeRocketCI/krci-env-daemon>



## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#affinity-and-anti-affinity |
| environments | list | `[]` | Environments the daemon serves. `namespace` gets read access for the daemon, `hostnames` get the waiting page path `/.krci-wake/`, and `route` names the environment's HTTPRoute that shows the waiting page while the environment sleeps. |
| fullnameOverride | string | `"krci-env-daemon"` |  |
| gateway | object | `{"name":"main-gateway","namespace":"envoy-gateway-system"}` | Gateway that the waiting page route attaches to. Only the environment HTTPRoutes this Gateway accepted give their hostnames to the environments. |
| goMemLimit | string | `"230MiB"` | Go's soft memory limit (GOMEMLIMIT): the garbage collector works harder as the heap nears it. Keep it about 10% below `resources.limits.memory`. |
| idle | string | `"15m"` | How long an environment stays active after its last use: a request or a running pipeline; at least `1s`. KEDA scales its workloads to zero a `cooldownPeriod` after that. |
| image | object | `{"pullPolicy":"IfNotPresent","repository":"epamedp/krci-env-daemon","tag":""}` | Daemon image. Top-level `image` because the CD Pipeline Operator overrides image.repository and image.tag at this path. |
| image.tag | string | `""` | Overrides the image tag whose default is the chart appVersion. |
| imagePullSecrets | list | `[]` |  |
| networkPolicy.enabled | bool | `true` | Accept traffic only from Envoy, KEDA and, when set, the metrics namespace. |
| networkPolicy.envoyNamespace | string | `"envoy-gateway-system"` | Namespace of the Envoy proxies: access log and waiting page requests. |
| networkPolicy.kedaNamespace | string | `"keda"` | Namespace of KEDA: external scaler calls. |
| networkPolicy.metricsNamespace | string | `""` | Namespace allowed to scrape the metrics port. Empty keeps the port closed. |
| nodeSelector | object | `{}` | https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#nodeselector |
| podAnnotations | object | `{}` |  |
| podLabels | object | `{}` |  |
| resources | object | `{"limits":{"memory":"256Mi"},"requests":{"cpu":"10m","memory":"64Mi"}}` | Memory grows with the number of environments, faster with a Role per namespace than with `watchAllNamespaces` (one set of informers per watched namespace). Raise `limits.memory` and `goMemLimit` together for many environments. |
| scaledObjectSelector | string | `"krci.epam.com/env-daemon=enabled"` | Label selector of the ScaledObjects the daemon drives. |
| serviceAccount.annotations | object | `{}` | Annotations to add to the service account. |
| serviceAccount.create | bool | `true` | Create the service account. The daemon needs its token to read the cluster. |
| serviceAccount.name | string | `""` | The name of the service account to use. If not set and create is true, a name is generated using the fullname template. |
| serviceMonitor.enabled | bool | `false` | Create a Prometheus Operator ServiceMonitor for the metrics port. |
| serviceMonitor.interval | string | `"30s"` | Scrape interval. |
| tolerations | list | `[]` | https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/ |
| waitingPage.mergeType | string | `"StrategicMerge"` | How the waiting page's BackendTrafficPolicy merges with one that targets the Gateway: `StrategicMerge` or `JSONMerge`. Empty replaces the Gateway's policy for the environment's route. |
| wakeAfterTimeout | string | `"2m"` | Longest wait for a `krci.epam.com/wake-after` dependency before the dependent workload starts anyway. |
| watchAllNamespaces | bool | `false` | Read all namespaces through a ClusterRole instead of a Role in each environment namespace. Any namespace with selected ScaledObjects is then an environment; `environments` is needed only for the waiting page. |
