# VSC-kubernetes-containers

Container build definitions, a generic Helm chart, and a CI pipeline for a small
authentication stack:
* a Spring Boot user-management API (GraalVM native)
* a Next.js auth-portal
* a FastAPI module service (Python compiled to a native binary with Nuitka), backed by a
  MySQL the operator manages
* PostgreSQL
* Traefik

Every image is built to an engine-independent container build standard:
* the images ship mechanisms (probe endpoints, env-driven configuration, signal-driven graceful
  shutdown, an OpenMetrics `/metrics` endpoint, JSON logs)
* all deployment policy lives in the chart

## Layout

```
src/<Component>/              one buildable image per folder
  Containerfile               multi-stage build, digest-pinned bases
  buildCommand                sh+PowerShell polyglot; THE canonical build invocation
  CONTRACT.md                 the image's complete deployment interface (incl. Metrics + Logs)
  patches/ supervisor/ ...    component-specific sources
src/charts/generic-stack/     the Helm chart (same artifact conventions: buildCommand, CONTRACT.md)
.github/workflows/build.yml   CI: build, scan, publish images + chart to GHCR
renovate.json                 automated bumps for base-image digests, action digests and CI tool pins
.trivyignore                  accepted Trivy findings, each with an expiry date
```

Operators and CD repositories need only the `CONTRACT.md` files:
* ports
* env knobs
* probe endpoints
* writable paths
* stop grace periods
* exit-code semantics
* the metric families and log shape the image serves

is all specified there.

## Artifacts and versioning

| Folder | Image | Artifact tag | Packaged software (OCI version label) | UID |
|---|---|---|---|---|
| src/Backend | user-mgmt-service | 0.0.3 | user_mgmt_service 0.0.1-SNAPSHOT | 10021 |
| src/DB | postgresql | 0.0.2 | PostgreSQL 16.15 | 10020 |
| src/Frontend | auth-portal | 0.0.2 | auth_portal 0.1.0 (static export + Go server) | 10022 |
| src/Frontend-node | auth-portal | 0.0.2-node | auth_portal 0.1.0 (Node.js runtime variant) | 10022 |
| src/Modules | module-service | 0.0.1 | module_service 0.1.0 (Nuitka native build, musl, scratch) | 10024 |
| src/Proxy | traefik | 0.0.3 | Traefik v3.7.13 | 10023 |
| src/charts/generic-stack | charts/generic-stack | 0.0.5 | — | — |

