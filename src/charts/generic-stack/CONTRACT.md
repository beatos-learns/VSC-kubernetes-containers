# generic-stack — chart contract

Chart: `generic-stack` 0.0.3 (retag/republish for your registry)
Artifact: OCI Helm chart, pushed to `oci://<registry>/charts` (see Publishing)
Consumers: CD repositories (Argo CD, Flux, plain `helm upgrade --install`) that supply a
values overlay per environment; the chart itself carries no environment-specific value.

The chart is the policy half of the container build standard: the images ship mechanisms
(probe endpoints, env config surface, signal-driven drain), this chart wires them into
Kubernetes policy (probe scheduling, grace periods, mounts, services, secrets references).
Everything is driven by one generic component model; the four default components are plain
values entries, so a CD repo can override any field, disable any component, or add entirely
new components without touching a template.

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
| `ports.main` | name/containerPort/servicePort/protocol | The traffic port (maps the image's `PORT`) |
| `ports.admin` | admin/9090 | Probe + metrics port (`ADMIN_PORT`); probes are wired to it |
| `ports.extra` | [] | Additional container/service ports |
| `env` | {} | Plain env vars, tpl-rendered; materialized as ConfigMap `<release>-<component>-env` and injected via `envFrom` (never secret material — use `secretEnv`) |
| `secretEnv` | {} | `ENV_NAME: secret-key` — env from the component's secret |
| `existingSecret` | "" | Use this pre-created Secret instead of rendering one |
| `secret` | {} | `key: value` rendered into a Secret when `existingSecret` is unset |
| `secretMount.enabled` / `mountPath` / `keys` | false / /run/secrets / [] | Mount the component's secret as files (preferred over env for secret material); `keys` projects only those keys (least privilege for shared Secrets) |
| `files` | {} | `filename: content` rendered into a ConfigMap, tpl-rendered |
| `filesMountPath` | "" | Where the files ConfigMap mounts (read-only) |
| `persistence.enabled` | false | PVC (Deployment) or volumeClaimTemplate (StatefulSet); when disabled but `mountPath` is set, an emptyDir is mounted instead |
| `persistence.mountPath` / `subPath` / `size` / `accessModes` / `storageClass` | — | `storageClass` falls back to `global.storageClass` |
| `service.enabled` / `type` / `annotations` / `exposeAdmin` | true / ClusterIP / {} / false | |
| `ingress.*` | disabled | Standard networking.k8s.io/v1 Ingress targeting the main port; fails at render time when enabled without `hosts` |
| `probes.startup/liveness/readiness` | see values | HTTP GET `/startupz` `/livez` `/readyz` on the admin port; per-probe `enabled`, `path`, `periodSeconds`, `failureThreshold`, `timeoutSeconds`, `initialDelaySeconds` |
| `monitoring.podMonitor.enabled` / `interval` / `labels` | false / 30s / {} | `monitoring.coreos.com/v1` PodMonitor scraping `/metrics` on the admin port (needs the Prometheus Operator CRDs) |
| `resources` | {} | |
| `strategy` | {} | Deployment `strategy` / StatefulSet `updateStrategy` verbatim. Deployment default: `RollingUpdate` with `maxUnavailable: 0`, `maxSurge: 1` (zero-downtime); a persistent Deployment defaults to `Recreate` |
| `minReadySeconds` | 0 | Rendered when > 0 |
| `antiAffinity` | preferred | `none` / `preferred` / `required` hostname anti-affinity among the component's own pods; merged into `affinity` unless that already carries a `podAntiAffinity` |
| `topologySpreadConstraints` | [] | Verbatim; `labelSelector` defaults to the component's selector labels when omitted |
| `priorityClassName` | "" | |
| `terminationGracePeriodSeconds` | 30 | Defaults per component already satisfy each image's documented minimum |
| `automountServiceAccountToken` | false | |
| `podSecurityContext` / `containerSecurityContext` | {} | Merged OVER the hardened baseline (non-root, read-only rootfs, all capabilities dropped, no privilege escalation, RuntimeDefault seccomp) |
| `podAnnotations` / `podLabels` / `nodeSelector` / `tolerations` / `affinity` | | Pass-through |
| `command` / `args` / `initContainers` / `extraVolumes` / `extraVolumeMounts` | | Pass-through (tpl-rendered where listed above) |

Top level: `global.imageRegistry`, `global.imagePullSecrets`, `global.storageClass` (other
`global.*` keys are tolerated: Helm shares `global` across every chart of a release),
`nameOverride`, `commonLabels`, `extraObjects` (list of raw manifests, tpl-rendered — the
escape hatch for NetworkPolicies, ServiceMonitors, etc.).

Every pod mounts an emptyDir at `/tmp` (all images run read-only and write only there plus
their declared paths). A `checksum/config` pod annotation restarts workloads when their
ConfigMap/Secret material (`files`, `env`, inline `secret`) changes. An `existingSecret` is
outside the chart's view: after rotating it, `kubectl rollout restart` the consumers.
`persistence.size` of a StatefulSet is immutable once created (expand the PVC directly;
do-block-storage supports it) — changing the value afterwards makes the sync fail.

## Default components

| Component | Image | Kind | Notes |
|-----------|-------|------|-------|
| db | postgresql:0.0.1 (PostgreSQL 16.15) | StatefulSet | PVC at `/var/lib/postgresql` (PGDATA is created beneath it by the supervisor); password read from the mounted secret key `db-password`; `filesMountPath` preset to `/docker-entrypoint-initdb.d` |
| backend | user-mgmt-service:0.0.2 | Deployment | Wired to `<release>-db`; DB password and `JWT_SECRET` from secret keys `db-password` / `jwt-secret` (upstream reads env only) |
| frontend | auth-portal:0.0.1 | Deployment | `API_URL` wired to `<release>-backend:8080` |
| proxy | traefik:0.0.2 (Traefik v3.7.11) | Deployment, Service type LoadBalancer (80→8080, 443→8443) | Routes via the file provider: `files.routes.yaml` ConfigMap mounted at `/etc/traefik/dynamic`, default router → frontend; `/data` is an emptyDir until `persistence.enabled` (required for ACME) |

## What every deployment must supply

- **Secret material** — install fails fast at template time until provided. Either set
  per-component `secret:` maps, or pre-create Secrets and point `existingSecret` at them
  (both `db` and `backend` need `db-password`, so a single shared Secret referenced by both
  `existingSecret` fields avoids stating the password twice):
  ```yaml
  components:
    db:
      existingSecret: app-credentials
    backend:
      existingSecret: app-credentials
  ```
  Required keys: `db-password` (db + backend), `jwt-secret` (backend; Base64, ≥256-bit decoded).
- **Registry credentials** — when the registry packages are private:
  `global.imagePullSecrets: [{name: <dockerconfig-secret>}]`.
- **Authority seed SQL** — the backend does not seed the `USER_MODIFY`/`USER_DELETE`
  authorities; mount a `.sql` via `components.db.files` (runs once on first init; the
  schema must match what Hibernate generates for the pinned backend commit).
- **Exposure** — default is the proxy's LoadBalancer Service with plain HTTP on 80. For TLS
  supply Traefik ACME env (`TRAEFIK_CERTIFICATESRESOLVERS_...`) plus
  `components.proxy.persistence.enabled: true`, or disable the proxy and use
  `components.frontend.ingress`. Set `components.frontend.env.COOKIE_SECURE: "false"` only
  for plain-HTTP development.
- **Storage** — `global.storageClass` if the cluster default is not wanted; db PVC size.

## Verification

`helm lint` and `helm template` must pass (`buildCommand` runs lint + package). Rendered
manifests encode each image's documented requirements: probe endpoints on the admin port,
stop grace periods at or above the contract minimums (db 45 ≥ 40, backend/frontend 20 ≥ 15,
proxy 25 ≥ 20), read-only rootfs, dropped capabilities, non-root fixed UIDs (10020–10023),
tmpfs-style `/tmp`. The CI chart job additionally renders with `hpa`/`pdb` enabled and runs
both variants through kubeconform.

## Publishing

```
helm package . --destination dist
helm push dist/generic-stack-0.0.3.tgz oci://<registry>/charts
```
For this repository `<registry>` is `ghcr.io/beatos-learns/vsc-kubernetes-containers`; the
CI workflow derives it from the repository name and overrides `global.imageRegistry` at
publish time.
