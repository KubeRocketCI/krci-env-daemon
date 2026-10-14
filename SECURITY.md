# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| 0.1.x   | :white_check_mark: |

## Reporting a Vulnerability

The KubeRocketCI team takes security vulnerabilities seriously. We appreciate your efforts to responsibly disclose your findings.

To report a security vulnerability, please follow these steps:

1. **DO NOT** disclose the vulnerability publicly on GitHub Issues or other public forums.
2. Email us at <SupportEPMD-EDP@epam.com> with a detailed description of the vulnerability.
3. Include steps to reproduce, impact, and any potential mitigations if known.
4. Allow time for the team to investigate and address the vulnerability before any public disclosure.

## What to expect

- Acknowledgment of your report within 48 hours
- An initial assessment of the report within 7 days
- Regular updates about the progress of addressing the vulnerability
- Credit for discovering the vulnerability (unless you prefer to remain anonymous)

## Disclosure Policy

We follow a coordinated disclosure process:

1. Once a vulnerability is confirmed, we develop and test a fix
2. We prepare a security advisory to accompany the fix
3. We release the fix and publish the security advisory simultaneously
4. After the fix has been available for 10 days, details may be discussed publicly

## Security Model

| Surface | Reachable from | Protection |
|---|---|---|
| `/.krci-wake/*` (`:8080`) | users, through Envoy, on every environment host | GET and HEAD only; the environment comes from the Host header through the HTTPRoute allowlist; answers component names and Ready flags, no namespace, pod, image, IP or error text; status requests are rate limited per environment; fixed list of assets |
| Waiting page | users, served by Envoy | CSP without inline script and with same-origin requests only, `no-store`, `X-Frame-Options: DENY`, `nosniff` |
| OTLP (`:4317`) | the Envoy namespace (NetworkPolicy) | reads the host and the response code of a request, never the path; a code that is not a status code and a host longer than a hostname are dropped; batches above 256 KiB and more than 16 streams in flight are refused before they are read (`internal/config`, `internal/grpcserver`) |
| KEDA scaler (`:9090`) | the KEDA namespace (NetworkPolicy) | answers whether a workload is active, changes nothing; a reference that is not a namespace and a name is refused before anything is stored; requests above 64 KiB and more than 4096 streams are refused |
| Metrics (`:8081`) | nobody, unless `networkPolicy.metricsNamespace` is set | read-only |
| Admin API | the pod's loopback | reachable only through `kubectl port-forward`; requests with an `Origin` header or a `Host` other than localhost are refused |
| Kubernetes API | the daemon | list and watch of ScaledObjects, Deployments, Pods and HTTPRoutes in the environment namespaces (all namespaces only with `watchAllNamespaces`); writes only its own Leases; no Secrets, ConfigMaps, exec or scale. The caches keep only the fields the daemon reads: replicas, readiness, pod phase, the ScaledObject scale target and wake-after annotation, the HTTPRoute hostnames and accepting Gateways, and no value longer than a Kubernetes name |
| HTTPRoutes | a tenant of an environment namespace | only routes the configured Gateway accepted give hostnames to an environment; a route without a parent cannot claim another environment's host |
| Pod | | distroless static image, non-root user 65532, read-only root filesystem, no capabilities, seccomp `RuntimeDefault`, `GOMEMLIMIT` below the memory limit |

NetworkPolicy takes effect only where the cluster's network plugin enforces it. Without it, any pod that reaches the gRPC ports can wake environments and, within the limits above, load the daemon.

Thank you for helping us keep our project and our users secure!