* The **tag is the artifact version** (this repo's build)
* the **OCI `image.version` label is the packaged software's version**
* Upstream application sources are cloned at build time
  * from commits pinned in the Containerfiles
  * patched from `patches/`
* All images run non-root with fixed UIDs, read-only rootfs, no capabilities,
and are published as OCI with zstd:chunked layers.

## Building locally

Inside a component folder:

```
sh buildCommand                          # POSIX shells
iex (Get-Content -Raw buildCommand)      # PowerShell
```

Both produce the identical multi-arch manifest `localhost/<image>:<tag>`.
The chart folder's `buildCommand` runs `helm lint` + `helm package`.

## Metrics contract

Every image serves `GET /metrics` on its `ADMIN_PORT` as OpenMetrics
(`application/openmetrics-text; version=1.0.0; charset=utf-8`, a `# TYPE` line per family,
`# EOF` terminator) — unconditionally, no content negotiation. The families below have the
same names, labels and histogram buckets in every image, so one dashboard query per question
works for every hop and a request flow can be drawn from the labels alone. No tracing runtime
is involved: flows are reconstructed from per-hop metrics and from the request id in the logs.

Pods carry the `app.kubernetes.io/name` / `app.kubernetes.io/version` labels; a scrape
configuration that copies them onto the series can group by component and running version
without a join.

### Baseline (every image)

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `build_info` | gauge | `version`, `revision` | always 1; the running artifact (OCI version label, VCS revision) |
| `health_check_up` | gauge | `check` | 1 = the check passed in the last checker cycle |
| `health_check_duration_seconds` | gauge | `check` | duration of the last run of that check |
| `health_check_last_success_timestamp_seconds` | gauge | `check` | unix time the check last passed; 0 until it passed once |
| `health_snapshot_age_seconds` | gauge | — | age of the cached health snapshot (staleness flips `/livez`) |
| `health_checker_cycles_total` | counter | — | completed checker cycles |
| `health_draining` | gauge | — | 1 once the shutdown drain latch is set |
| `supervisor_child_up`, `supervisor_child_start_time_seconds` | gauge | — | split layouts only (db, proxy): the supervised child process |

Plus the runtime of the language: `jvm_*` and `process_*` (backend), `go_*` and `process_*`
(the Go supervisors and the frontend server), `nodejs_*` and `process_*` (node variant),
`python_*` and `process_*` (modules). The baseline is served from the moment the admin listener
is up, before the application is ready.

### Routes (every HTTP server: backend, frontend, frontend-node, modules; Traefik has its own)

| Family | Type | Labels | Enables |
|---|---|---|---|
| `http_server_requests_seconds_{count,sum,bucket}` | histogram | `method`, `uri`, `status`, `outcome`, `exception` | request rate, error rate and latency percentiles per route |
| `http_server_requests_active` | gauge | `method`, `uri` | in-flight requests per route |
| `http_server_request_bytes_total`, `http_server_response_bytes_total` | counter | `uri` | traffic volume per route |

`uri` is always a route template, never the raw path: the backend's `/users/register`,
`/users/login`, `/users/me`, `/users`, `/users/{id}`, `/users/{id}/modules/{moduleId}`,
`/modules`; the frontend's `/`, `/login`, `/signup`,
`/dashboard`, `/api/login`, `/api/logout`, `/api/me`, `/api/signup`, `/static/**`; the module
service's `/api/v1/modules`, `/api/v1/modules/{module_id}`, `/openapi.json`, `/docs`,
`/docs/oauth2-redirect`, `/redoc`. Anything that
matches no route is `UNKNOWN` (the backend uses Micrometer's own `NOT_FOUND` / `REDIRECTION` /
`root` for the same purpose), so a scan cannot create series. `outcome` is Micrometer's
enumeration (`INFORMATIONAL`, `SUCCESS`, `REDIRECTION`, `CLIENT_ERROR`, `SERVER_ERROR`,
`UNKNOWN`). Histogram buckets are the same everywhere, including Traefik:
`0.005 0.01 0.025 0.05 0.1 0.25 0.5 1 2.5 5 10`.

### Flows (every hop that calls another component)

| Hop | Family | Labels |
|---|---|---|
| Traefik -> frontend | `traefik_service_requests_total`, `traefik_service_request_duration_seconds_*`, `traefik_service_requests_bytes_total`, `traefik_service_responses_bytes_total` | `service`, `code`, `method`, `protocol` (Traefik's own, on the proxy's port 9101) |
| frontend -> backend | `http_client_requests_seconds_{count,sum,bucket}`, `http_client_requests_active`, `http_client_request_bytes_total`, `http_client_response_bytes_total` | `peer` (= `backend`), `method`, `uri` (the backend's route template), `status`, `outcome` |
| backend -> modules | `http_client_requests_seconds_{count,sum,bucket}`, `http_client_requests_active`, `http_client_request_bytes_total`, `http_client_response_bytes_total` | `peer` (= `modules`), `method`, `uri` (the module service's route template), `status`, `outcome` (Micrometer's RestClient observation relabelled; see `src/Backend/CONTRACT.md`) |
| backend -> database | `hikaricp_connections_{active,idle,pending}`, `hikaricp_connections_acquire_seconds_*`, `hikaricp_connections_usage_seconds_*`, `hikaricp_connections_timeout_total`, `spring_data_repository_invocations_seconds_*` (`repository`, `method`, `state`) | Micrometer's own |
| modules -> MySQL | `db_client_statements_seconds_{count,sum,bucket}` (`operation` = `select`/`insert`/`update`/`delete`/`other`, shared buckets), `db_client_statement_errors_total` (`operation`), `db_pool_connections` (`state` = `checked_out`/`idle`/`overflow`), `db_pool_size` | SQLAlchemy cursor events and pool state (see `src/Modules/CONTRACT.md`) |
| database | `pg_up`, `pg_stat_database_*` (`numbackends`, `xact_commit_total`, `xact_rollback_total`, `blks_hit_total`, `blks_read_total`, `tup_{fetched,inserted,updated,deleted}_total`, `deadlocks_total`), `pg_database_size_bytes`, `pg_stat_activity_backends`, `pg_settings_max_connections`, `pg_locks` (see `src/DB/CONTRACT.md`; `_count` suffixes are reserved for histograms, which `promtool` enforces) | `datname`, `state`, `mode` |

`peer` is the logical component name from the chart (`backend`, `modules`, `db`), never a host
or an IP. With `job` on the server side and `peer` on the client side of every hop, a per-hop
table (rate, p95, error share) and a node graph edge -> frontend -> backend -> modules / db need
no extra joins.

### Traffic (every listener)

| Family | Type | Labels | Where |
|---|---|---|---|
| `http_server_connections_active` | gauge | `listener` (`main`, `admin`) | Go and Node servers via the connection-state hook; the module service from its two uvicorn listeners; the backend reads its `main` listener from the Tomcat connector (and serves the `tomcat_connections_*` families beside it) |
| `traefik_entrypoint_requests_total`, `traefik_entrypoint_requests_bytes_total`, `traefik_entrypoint_responses_bytes_total`, `traefik_open_connections` | Traefik | `entrypoint`, `code`, `method`, `protocol` | proxy, port 9101 |
| `traefik_router_requests_total`, `traefik_router_request_duration_seconds_*` | Traefik | `router`, `service`, `code`, `method`, `protocol` | proxy, port 9101: the routes as the edge sees them |

### Rules

- No label may carry a user id, an email, a token, a raw path or a client IP.
- Label values are stable, documented enumerations (each `CONTRACT.md` lists them); a new
  value is a contract change.
- OpenMetrics content type, `# EOF` terminator, `build_info` and the health families are
  served even before the application is up.

## Logs

Every image writes its own log as JSON to stdout, one object per line, with ECS field names as
flat dotted keys: `@timestamp`, `log.level`, `log.logger`, `message`, `ecs.version`,
`service.name`, `service.version`, and `http.request.id` on every line written inside a request.
`LOG_LEVEL` / `LOG_FORMAT` (`json` | `text`) are the knobs; the chart's `monitoring.logging` block
sets them for every component at once. Third-party children keep their own shape (PostgreSQL's
stderr lines with a fixed prefix, Traefik's JSON, Next.js's startup lines) — the contracts say
which.

The request id is the correlation key across proxy, frontend, backend and module service:
`X-Request-Id` is kept when the client sends a well-formed one, generated at the frontend
otherwise, forwarded to the backend (and by the backend to the module service), echoed in every
response, kept in Traefik's access log, and logged as `http.request.id`.

Access logs (`ACCESS_LOG`, `TRAEFIK_ACCESSLOG` for the proxy) are JSON, one line per request on
the main listener, never for probe or metrics hits: `http.request.id`, `http.request.method`,
`url.path` (the route template), `http.response.status_code`, `event.duration` (ns),
`http.request.bytes`, `http.response.bytes`, and `upstream.duration` for proxied calls — never
headers, bodies or query strings. Default on in the frontend (the edge of the application),
off in the backend (its metrics cover the steady state) and the proxy. The backend additionally
logs its domain events at `info` with `event.action` / `event.outcome` (`user.register`,
`user.login` with `reason`, `user.delete`), never the password or the token.

## The chart

`generic-stack` is a single generic chart: templates range over a `components:` map that is
deep-merged over `componentDefaults`, so CD repositories control everything through values —
override any field, disable a component, or add new components without touching a template.
Values are validated by `values.schema.json`; per-component `hpa:` / `pdb:` add scaling
policy. The default components wire
the full stack (Traefik file-provider routes → frontend → backend → db, plus the in-cluster
module service the backend calls) and encode every
image contract's probe endpoints, grace periods, and security posture. `templates/monitoring.yaml`
configures the admin and log interface of every container from one `monitoring` block (ports,
health checker, drain sequence, `LOG_LEVEL` / `LOG_FORMAT` / `ACCESS_LOG`); probe periods and
grace periods are derived from the same values. Scraping, alerting and dashboards are deployment
policy. What each deployment must supply (one Secret, registry credentials, the authority seed
SQL, the managed MySQL and its schema, storage, exposure, scraping) is listed in the chart's
`CONTRACT.md`.

## CI

`.github/workflows/build.yml` converges the registry to the repo state:

- **discover** hashes each build folder (`git rev-parse HEAD:src/<dir>`) and compares
  against `tree-<hash>` marker tags on GHCR — only out-of-date artifacts are rebuilt, and a
  failed run self-heals on the next push. Two gates: a **consistency gate** fails the run if a
  `buildCommand` tag and the chart's pinned image tag disagree, and an **immutability gate**
  fails it if a version tag is already published from a different source tree (bump the tag;
  `workflow_dispatch` with `force` overrides). Registry errors fail closed.
- **build** is a matrix of out-of-date images × [amd64, arm64] on native runners,
  with layer caching (actions/cache of the podman store) per image, architecture and
  source tree. Per-arch build args
  are parsed out of the folder's `buildCommand` — the workflow holds no
  build knowledge of its own. Each freshly built image passes a Trivy **HIGH+CRITICAL** gate
  (accepted findings expire in `.trivyignore`) and emits an SPDX SBOM before its per-arch push.
- **manifest** assembles the multi-arch lists and moves the tree + version tags. There is
  no floating `latest` tag: consumers pin versions.
- **chart** verifies (`helm lint --strict`; the default and scaling renders validated with
  kubeconform against the cluster's Kubernetes version), stamps the registry from the repo
  name, and publishes to `oci://ghcr.io/<repo>/charts`.
- **sign** signs every published version tag of every artifact (images and chart,
  older tags included) keylessly with cosign (GitHub OIDC, Rekor-logged) and attaches the
  SBOMs of freshly built images as attestations. The Ops repo's validate gate refuses
  unsigned references. Publishing and signing only happen on the default branch.
- **promote** commits the result into the Ops repo
  (`beatos-learns/VSC-kubernetes-deployment`) - the only way anything reaches the cluster
  (automated commits, no deploy step, no cluster credentials). It edits the
  anchored `tag: "…" # promoted …` lines of the two environment overlays in place (only
  the components the overlays manage: `db`, `backend`, `frontend`) and pins a newly
  published chart version in `Chart.yaml`/`Chart.lock`. Branch `promote/staging` becomes a
  PR with auto-merge armed - it merges itself once the Ops validate checks are green;
  branch `promote/prod` becomes a PR a human merges after verifying staging. Both branches
  are rebuilt from the Ops `main` on every run and only pushed when their content changed,
  so a run that rebuilt nothing causes no PR churn. Requires the Actions secret
  `OPS_REPO_TOKEN`: a fine-grained PAT scoped to **only** the Ops repo with *Contents* and
  *Pull requests* read/write (the default `GITHUB_TOKEN` cannot cross repositories); the
  job fails loudly when it is missing.

All actions are pinned to commit SHAs and every downloaded tool (trivy, syft, kubeconform)
is checksum-verified. Pull requests run everything except pushes and signing.
`workflow_dispatch` with `force` rebuilds all.

**Releasing a change:** bump the tag in the component's `buildCommand` and the matching
`components.<name>.image.tag` in the chart values (plus the chart's own `version`), push —
CI rebuilds exactly that, and the promote job opens the two Ops PRs. Forgetting one half
trips the consistency gate. Renovate proposes
base-image digest bumps and CI tool updates; upstream source bumps are manual (change the
pinned commit and upstream version ARG in the Containerfile).
