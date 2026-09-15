# traefik — image contract

Image: localhost/traefik:0.0.3 (Traefik v3.7.13; retag for your registry — the OCI version
label keeps the packaged-software version, the tag is the artifact version)
UID:GID baked: 10023:10023 (ad-hoc assignment; override with `--build-arg APP_UID/APP_GID`)
Checker topology: checker is parent (`traefiksupervisor`, PID 1), traefik is its child
Checker runtime: static Go binary (no interpreter or extra runtime in the image)
Layer format: OCI, zstd:chunked (applied at push, see Publishing)

Content: the official Traefik v3.7.13 release binary (per-architecture tarball, sha256 pinned from
the release's checksums file), CA trust bundle and zoneinfo from a digest-pinned AlmaLinux 10
minimal stage, shipped `FROM scratch`.

## Ports
| Port | Purpose                                                                          |
|------|----------------------------------------------------------------------------------|
| 8080 | entrypoint `web` (publish as 80; HTTP + ACME HTTP-01 challenge)                  |
| 8443 | entrypoint `websecure` (publish as 443; TLS + ACME TLS-ALPN-01)                  |
| 9090 | ADMIN_PORT: /startupz /livez /readyz /metrics (supervisor, OpenMetrics)          |
| 9101 | entrypoint `metrics`: Traefik's own Prometheus metrics (classic text format)     |
| 8082 | internal only (127.0.0.1): entrypoint `ping` backing the checker                 |

High ports keep the image non-root and capability-free; publishing 80->8080 / 443->8443 (or
`ip_unprivileged_port_start=0`, or NET_BIND_SERVICE) is the deployer's choice.

## Configuration (all runtime-overridable)
Traefik's entire static configuration is env-driven (`TRAEFIK_*`). Baked defaults:

| Env                                                  | Default                     | Notes                                              |
|------------------------------------------------------|-----------------------------|----------------------------------------------------|
| TRAEFIK_ENTRYPOINTS_WEB_ADDRESS                      | :8080                       |                                                    |
| TRAEFIK_ENTRYPOINTS_WEBSECURE_ADDRESS                | :8443                       |                                                    |
| TRAEFIK_ENTRYPOINTS_PING_ADDRESS                     | 127.0.0.1:8082              | keep aligned with PING_URL                         |
| TRAEFIK_PING / TRAEFIK_PING_ENTRYPOINT               | true / ping                 | required by the checker                            |
| TRAEFIK_ENTRYPOINTS_METRICS_ADDRESS                  | :9101                       |                                                    |
| TRAEFIK_METRICS_PROMETHEUS(_ENTRYPOINT)              | true / metrics              |                                                    |
| TRAEFIK_METRICS_PROMETHEUS_ADDENTRYPOINTSLABELS      | true                        | `traefik_entrypoint_*` families (see Metrics)      |
| TRAEFIK_METRICS_PROMETHEUS_ADDROUTERSLABELS          | true                        | `traefik_router_*` families                        |
| TRAEFIK_METRICS_PROMETHEUS_ADDSERVICESLABELS         | true                        | `traefik_service_*` families                       |
| TRAEFIK_METRICS_PROMETHEUS_BUCKETS                   | 0.005,0.01,0.025,0.05,0.1,0.25,0.5,1,2.5,5,10 | the shared bucket list of every image, so edge and server latency are comparable |
| TRAEFIK_ACCESSLOG                                    | unset (off)                 | `true` enables the JSON access log on stdout (see Logs) |
| TRAEFIK_ACCESSLOG_FORMAT                             | json                        |                                                    |
| TRAEFIK_ACCESSLOG_FIELDS_HEADERS_DEFAULTMODE         | drop                        | no request/response header reaches the access log … |
| TRAEFIK_ACCESSLOG_FIELDS_HEADERS_NAMES_X-Request-Id  | keep                        | … except the request id (correlation key)          |
| TRAEFIK_PROVIDERS_FILE_DIRECTORY / _WATCH            | /etc/traefik/dynamic / true | socket-free dynamic config                         |
| TRAEFIK_LOG_FORMAT / TRAEFIK_LOG_LEVEL               | json / INFO                 | also the supervisor's defaults (see below)         |

Let's Encrypt: supply a resolver per deployment, e.g.
```
TRAEFIK_CERTIFICATESRESOLVERS_LE_ACME_EMAIL=<you@example.org>
TRAEFIK_CERTIFICATESRESOLVERS_LE_ACME_STORAGE=/data/acme.json
TRAEFIK_CERTIFICATESRESOLVERS_LE_ACME_HTTPCHALLENGE_ENTRYPOINT=web
```
(or `..._ACME_TLSCHALLENGE=true` for TLS-ALPN-01). HTTP-01 requires Let's Encrypt to reach this
container on public port 80, TLS-ALPN-01 on 443. Routing to backends goes through the file provider
(mount dynamic config into /etc/traefik/dynamic — mount the directory, not single files, so watch
keeps working) or `TRAEFIK_PROVIDERS_HTTP_ENDPOINT`. No engine socket is used or supported.

Header aliasing: Traefik warns once per entrypoint at startup while
`entryPoints.<name>.http.aliasHeadersStrategy` is unset. Whether aliased header names
(`X_Auth_User` for `X-Auth-User`) must be deleted or rejected depends on the backends behind
the proxy, so the image bakes no value; set
`TRAEFIK_ENTRYPOINTS_WEB_HTTP_ALIASHEADERSSTRATEGY=delete` (and the same for `websecure`) in
deployments fronting CGI/WSGI/PHP/NGINX-style backends.

Supervisor knobs:

| Env                   | Required | Default                      | Notes                              |
|-----------------------|----------|------------------------------|------------------------------------|
| ADMIN_PORT            | optional | 9090                         |                                    |
| BIND_ADDR             | optional | 0.0.0.0                      | admin listener bind                |
| PING_URL              | optional | http://127.0.0.1:8082/ping   | must match the ping entrypoint     |
| ACME_STORAGE_PREPARE  | optional | /data/acme.json              | pre-created 0600; `off` disables   |
| LOG_LEVEL             | optional | from TRAEFIK_LOG_LEVEL (info)| supervisor lines; trace/debug/info/warn/error. Unset = Traefik's level lower-cased (FATAL/PANIC -> error) |
| LOG_FORMAT            | optional | from TRAEFIK_LOG_FORMAT (json)| supervisor lines; `json` or `text`. Unset = Traefik's format (`common` -> text) |
| HEALTH_CHECK_INTERVAL | optional | 5 s                          |                                    |
| HEALTH_CHECK_TIMEOUT  | optional | 2 s                          | must be < interval                 |
| HEALTH_STALE_FACTOR   | optional | 3                            |                                    |
| SHUTDOWN_DRAIN_DELAY  | optional | 3 s                          |                                    |
| SHUTDOWN_TIMEOUT      | optional | 15 s                         | covers traefik's own graceTimeOut  |

## Health checks registered
| Check        | Verifies                                                  |
|--------------|-----------------------------------------------------------|
| traefik-ping | traefik's /ping on the internal ping entrypoint returns 200 |

A dead traefik process additionally terminates the container immediately with the child's exit
status — no lying green. Probe command: `/traefiksupervisor healthcheck --endpoint=<startupz|livez|readyz>`
(exit 0/1); it targets 127.0.0.1 for wildcard binds, otherwise the configured BIND_ADDR.

## Metrics
The image implements the repository-wide metrics contract (README, section *Metrics contract*):
the supervisor serves the baseline families on `ADMIN_PORT` 9090 (`/metrics`, OpenMetrics text
`application/openmetrics-text; version=1.0.0; charset=utf-8`, `# EOF` terminated, served from the
first millisecond — before traefik answers its ping), and Traefik serves its own metrics on the
`metrics` entrypoint 9101 (`/metrics`, Prometheus classic text format as produced by Traefik;
scrape both ports).

Port 9090 (supervisor):

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `build_info` | gauge | `version`, `revision` | always 1; the running artifact (OCI version label = Traefik version, revision = build's VCS ref) |
| `health_check_up` | gauge | `check` (`traefik-ping`) | 1 = the check passed in the last checker cycle |
| `health_check_duration_seconds` | gauge | `check` | duration of the last run of that check |
| `health_check_last_success_timestamp_seconds` | gauge | `check` | unix time of the last pass; 0 until the check passed once |
| `health_snapshot_age_seconds` | gauge | — | age of the cached snapshot; grows past `HEALTH_STALE_FACTOR × HEALTH_CHECK_INTERVAL` exactly when `/livez` turns 503 |
| `health_checker_cycles_total` | counter | — | completed checker cycles |
| `health_draining` | gauge | — | 1 once SIGTERM/SIGINT latched the drain (readiness 503) |
| `supervisor_child_up`, `supervisor_child_start_time_seconds` | gauge | — | 1 while the traefik child runs / its start time (unix seconds) |
| `go_*`, `process_*` | Go runtime | Go client defaults | goroutines, GC, memory, CPU, fds, start time of the supervisor process |

Port 9101 (Traefik, labels enabled by the baked `TRAEFIK_METRICS_PROMETHEUS_ADD*LABELS`; the
histograms use the shared bucket list):

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `traefik_config_reloads_total`, `traefik_config_last_reload_success` | counter, gauge | — | dynamic configuration reloads and the unix time of the last successful one |
| `traefik_config_last_reload_failure` | gauge | — | unix time of the last failed reload (appears after the first failed reload only) |
| `traefik_open_connections` | gauge | `entrypoint`, `protocol` (`TCP`/`UDP`) | open connections per listener, every entrypoint including `ping` and `metrics` |
| `traefik_entrypoint_requests_total`, `traefik_entrypoint_requests_bytes_total`, `traefik_entrypoint_responses_bytes_total` | counter | `entrypoint`, `code`, `method`, `protocol` | requests and bytes as the edge sees them |
| `traefik_entrypoint_request_duration_seconds_{count,sum,bucket}` | histogram | `entrypoint`, `code`, `method`, `protocol` | request duration per entrypoint |
| `traefik_router_requests_total`, `traefik_router_requests_bytes_total`, `traefik_router_responses_bytes_total` | counter | `router`, `service`, `code`, `method`, `protocol` | the routes as the edge sees them (`<router>@<provider>`, e.g. `app@file`) |
| `traefik_router_request_duration_seconds_{count,sum,bucket}` | histogram | `router`, `service`, `code`, `method`, `protocol` | request duration per router |
| `traefik_service_requests_total`, `traefik_service_requests_bytes_total`, `traefik_service_responses_bytes_total` | counter | `service`, `code`, `method`, `protocol` | the hop Traefik -> upstream (`<service>@<provider>`, e.g. `frontend@file`) |
| `traefik_service_request_duration_seconds_{count,sum,bucket}` | histogram | `service`, `code`, `method`, `protocol` | upstream latency per service |
| `traefik_service_server_up` | gauge | `service`, `url` | 1 while an upstream server passes its health check (only for services with `healthCheck` configured; `url` is the configured upstream URL, never a client address) |
| `traefik_service_retries_total` | counter | `service` | retry attempts (only appears after a retry middleware fired) |
| `traefik_tls_certs_not_after` | gauge | `cn`, `sans`, `serial` | expiry of served certificates |
| `go_*`, `process_*` | Go runtime | Go client defaults | Traefik's own process |

Label values: `entrypoint` ∈ {`web`, `websecure`} for the request families (Traefik's internal
resources `ping@internal` / `prometheus@internal` are excluded from the request metrics unless
`TRAEFIK_METRICS_ADDINTERNALS=true`; `traefik_open_connections` lists all four entrypoints),
`code` = HTTP status, `method` = HTTP method, `protocol` ∈ {`http`, `websocket`, `grpc`} on the
request families and `TCP`/`UDP` on `traefik_open_connections`; `router`/`service` are the names
from the dynamic configuration suffixed with the provider. No label carries a client address, a
raw path, a header or a user identifier. Families appear on first use (Traefik creates series
lazily): the request families exist once the first request was routed.

## Logs
All of the image's own output is stdout, one JSON object per line:

- **Supervisor** lines (ECS field names): `@timestamp` (RFC 3339, UTC, nanoseconds),
  `log.level` (`trace`|`debug`|`info`|`warn`|`error`), `log.logger` = `supervisor`, `message`,
  `ecs.version` = `8.11`, `service.name` = `traefik`, `service.version` (the Traefik version),
  plus `process.pid` on the child-start line and `health.check` / `health.up` on transitions.
  Logged: one startup line with the effective non-secret configuration, the child start,
  `startup complete`, health-check transitions, drain start / completion / budget exhaustion,
  ACME storage preparation. Never a line per probe hit, never a configuration secret.
  `LOG_FORMAT=text` switches to `<time> <LEVEL> supervisor: <message> key=value…`.
- **Traefik's own log** keeps Traefik's JSON shape (`level`, `time`, `message`, …) as configured by
  `TRAEFIK_LOG_FORMAT` / `TRAEFIK_LOG_LEVEL`; it is third-party output and not re-shaped.
- **Traefik access log** (`TRAEFIK_ACCESSLOG=true`, off by default; the chart's `logging.access`
  sets it): Traefik's JSON access log on stdout, one line per request on the `web` / `websecure`
  entrypoints with `entryPointName`, `RouterName`, `ServiceName`, `RequestMethod`, `RequestPath`
  (the raw path — Traefik has no route templates), `DownstreamStatus`, `OriginStatus`, `Duration`,
  `OriginDuration`, `RequestContentSize`, `DownstreamContentSize`, `ClientAddr`, `StartUTC`, and
  the request id as `request_X-Request-Id`. All other request and response headers are dropped
  (`DEFAULTMODE=drop`), so no cookie, token or user agent reaches the log. Requests on the
  `ping` entrypoint (the checker's probe) are never logged: Traefik's internal resources
  (`ping@internal`, `prometheus@internal`) are excluded from the access log unless
  `TRAEFIK_ACCESSLOG_ADDINTERNALS=true` is set. Bodies and query strings are never logged.

## Deployment requirements
- Stop grace period: grant at least **20 s** before SIGKILL (3 s drain delay + 15 s drain budget + margin)
- Writable paths: `/data` (ACME storage; persistent mount, owner 10023), `/tmp` (tmpfs); image runs read-only
- Init required: no (the supervisor reaps)
- Capabilities required: none on the baked high ports; NET_BIND_SERVICE only if you reconfigure
  entrypoints to bind below 1024 inside the container
- Shutdown semantics: SIGTERM/SIGINT -> drain latch, delay, SIGTERM to traefik (its own graceful
  lifecycle applies); budget exhaustion escalates to SIGKILL and exits 1
- Scraping: two targets, the `admin` port (9090, `/metrics`) and the `metrics` port (9101,
  `/metrics`); the generic-stack chart keeps 9101 container-only, off the LoadBalancer Service

## Exit codes
0 clean shutdown · non-zero: traefik exit status propagated, or 1 when the drain budget was
exceeded / a fatal error occurred · (137 observed = SIGKILL, grace period granted was below the
documented requirement)

## Build host requirements
- podman with OCI image format; network to github.com (release tarball), proxy.golang.org (the
  supervisor's pinned Go modules, checksums in `supervisor/go.sum`) and the digest-pinned base
  registries
- the linux/arm64 half on an amd64 host requires qemu-user-static (binfmt) in the build VM
  (the Go supervisor cross-compiles natively; only the fetch stage runs emulated)

## Publishing
```
podman manifest push --all --compression-format zstd:chunked --compression-level 19 --format oci \
  localhost/traefik:0.0.3 docker://<registry>/traefik:0.0.3
```
