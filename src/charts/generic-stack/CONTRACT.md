# generic-stack — chart contract

Chart: `generic-stack` 0.0.6 (retag/republish for your registry)
Artifact: OCI Helm chart, pushed to `oci://<registry>/charts` (see Publishing)
Consumers: CD repositories (Argo CD, Flux, plain `helm upgrade --install`) that supply a
values overlay per environment; the chart itself carries no environment-specific value.

The chart is the policy half of the container build standard: the images ship mechanisms
(probe endpoints, env config surface, signal-driven drain, `/metrics`, JSON logs), this chart
wires them into Kubernetes policy (probe scheduling, grace periods, mounts, services, secrets
references, the admin/log interface settings). Everything is driven by one generic component model;
the five default components are plain values entries, so a CD repo can override any field,
disable any component, or add entirely new components without touching a template.

## Component model

Resources are rendered by ranging over `components.<name>`. Every component is deep-merged
over `componentDefaults` (maps merge per key, lists and scalars replace), so a CD overlay
only states differences. All resource names are `<release>-<component>`; `nameOverride`
replaces the release-name part (the default cross-component URLs follow it).

Strings in `env`, `secret`, `files`, `existingSecret`, `ingress.hosts[].host`, `ingress.tls`,
`ingress.annotations`, `initContainers`, `extraVolumes`, `extraVolumeMounts`, and `extraObjects`
are rendered through `tpl` and may use template expressions (the default cross-component URLs
use `include "generic-stack.componentName"`, so they follow `nameOverride` and port changes).

Values are validated against `values.schema.json` on every lint/template/install: unknown keys
at the top level and inside a component are rejected (typo protection for CD overlays), enums
and integer ranges are enforced; k8s passthrough objects (`resources`, `affinity`, tolerations,
security contexts, extra volumes/containers) stay unvalidated by design.

