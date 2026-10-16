# krci-env-daemon

`github.com/KubeRocketCI/krci-env-daemon`: a Go service, one pod per cluster, that wakes KubeRocketCI environments KEDA scaled to zero. Envoy Gateway sends it the access log over OTLP; the daemon drives KEDA through the external-push scaler protocol and serves the progress for the waiting page that Envoy shows while an environment starts. KEDA stays the only writer of replicas.

## Build & Test

```
make build       # → dist/krci-env-daemon-<arch>  (CGO_ENABLED=0)
make generate    # regenerate internal/externalscaler from externalscaler.proto (buf, pinned plugins)
make test        # lint first, then go test ./... -coverprofile=coverage.out
make lint        # golangci-lint v2 with .golangci.yaml
make helm-lint   # helm lint deploy-templates
make helm-docs   # regenerate deploy-templates/README.md from README.md.gotmpl
make help        # lists the other targets
```

- Unit tests run anywhere: `internal/cluster` and `internal/envstate` use client-go fake clients, the other packages use in-package fakes and a fake clock.
- Chart render tests (`test/deploy`) shell out to `helm template` and skip when `helm` is absent.

Run one test: `go test ./internal/<package>/... -run <TestName>`.

Configuration comes from environment variables read in `internal/config`; the chart sets them from its values, and `GOMEMLIMIT` from `goMemLimit`.

## Architecture

### Request, wake, sleep

1. The `EnvoyProxy` access-log sink sends a record per request over OTLP. `internal/otlp` reads `authority` and `response_code` only, never the path.
2. `internal/cluster` maps the host to an environment: a namespace with ScaledObjects that match `SCALEDOBJECT_SELECTOR` is an environment, and the hostnames of its HTTPRoutes that the `GATEWAY` accepted (`status.parents`) belong to it. A hostname claimed by several environments goes to the oldest route.
3. `internal/envstate` marks the environment active and signals KEDA's `StreamIsActive` streams (`internal/scaler`) for the workloads without `krci.epam.com/wake-after`. The others follow once their dependency is Ready or has no Deployment, or after `WAKE_AFTER_TIMEOUT`. A ScaledObject that is not a target of a managed environment gets a `NotFound` error, so KEDA keeps its replicas.
4. KEDA's polls get `IsActive=true` until `IDLE` passes without a request; Pending or Running Tekton TaskRun pods count as use and wake a sleeping environment. Then they get `false`, and KEDA scales to zero after the ScaledObject's `cooldownPeriod`.
5. While a route has no ready endpoints, Envoy answers with the waiting page (`deploy-templates/files/waiting-page.html` in a BackendTrafficPolicy `responseOverride`). The page polls `/.krci-wake/status` on its own host (`internal/web`) and loads `/.krci-wake/assets/*` from the binary; `TestWaitingPageContract` ties the page, its script and the asset allowlist together.

### Layout

```
cmd/krci-env-daemon/      — wiring: config, informers, state, listeners
internal/config/          — environment variables → Config, the fixed ports
internal/cluster/         — informers (listed namespaces or all), topology: environments, hosts, readiness
internal/envstate/        — the state machine, Leases, waiting page status, admin snapshot
internal/scaler/          — KEDA external scaler gRPC server
internal/otlp/            — OTLP logs receiver
internal/grpcserver/      — gRPC servers with the message, header, stream and connection limits
internal/web/             — public status and assets, admin API, metrics; assets/ is embedded
internal/externalscaler/  — generated from externalscaler.proto by make generate (KEDA version in doc.go)
deploy-templates/         — Helm chart; files/waiting-page.html is the page Envoy serves
test/deploy/              — Helm chart render tests
```

## Conventions / Do NOT

- **One replica.** The state lives in memory and in one Lease per environment (`env-<namespace>`); the chart pins `replicas: 1` with the `Recreate` strategy.
- **Never push `false` to KEDA.** Sleep goes through the polls; the reason is in the comment on `Manager.push`.
- **No write access** beyond the daemon's own Leases (server-side apply, delete). KEDA scales.
- **The waiting page learns component names and Ready flags only.** The environment comes from the Host header; no handler reflects input. `wake.js` writes with `textContent` only; the page has no inline script and no `%` (Envoy reads the body as a format string).
- **The informer caches keep only the fields the daemon reads** (transforms in `internal/cluster`), and clip every tenant-set value to one character over a Kubernetes name. Extend a transform before reading another field.
- **Only accepted HTTPRoutes count.** A route claims hostnames for its environment only with `Accepted=True` from the configured Gateway; otherwise a tenant could keep another environment asleep with an unattached route.
- **The gRPC listeners have no authentication; their limits are the defence.** Message size, streams in flight and open connections live in `internal/config` next to the ports, the server options in `internal/grpcserver`. Validate every input before storing anything under it.
- **Ports live in `internal/config`**, with what each serves. The chart's container and Service ports repeat them; `test/deploy` checks that they match.
- **Never hand-edit `internal/externalscaler`.** Run `make generate`.
