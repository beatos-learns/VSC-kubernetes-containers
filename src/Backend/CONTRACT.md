# user-mgmt-service — image contract

Image: localhost/user-mgmt-service:0.0.3 (user_mgmt_service 0.0.1-SNAPSHOT; retag for your
registry — the OCI version label keeps the packaged-software version, the tag is the artifact version)
UID:GID baked: 10021:10021 (ad-hoc assignment; override with `--build-arg APP_UID/APP_GID`)
Checker topology: in-process (the native binary serves endpoints, checker loop, signals, probe subcommand)
Checker runtime: none — in-process
Layer format: OCI, zstd:chunked (applied at push, see Publishing)

Source: `github.com/yagan93/user_mgmt_service` @ `747b7f4e3d1f09cc4434a75d9c991ea091fda548`, cloned during the build and
patched with `patches/0001-container-build-standard.patch` (health/ops machinery, graceful shutdown,
GraalVM native build config, Micrometer/Actuator metrics served on the admin port, request-id
correlation, access log, domain events). Spring Boot 4.1.0-M3 (pre-release milestone), Java 25,
compiled with GraalVM native-image (no JVM in the image).

Final base (per architecture, deliberate split):
- linux/amd64: `scratch` — fully static native binary (`--static --libc=musl`)
- linux/arm64: `quay.io/almalinuxorg/10-micro` (digest-pinned) — mostly static binary (`--static-nolibc`),
  because GraalVM's musl toolchain exists for x64 only

## Ports
| Env        | Default | Purpose                                                        |
|------------|---------|----------------------------------------------------------------|
| PORT       | 8080    | HTTP REST API (`/users/...`)                                   |
| ADMIN_PORT | 9090    | /startupz /livez /readyz /metrics (OpenMetrics, see Metrics)   |

## Configuration (all runtime-overridable)
| Env                          | Required | Default | Notes                                              |
|------------------------------|----------|---------|----------------------------------------------------|
| SPRING_DATASOURCE_URL        | required | —       | JDBC URL, e.g. `jdbc:postgresql://db:5432/appdb`   |
| SPRING_DATASOURCE_USERNAME   | required | —       |                                                    |
| SPRING_DATASOURCE_PASSWORD   | required | —       | secret; upstream reads env only — inject via engine secret-to-env |
| SPRING_JPA_HIBERNATE_DDL_AUTO| required | —       | validate / update / create / create-drop / none    |
| JWT_ISSUER                   | required | —       | issuer claim of issued tokens                      |
| JWT_SECRET                   | required | —       | secret; Base64, must decode to >= 256 bits (HMAC)  |
| JWT_EXPIRATION_MILLIS        | required | —       | token lifetime in milliseconds                     |
| BIND_ADDR                    | optional | 0.0.0.0 |                                                    |
| LOG_LEVEL                    | optional | info    | trace/debug/info/warn/error                        |
| LOG_FORMAT                   | optional | json    | json (ECS structured) or text                      |
| ACCESS_LOG                   | optional | false   | one JSON line per request on PORT (see Logs); never for ADMIN_PORT hits |
| HEALTH_CHECK_INTERVAL        | optional | 5 s     | sized for a fast API in front of a local database  |
| HEALTH_CHECK_TIMEOUT         | optional | 2 s     | must be < interval (validated at startup)          |
| HEALTH_STALE_FACTOR          | optional | 3       | snapshot older than factor x interval => not alive |
| SHUTDOWN_DRAIN_DELAY         | optional | 3 s     | lets the proxy discover not-ready before refusal   |
| SHUTDOWN_TIMEOUT             | optional | 10 s    | bound for draining in-flight requests              |

Startup validates the whole configuration; a missing required value or an invariant violation logs one
clear error and exits non-zero immediately. After a successful start one line logs name, version,
revision, and the effective non-secret configuration.

## Health checks registered
| Check          | Verifies                                                        |
|----------------|-----------------------------------------------------------------|
| spring-context | application context finished starting                           |
| database       | `SELECT 1` on the connection pool                               |

`/startupz` latches on the first cycle in which every registered check passes — including the
database check, so a database that is unreachable at boot delays startup completion (size startup
probe budgets accordingly). Both checks gate `/readyz`; neither ever gates `/livez` (staleness only).