| Key | Default | Purpose |
|-----|---------|---------|
| `enabled` | true | Render this component at all |
| `kind` | Deployment | `Deployment` or `StatefulSet` (StatefulSet gets a `-hl` headless service and `volumeClaimTemplates`) |
| `replicas` | 1 | Ignored (omitted from the workload) when `hpa.enabled` |
| `hpa.enabled` / `minReplicas` / `maxReplicas` | false / 1 / 3 | `autoscaling/v2` HorizontalPodAutoscaler on the workload; `replicas` is then omitted |
| `hpa.targetCPUUtilizationPercentage` / `targetMemoryUtilizationPercentage` / `behavior` | 70 / — / {} | At least one utilization target, each requires the matching `resources.requests`; `behavior` is passed through verbatim |
| `pdb.enabled` / `minAvailable` / `maxUnavailable` | false / — / — | `policy/v1` PodDisruptionBudget on the selector labels; exactly one of the two |
| `image.registry` | "" | Falls back to `global.imageRegistry` |
| `image.repository` / `tag` / `digest` | — | `repository` required; `digest` wins over `tag` |
| `image.pullPolicy` | IfNotPresent | |
| `security.uid` / `security.gid` | — | Required; become runAsUser/runAsGroup/fsGroup |
| `ports.main` | name/containerPort/servicePort/protocol | The traffic port; its `containerPort` is written to the container as `monitoring.portEnv` (`PORT`), so pod and image always listen on the same port |
| `ports.admin` | admin/9090 | Probe + metrics port, written as `monitoring.adminPortEnv` (`ADMIN_PORT`); the probes are wired to it and `/metrics` is served on it |
| `ports.extra` | [] | Additional container ports; each is also a Service port unless `exposed: false` (container-only, e.g. a metrics port that must not reach a LoadBalancer Service); an entry with `env` has its `containerPort` written to that variable in `monitoring.portFormat` |
| `env` | {} | Plain env vars, tpl-rendered; materialized as ConfigMap `<release>-<component>-env` and injected via `envFrom` after the `-monitoring` ConfigMap, so a key set here overrides the chart-wide `monitoring` block (never secret material — use `secretEnv`) |
| `secretEnv` | {} | `ENV_NAME: secret-key` — env from the component's secret |
| `existingSecret` | "" | Use this pre-created Secret instead of rendering one; falls back to `global.existingSecret` |
| `secret` | {} | `key: value` rendered into a Secret when neither `existingSecret` nor `global.existingSecret` is set |
| `secretMount.enabled` / `mountPath` / `keys` | false / /run/secrets / [] | Mount the component's secret as files (preferred over env for secret material); `keys` projects only those keys (least privilege for shared Secrets) |
| `files` | {} | `filename: content` rendered into a ConfigMap, tpl-rendered |
| `filesMountPath` | "" | Where the files ConfigMap mounts (read-only) |
| `persistence.enabled` | false | PVC (Deployment) or volumeClaimTemplate (StatefulSet); when disabled but `mountPath` is set, an emptyDir is mounted instead |
| `persistence.mountPath` / `subPath` / `size` / `accessModes` / `storageClass` | — | `storageClass` falls back to `global.storageClass` |
| `service.enabled` / `type` / `annotations` / `exposeAdmin` | true / ClusterIP / {} / false | `exposeAdmin` publishes the admin port on the Service for scrapers that target Services (a deployer's ServiceMonitor); pod-level scrapers need no Service port |
| `ingress.*` | disabled | Standard networking.k8s.io/v1 Ingress targeting the main port; fails at render time when enabled without `hosts` |
| `probes.startup/liveness/readiness` | see values | HTTP GET `/startupz` `/livez` `/readyz` on the admin port; per-probe `enabled`, `path`, `periodSeconds`, `failureThreshold`, `timeoutSeconds`, `initialDelaySeconds`. `periodSeconds` null (the default) follows the effective health checker: startup and readiness once per checker interval, liveness once per staleness window (staleFactor × interval) |
| `monitoring.portEnv` / `adminPortEnv` / `portFormat` | PORT / ADMIN_PORT / "%d" | Env names the container ports are written to, and the printf format of a traffic-port value (the proxy: `TRAEFIK_ENTRYPOINTS_WEB_ADDRESS` with `":%d"`) |
| `monitoring.levelEnv` / `formatEnv` / `accessEnv` | LOG_LEVEL / LOG_FORMAT / ACCESS_LOG | Env names the chart-wide `monitoring.logging` block is written to for this component (the proxy maps to `TRAEFIK_LOG_LEVEL` / `TRAEFIK_LOG_FORMAT` / `TRAEFIK_ACCESSLOG`) |
| `monitoring.upperCaseLevel` / `formatValues` | false / {json: json, text: text} | Level written upper-cased (Traefik), and the per-image spelling of the two formats (the proxy maps `text` to Traefik's `common`) |
| `monitoring.healthCheck.interval` / `timeout` / `staleFactor` | 5 / 2 / 3 | The image's own health-checker defaults (its CONTRACT.md); in effect while the chart-wide value is null, and the base of the derived probe periods (db: timeout 3) |
| `monitoring.shutdown.drainDelay` / `timeout` / `escalation` | 3 / 10 / 0 | The image's own drain defaults and the seconds it spends escalating after the budget; in effect while the chart-wide value is null, and the base of the derived grace period (db: 0 / 30 / 5, proxy timeout 15) |
| `resources` | {} | |
| `strategy` | {} | Deployment `strategy` / StatefulSet `updateStrategy` verbatim. Deployment default: `RollingUpdate` with `maxUnavailable: 0`, `maxSurge: 1` (zero-downtime); a persistent Deployment defaults to `Recreate` |
| `minReadySeconds` | 0 | Rendered when > 0 |
| `antiAffinity` | preferred | `none` / `preferred` / `required` hostname anti-affinity among the component's own pods; merged into `affinity` unless that already carries a `podAntiAffinity` |
| `topologySpreadConstraints` | [] | Verbatim; `labelSelector` defaults to the component's selector labels when omitted |
| `priorityClassName` | "" | |
| `terminationGracePeriodSeconds` | null | null = effective drain delay + shutdown timeout + escalation + `monitoring.shutdown.margin`, so the grace period follows the drain settings; an explicit value must still cover delay + timeout + escalation or the render fails |
| `automountServiceAccountToken` | false | |
| `podSecurityContext` / `containerSecurityContext` | {} | Merged OVER the hardened baseline (non-root, read-only rootfs, all capabilities dropped, no privilege escalation, RuntimeDefault seccomp) |
| `podAnnotations` / `podLabels` / `nodeSelector` / `tolerations` / `affinity` | | Pass-through |
| `command` / `args` / `initContainers` / `extraVolumes` / `extraVolumeMounts` | | Pass-through (tpl-rendered where listed above) |

Top level: `global.imageRegistry`, `global.imagePullSecrets`, `global.storageClass`,
`global.existingSecret` (one pre-created Secret every component that consumes secret keys
falls back to; other `global.*` keys are tolerated: Helm shares `global` across every chart of a
release),
`nameOverride`, `commonLabels`, `extraObjects` (list of raw manifests, tpl-rendered — the
escape hatch for NetworkPolicies, PrometheusRules, etc.), and the chart-wide `monitoring` block below.

### Monitoring (chart-wide: the admin and log interface of every container)

Every image ships the same admin interface (`/startupz` `/livez` `/readyz` and `/metrics` on
the admin port, driven by a cached health checker and a signal-driven drain) and the same log
interface (ECS JSON on stdout). `templates/monitoring.yaml` configures it once for all
components: each value below is written to a `<release>-<component>-monitoring` ConfigMap under
the env name the image reads, and the workload consumes it via `envFrom`. `null` keeps the
image's own default (the contracts list them per image).

| Key | Default | Env written | Purpose |
|-----|---------|-------------|---------|
| (ports) | — | `PORT`, `ADMIN_PORT`, extra ports with `env` | always written from `ports.*.containerPort` through the component's `monitoring.portEnv` / `adminPortEnv` / `portFormat` |
| `monitoring.healthCheck.interval` | null | `HEALTH_CHECK_INTERVAL` | seconds between checker cycles |
| `monitoring.healthCheck.timeout` | null | `HEALTH_CHECK_TIMEOUT` | seconds per check; every image requires it below the interval |
| `monitoring.healthCheck.staleFactor` | null | `HEALTH_STALE_FACTOR` | a snapshot older than factor × interval turns `/livez` 503 |
| `monitoring.shutdown.drainDelay` | null | `SHUTDOWN_DRAIN_DELAY` | seconds `/readyz` reports 503 before intake stops |
| `monitoring.shutdown.timeout` | null | `SHUTDOWN_TIMEOUT` | seconds granted to in-flight work |
| `monitoring.shutdown.margin` | 5 | — | seconds added to delay + timeout + escalation for the derived `terminationGracePeriodSeconds` |
| `monitoring.logging.level` | info | component `monitoring.levelEnv` (`LOG_LEVEL`; proxy `TRAEFIK_LOG_LEVEL`, upper-cased) | log threshold |
| `monitoring.logging.format` | json | component `monitoring.formatEnv` (`LOG_FORMAT`; proxy `TRAEFIK_LOG_FORMAT`, `text` -> `common`) | `json` (ECS) or `text` |
| `monitoring.logging.access` | null | component `monitoring.accessEnv` (`ACCESS_LOG`; proxy `TRAEFIK_ACCESSLOG`) | `true`/`false` switches the access log of every HTTP component; `null` keeps each image's default (frontend on, backend off, proxy off) |

A component's own `env` overrides any of these key by key. Both ConfigMaps are covered by the
`checksum/config` pod annotation, so a monitoring change rolls the pods. The effective health
checker (chart-wide value, else the component's `monitoring.healthCheck` image default) drives
the probe periods, and the effective drain (chart-wide, else `monitoring.shutdown`) drives the
grace period, so the pod's timing always matches what the container is told. What `/metrics` serves
is the repository README's "Metrics contract"; each image's `CONTRACT.md` lists its families.
`service.exposeAdmin` publishes the admin port on the Service for Service-based scrapers; the
proxy's Traefik metrics port 9101 is container-only (`ports.extra[].exposed: false`) and never
reaches the LoadBalancer Service. Pods carry the `app.kubernetes.io/version` label so series
can be grouped by running image tag. Scraping, alerting and dashboards are deployment policy
(`extraObjects` or the wrapper chart).

Every pod mounts an emptyDir at `/tmp` (all images run read-only and write only there plus
their declared paths). A `checksum/config` pod annotation restarts workloads when their
ConfigMap/Secret material (`files`, effective env, inline `secret`) changes. An `existingSecret` is
outside the chart's view: after rotating it, `kubectl rollout restart` the consumers.
`persistence.size` of a StatefulSet is immutable once created (expand the PVC directly;
do-block-storage supports it) — changing the value afterwards makes the sync fail.

## Default components

| Component | Image | Kind | Notes |
|-----------|-------|------|-------|
| db | postgresql:0.0.2 (PostgreSQL 16.15) | StatefulSet | PVC at `/var/lib/postgresql` (PGDATA is created beneath it by the supervisor); password read from the mounted secret key `db-password`; `filesMountPath` preset to `/docker-entrypoint-initdb.d`, so `files` entries run as first-init SQL; `pg_*` statistics on the admin port |
| backend | user-mgmt-service:0.0.5 | Deployment | Wired to `<release>-db` and, through `MODULE_SERVICE_URL`, to `<release>-modules:8080`; DB password and `JWT_SECRET` from secret keys `db-password` / `jwt-secret` (upstream reads env only); Micrometer route/JVM/pool metrics and the modules-hop client metrics on the admin port |
| frontend | auth-portal:0.0.2 | Deployment | `API_URL` wired to `<release>-backend:8080`; route, backend-hop and connection metrics on the admin port, access log on by default |
| modules | module-service:0.0.1 (module_service 0.1.0, compiled to a native binary) | Deployment | MySQL connection URL read from the mounted secret key `database-url` (`secretMount.keys` projects only that key); in-cluster only, no proxy route: the backend reaches it as `<release>-modules:8080`; route, database-hop and process metrics on the admin port |
| proxy | traefik:0.0.3 (Traefik v3.7.13) | Deployment, Service type LoadBalancer (80→8080, 443→8443) | Routes via the file provider: `files.routes.yaml` ConfigMap mounted at `/etc/traefik/dynamic`, default router → frontend; `/data` is an emptyDir until `persistence.enabled` (required for ACME); Traefik's entrypoint/router/service metrics on container port 9101 (not on the Service) |

## What every deployment must supply

- **Secret material** — install fails fast at template time until provided. Either set
  per-component `secret:` maps, or pre-create one Secret with every key and name it once:
  ```yaml
  global:
    existingSecret: app-credentials
  ```
  Required keys: `db-password` (db + backend), `jwt-secret` (backend; Base64, ≥256-bit decoded),
  `database-url` (modules; the managed MySQL as one URL,
  `mysql+pymysql://<user>:<password>@<host>:<port>/<database>?charset=utf8mb4`).
  A component's own `existingSecret` overrides the global one.
- **MySQL for the module service** — a database the operator manages (the stack ships no MySQL
  image); apply the upstream `schema.sql` to it once (the service runs no migrations, its
  `schema` health check stays red until the `modules` table exists). TLS defaults to
  `MYSQL_SSL_MODE=preferred` (used when the server offers it, certificate not verified); for a
  verified connection put the server CA under a secret key (e.g. `mysql-ca`) and set
  `components.modules.env.MYSQL_SSL_MODE: verify-identity`,
  `components.modules.env.MYSQL_SSL_CA: /run/secrets/mysql-ca` and
  `components.modules.secretMount.keys: [database-url, mysql-ca]`.
- **Registry credentials** — when the registry packages are private:
  `global.imagePullSecrets: [{name: <dockerconfig-secret>}]`.
- **Authority seed SQL** — the backend never creates the `USER_MODIFY` / `USER_DELETE`
  authorities its PUT/DELETE endpoints require, nor assigns a role to a user; supply a `.sql`
  via `components.db.files` (runs once on first init; its schema must match what Hibernate
  generates for the pinned backend commit).
- **Exposure** — default is the proxy's LoadBalancer Service with plain HTTP on 80. For TLS
  supply Traefik ACME env (`TRAEFIK_CERTIFICATESRESOLVERS_...`) plus
  `components.proxy.persistence.enabled: true`, or disable the proxy and use
  `components.frontend.ingress`. Set `components.frontend.env.COOKIE_SECURE: "false"` only
  for plain-HTTP development.
- **Storage** — `global.storageClass` if the cluster default is not wanted; db PVC size.
- **Scraping** — a scrape configuration for the admin port (`/metrics`; 9101 as well for the
  proxy) and a NetworkPolicy that admits the scraper; alerts and dashboards via `extraObjects`
  or the wrapper chart.

## Verification

`helm lint --strict` and `helm template` must pass (`buildCommand` runs lint + package).
CI validates the default and scaling renders with kubeconform against the Kubernetes schemas.
Rendered manifests encode each
image's documented requirements: probe endpoints on the admin port, stop grace periods derived
from each image's drain (db 40 ≥ 40, backend/frontend/modules 18 ≥ 15, proxy 23 ≥ 20), read-only
rootfs, dropped capabilities, non-root fixed UIDs (10020–10024), tmpfs-style `/tmp`.
Not covered by rendering: the scrape itself (each image's `CONTRACT.md` lists what
`/metrics` serves).

## Publishing

```
helm package . --destination dist
helm push dist/generic-stack-0.0.5.tgz oci://<registry>/charts
```
For this repository `<registry>` is `ghcr.io/beatos-learns/vsc-kubernetes-containers`; the
CI workflow derives it from the repository name and overrides `global.imageRegistry` at
publish time.