Probe command: `/app/user-mgmt-service healthcheck --endpoint=<startupz|livez|readyz>` (exit 0/1);
it targets 127.0.0.1 for wildcard binds, otherwise the configured BIND_ADDR.

## Metrics

Implements the repository README's *Metrics contract* (shared families, labels and histogram
buckets across every image of the stack). One endpoint: `GET /metrics` on ADMIN_PORT, always
OpenMetrics (`application/openmetrics-text; version=1.0.0; charset=utf-8`, `# HELP`/`# TYPE` per
family, terminated by `# EOF`). The body is the application's Micrometer registry (Prometheus
exposition, only the families below are added by the image) followed by the ops families, which
are served from the health snapshot even before the Spring context is up (a scrape during startup
returns 200 with the ops families alone). No Spring Boot management server is started and no
actuator HTTP endpoint is exposed; the admin listener is the single scrape target.

Histogram buckets (`_bucket` boundaries) of every `*_seconds` histogram below: the shared list
`0.005 0.01 0.025 0.05 0.1 0.25 0.5 1 2.5 5 10` (Micrometer additionally keeps its own finer
percentile-histogram boundaries between 1 ms and 30 s; `histogram_quantile()` works on either).

### Ops (image contract baseline, served by the admin listener itself)
| Family | Type | Labels | Meaning |
|---|---|---|---|
| `build_info` | gauge | `version`, `revision` | always 1; packaged-software version and VCS revision of the running artifact |
| `health_check_up` | gauge | `check` = `spring-context`, `database` | 1 = the check passed in the last checker cycle |
| `health_check_duration_seconds` | gauge | `check` | duration of the last run of that check |
| `health_check_last_success_timestamp_seconds` | gauge | `check` | unix time of the last pass; 0 until the check first passed |
| `health_snapshot_age_seconds` | gauge | — | age of the cached snapshot the probes read (staleness = liveness) |
| `health_checker_cycles_total` | counter | — | completed checker cycles since process start |
| `health_draining` | gauge | — | 1 once the shutdown drain latch is set (readiness is 503 from then on) |

### Routes (main listener)
| Family | Type | Labels | Meaning |
|---|---|---|---|
| `http_server_requests_seconds_{count,sum,bucket}` | histogram | `method`, `uri`, `status`, `outcome`, `exception`, `error` | request rate, error rate and latency per route (Micrometer `http.server.requests`); `http_server_requests_seconds_max` is the per-interval maximum |
| `http_server_requests_active` | gauge | `method`, `uri` | requests in flight per route, the route being the one known when the request arrived (`UNKNOWN` for anything outside the route table; `method` outside the standard set is `OTHER`). Micrometer's `http.server.requests.active` long task timer is suppressed: it is tagged when the observation starts, before any route is known, so its `uri` would always be `UNKNOWN` |
| `http_server_request_bytes_total`, `http_server_response_bytes_total` | counter | `uri` | body bytes received / sent per route; bodies the servlet container renders itself (error pages) are not counted |
| `http_server_connections_active` | gauge | `listener` = `main` | open connections on the request listener, idle keep-alive included (read from the Tomcat connector) |
| `tomcat_connections_current_connections`, `tomcat_connections_config_max_connections`, `tomcat_threads_busy_threads`, `tomcat_threads_current_threads`, `tomcat_threads_config_max_threads` | gauge | `name` = `http-nio-0.0.0.0-8080` (connector) | Tomcat connector saturation under Micrometer's Tomcat names, read from the connector itself: the MBean-based Tomcat binder (`server.tomcat.mbeanregistry.enabled`) is off — with the registry on, the native image cannot start Tomcat (GraalVM reflection metadata of the connector introspection is conditional on it), so `tomcat_global_*`, `tomcat_servlet_*`, `tomcat_cache_*` and the keep-alive gauge are not served |

`uri` values (stable enumeration): `/users/register`, `/users/login`, `/users/me`, `/users`,
`/users/{id}` for the routes of the API — recorded for every request to a known route, including
the ones the security chain rejects with 401/403 and the login route the security filter answers
itself (the image announces the template on the request observation, Micrometer alone would
record `UNKNOWN` for those) — plus Micrometer's own fallbacks `NOT_FOUND` (404), `REDIRECTION`
(3xx), `root` and `UNKNOWN` (any other request that matched no route). A path scan cannot create
series. `outcome`: `INFORMATIONAL`, `SUCCESS`, `REDIRECTION`, `CLIENT_ERROR`, `SERVER_ERROR`,
`UNKNOWN`. `exception` / `error`: `none` or the simple class name of the exception that ended the
request. Note (upstream behaviour): the stateless security chain answers 403 for unauthenticated
requests, and error responses (404, 500, ...) are re-dispatched to Spring's `/error` handler which
the same chain rejects, so the client sees 403 while the metrics and the access log record the
status of the original request (404, 500, ...).

### Database hop (Micrometer binders)
| Family | Type | Labels | Meaning |
|---|---|---|---|
| `hikaricp_connections`, `hikaricp_connections_{active,idle,pending,min,max}` | gauge | `pool` = `HikariPool-1` | connection pool state |
| `hikaricp_connections_acquire_seconds_{count,sum}`, `hikaricp_connections_usage_seconds_{count,sum}`, `hikaricp_connections_creation_seconds_{count,sum}` | summary | `pool` | time to obtain a connection, time a connection is held, time to create one; each has a `..._seconds_max` gauge |
| `hikaricp_connections_timeout_total` | counter | `pool` | acquisitions that hit the pool timeout |
| `jdbc_connections_{active,idle,max,min}` | gauge | `name` = `dataSource` | the same pool as the JDBC layer sees it |
| `spring_data_repository_invocations_seconds_{count,sum,bucket}` | histogram | `repository`, `method`, `state`, `exception` | repository calls per method (`UserRepository`: `save`, `findByEmail`, `findById`, `existsById`, `findAll`, `deleteById`), `state` = `SUCCESS` / `CANCELED` / `ERROR`, `exception` = `None` or a class name; `spring_data_repository_invocations_seconds_max` is the per-interval maximum |

### Runtime
| Family | Type | Labels | Meaning |
|---|---|---|---|
| `jvm_info` | info | `runtime`, `vendor`, `version` | the runtime (GraalVM native image) |
| `jvm_memory_{used,committed,max}_bytes` | gauge | `area`, `id` | memory pools of the native-image runtime (`eden space`, `survivor space`, `old generation space`, the runtime code caches) |
| `jvm_gc_*` | gauge | — | garbage collection: `jvm_gc_overhead` in the native image (its collector MXBeans emit no notifications, so Micrometer's pause and allocation families stay absent) |
| `jvm_threads_*`, `jvm_classes_*` | gauge / counter | `state` | live, daemon, peak and started threads, thread states, loaded classes (`jvm_buffer_*` only appears where the runtime exposes buffer pools — not in the native image) |
| `process_cpu_usage`, `process_uptime_seconds`, `process_start_time_seconds`, `process_files_open_files`, `process_files_max_files` | gauge | — | process CPU share, uptime, start time and file descriptors |
| `system_cpus`, `system_load_average_1m` | gauge | — | available CPUs (Micrometer's `system.cpu.count`, renamed because a plain gauge must not end in `_count`) and load; `system_cpu_usage` is disabled (the native runtime cannot provide it and Micrometer would export NaN) |
| `logback_events_total` | counter | `level` | log events per level |
| `executor_*` | various | `name` = `applicationTaskExecutor` | Spring's application task executor (threads, queue, completed tasks) |
| `application_ready_time_seconds`, `application_started_time_seconds` | gauge | `main_application_class` | startup timing |
| `tomcat_sessions_*` | gauge / counter | — | servlet session counters (Micrometer's Tomcat binder reads them from the context manager without JMX); always 0 for this stateless API |

Deliberately not served: `disk_*` (read-only rootfs; the family would carry a filesystem `path`
label), `spring_security_*` (Spring Security's per-filter observation counters — ~40 series that
duplicate what `http_server_requests_seconds` and the login events say), `jvm_compilation_time_ms`
(no JIT in a native image), `process_cpu_time_ns` (abbreviated units; `process_cpu_usage`
remains), the MBean-based `tomcat_global_*` / `tomcat_servlet_*` / `tomcat_sessions_*` /
`tomcat_cache_*` and `tomcat_connections_keepalive_current_connections` (Tomcat's MBean registry stays off, see the Routes table), `system_cpu_usage` (NaN in the native image) and Micrometer's
`http_server_requests_active_seconds` long task timer (`http_server_requests_active` carries the
in-flight count per route, see the Routes table).
No label ever carries a user id, an email, a token, a raw path or a client address.

## Logs

Format: JSON, one object per line on **stdout**, flat dotted ECS field names (`LOG_FORMAT=json`,
the default; `text` switches to Logback's single-line human pattern for local development).
Emitted by the application's own logger — Logback through a structured formatter shipped in the
image (Spring Boot's built-in ECS format nests the fields; the shared log shape of the stack is
flat) — so every line of the process has the same shape, from the first line on.

Fixed fields: `@timestamp`, `log.level` (`trace`/`debug`/`info`/`warn`/`error`), `log.logger`,
`message`, `ecs.version`, `service.name` = `user-mgmt-service`, `service.version` = the
packaged-software version (`APP_VERSION`), `process.pid`, `process.thread.name`, and
`http.request.id` on every line written while a request of the main listener is being served.
Errors add `error.type`, `error.message`, `error.stack_trace`.

Request id: the `X-Request-Id` request header is accepted when it is 1–128 characters of
`[A-Za-z0-9._-]`, otherwise a lowercase UUIDv4 is generated; it is echoed in the `X-Request-Id`
response header and is the correlation key across proxy, frontend and backend logs.

Domain events (`log.level` `info`, `log.logger` = the emitting class):

| `event.action` | `event.outcome` | extra fields | when |
|---|---|---|---|
| `user.register` | `success` | `user.id` | a user was created |
| `user.register` | `failure` | `reason` = `duplicate-email` | the email is already registered |
| `user.login` | `success` | `user.id` | credentials accepted, token issued |
| `user.login` | `failure` | `reason` = `bad-credentials` / `unknown-user` / `other` | wrong password / no such user / any other authentication error (the client always receives 401) |
| `user.delete` | `success` / `failure` | `user.id`, `reason` = `unknown-user` on failure | a user was deleted / the id does not exist |

Access log (`ACCESS_LOG=true`, default `false` — the route metrics cover the steady state):
`log.logger` = `access`, one `info` line per request of the main listener with `http.request.id`,
`http.request.method`, `url.path` (the **route template**, same enumeration as the `uri` label —
never the raw path), `http.response.status_code`, `event.duration` (integer nanoseconds),
`http.request.bytes`, `http.response.bytes`. Never written for admin-port (probe, metrics) hits.

Startup: one line logs name, version, revision and the effective non-secret configuration; health
check transitions and the drain sequence are logged at `info`. Hibernate SQL echo (`show-sql`) is off, so
stdout carries nothing but JSON lines.

Never logged: passwords, tokens, the `Authorization` header or any other header, request or
response bodies, query strings, client addresses, emails.

## Deployment requirements
- Stop grace period: grant at least **15 s** before SIGKILL (3 s drain delay + 10 s drain budget + margin)
- Writable paths: `/tmp` only (image runs read-only; mount tmpfs as the deployer sees fit)
- Init required: no
- Capabilities required: none; no privilege escalation
- PID 1 exception: none — the native binary is PID 1 via exec-form ENTRYPOINT
- Database schema: no migrations ship with the app; schema handling is entirely
  `SPRING_JPA_HIBERNATE_DDL_AUTO`. The application itself never creates the `USER_MODIFY` /
  `USER_DELETE` authorities the PUT/DELETE endpoints require, nor assigns a role to a user;
  the deployment seeds both through the db image's init-script mechanism.

## Exit codes
0 clean shutdown · 1 drain deadline exceeded or fatal error · (137 observed = SIGKILL,
grace period granted was below the documented requirement)

## Build host requirements
- podman with OCI image format; network to github.com, services.gradle.org, Maven Central, ghcr.io
- native-image needs roughly 6–8 GB free memory per compile
- `buildCommand` runs two single-platform builds and assembles one manifest list
  (per-architecture toolchain/base selection; a single `--platform a,b` build mis-prunes
  ARG-parameterized stages under podman)
- the linux/arm64 half on an amd64 host requires qemu-user-static (binfmt) in the build VM and is
  slow under emulation; a native arm64 builder is the fast path
- exercise a first native build with a register/login round trip before rollout: jjwt's
  ServiceLoader wiring and BouncyCastle rely on shipped/repository reachability metadata

## Publishing
```
podman manifest push --all --compression-format zstd:chunked --compression-level 19 --format oci \
  localhost/user-mgmt-service:0.0.3 docker://<registry>/user-mgmt-service:0.0.3
```
