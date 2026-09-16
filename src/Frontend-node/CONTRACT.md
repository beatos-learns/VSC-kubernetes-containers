# auth-portal (Node.js variant) — image contract

**Alternative variant.** The default auth-portal build is `src/Frontend` (static export served by a
compiled Go server, no Node runtime, ~10x smaller). Use this variant only when the full Next.js
server runtime is explicitly wanted. Both variants serve the same metric families, label
enumerations, histogram buckets and log shape, so swapping the variant changes no dashboard query.

Image: localhost/auth-portal:0.0.2-node (auth_portal 0.1.0; retag for your registry — the OCI
version label keeps the packaged-software version, the tag is the artifact version)
UID:GID baked: 10022:10022 (ad-hoc assignment; override with `--build-arg APP_UID/APP_GID`)
Checker topology: in-process (`ops/bootstrap.cjs` runs the Next.js standalone server, the checker
loop, the admin endpoints, the request instrumentation, and the signal handling in one Node process)
Checker runtime: reuses image runtime: Node.js 24
Metrics runtime: none — the exposition is hand-written in the bootstrap (no client library, no second
package graph in the image)
Layer format: OCI, zstd:chunked (applied at push, see Publishing)

Source: `github.com/yagan93/auth_portal` @ `599f7f8b0abcac739456d7bd95215c95b26706b6`, cloned during
the build and patched with `patches/0001-runtime-injectable-config.patch`. The patch makes the image
environment-neutral: the backend URL is the runtime env `API_URL` (server-side only, not the
build-time-inlined `NEXT_PUBLIC_API_URL`), the signup call is proxied through `/api/signup` so the
browser only ever talks same-origin, the jwt cookie `secure` flag is env-derived (`COOKIE_SECURE`),
the avatar is a local asset instead of a third-party CDN one, `output: 'standalone'` is enabled, and
`package.json` plus the pnpm lockfile pin next 16.3.5 (with sharp 0.35.4) in place of the upstream
next 16.2.4.

Base: `gcr.io/distroless/nodejs24-debian13` (digest-pinned) — documented exception to the base
policy: a purpose-built runtime image chosen for the Node.js environment it ships. The final image
is shell-less and has no package manager; the node binary lives at `/nodejs/bin/node` (not on
PATH), so every exec into the container must use that absolute path. The build stage remains
`node:24-slim`.

## Ports
| Env        | Default | Purpose                                  |
|------------|---------|------------------------------------------|
| PORT       | 3000    | HTTP UI + same-origin /api routes        |
| ADMIN_PORT | 9090    | /startupz /livez /readyz /metrics        |

## Configuration (all runtime-overridable)
| Env                   | Required | Default | Notes                                                    |
|-----------------------|----------|---------|----------------------------------------------------------|
| API_URL               | required | —       | base URL of user-mgmt-service as reachable FROM THIS CONTAINER, e.g. `http://backend:8080` |
| COOKIE_SECURE         | optional | true    | set `false` only for plain-HTTP local development        |
| BIND_ADDR             | optional | 0.0.0.0 |                                                          |
| LOG_LEVEL             | optional | info    | trace/debug/info/warn/error (bootstrap, access and re-emitted Next.js lines) |
| LOG_FORMAT            | optional | json    | `json` (ECS) or `text`; applies to every line the process writes |
| ACCESS_LOG            | optional | true    | one JSON line per request on the main listener (see Logs) |
| HEALTH_CHECK_INTERVAL | optional | 5 s     | sized for an interactive UI behind the proxy             |
| HEALTH_CHECK_TIMEOUT  | optional | 2 s     | must be < interval (validated at startup)                |
| HEALTH_STALE_FACTOR   | optional | 3       |                                                          |
| SHUTDOWN_DRAIN_DELAY  | optional | 3 s     | lets the proxy discover not-ready                        |
| SHUTDOWN_TIMEOUT      | optional | 10 s    | bound for draining in-flight requests                    |

Startup validates the whole configuration; a missing/invalid value logs one clear error and exits
non-zero immediately. One line logs name, version, revision, and the effective configuration when
the server starts listening.

## Health checks registered
| Check       | Verifies                                                                     |
|-------------|------------------------------------------------------------------------------|
| next-server | the Next.js HTTP listener is up in this process                              |
| backend-api | `${API_URL}/users` answers ANY HTTP response (reachability; 401/403 count as reachable) |

Probe command: `/nodejs/bin/node /app/ops/probe.cjs --endpoint=<startupz|livez|readyz>` (exit 0/1);
it targets 127.0.0.1 for wildcard binds, otherwise the configured BIND_ADDR.

## Metrics

`GET /metrics` on ADMIN_PORT (9090) serves OpenMetrics text (`application/openmetrics-text;
version=1.0.0; charset=utf-8`, a `# HELP`/`# TYPE` pair per family, `# EOF` terminator); the
families below implement the shared *Metrics contract* of the repository README. The ops families
(`build_info`, `health_*`) and the runtime families are served from the first millisecond, before
the Next.js server is up; the route, flow and traffic families appear with the first request.

Baseline:

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `build_info` | gauge | `version`, `revision` | always 1; the running artifact (packaged-software version, VCS revision) |
| `health_check_up` | gauge | `check` = `next-server`, `backend-api` | 1 = the check passed in the last checker cycle |
| `health_check_duration_seconds` | gauge | `check` | duration of the last run of that check |
| `health_check_last_success_timestamp_seconds` | gauge | `check` | unix time of the last pass; 0 until the check first passed |
| `health_snapshot_age_seconds` | gauge | — | age of the cached snapshot (staleness drives `/livez`) |
| `health_checker_cycles_total` | counter | — | completed checker cycles |
| `health_draining` | gauge | — | 1 once the drain latch is set (`/readyz` reports 503) |

Runtime (the process and Node.js, prom-client's names):

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `process_cpu_user_seconds_total`, `process_cpu_system_seconds_total`, `process_cpu_seconds_total` | counter | — | CPU time of the process |
| `process_start_time_seconds`, `process_resident_memory_bytes`, `process_virtual_memory_bytes`, `process_heap_bytes` | gauge | — | start time and memory (`/proc/self/status`: VmRSS, VmSize, VmData) |
| `process_open_fds`, `process_max_fds` | gauge | — | open file descriptors and the soft limit (`/proc/self`) |
| `nodejs_eventloop_lag_seconds` | gauge | — | event loop lag measured at scrape time |
| `nodejs_eventloop_lag_{min,max,mean,stddev,p50,p90,p99}_seconds` | gauge | — | event loop delay distribution since the previous scrape |
| `nodejs_active_handles`, `nodejs_active_requests` | gauge | — | active libuv handles and requests (prom-client names them `*_total`; a gauge with that suffix fails `promtool check metrics`, so the suffix is dropped here) |
| `nodejs_active_resources` | gauge | `type` | resources keeping the event loop alive, per type (`process.getActiveResourcesInfo()`) |
| `nodejs_heap_size_total_bytes`, `nodejs_heap_size_used_bytes`, `nodejs_external_memory_bytes` | gauge | — | V8 heap and external memory |
| `nodejs_version_info` | gauge | `version`, `major`, `minor`, `patch` | always 1; the Node.js runtime |

Routes (main listener):

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `http_server_requests_seconds_{count,sum,bucket}` | histogram | `method`, `uri`, `status`, `outcome`, `exception` | request rate, error share and latency percentiles per route; buckets `0.005 0.01 0.025 0.05 0.1 0.25 0.5 1 2.5 5 10` |
| `http_server_requests_active` | gauge | `method`, `uri` | requests in flight per route |
| `http_server_request_bytes_total`, `http_server_response_bytes_total` | counter | `uri` | traffic volume per route (request bytes = `Content-Length` of the request; response bytes = body bytes written) |

Flows (this server → user-mgmt-service, the `fetch` calls of the route handlers):

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `http_client_requests_seconds_{count,sum,bucket}` | histogram | `peer` = `backend`, `method`, `uri`, `status`, `outcome` | proxied calls per backend route template, timed until the response headers arrive; same buckets |
| `http_client_requests_active` | gauge | `peer`, `method`, `uri` | proxied calls in flight |
| `http_client_request_bytes_total`, `http_client_response_bytes_total` | counter | `peer`, `uri` | bytes sent (the request body) and announced by the backend (`Content-Length`, 0 when absent) |

Traffic (every listener):

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `http_server_connections_active` | gauge | `listener` = `main`, `admin` | open TCP connections per listener |

Label enumerations (fixed; a new value is a contract change):

- `uri` (server side) is the route class, never the raw path: `/`, `/login`, `/signup`,
  `/dashboard` (also `/dashboard/**`), `/api/login`, `/api/logout`, `/api/me`, `/api/signup`,
  `/static/**` (everything under `/_next/`, `/favicon.ico`, and the files of the bundle's
  `public/` directory), `UNKNOWN` for anything else — a scan cannot create series.
- `uri` (client side) is the backend's route template: `/users/register`, `/users/login`,
  `/users/me`, `/users`, `/users/{id}`, `UNKNOWN`.
- `status`: the numeric HTTP status; server side `0` when the client went away before a response
  was sent; client side `IO_ERROR` when the call failed before a response arrived.
- `outcome`: `INFORMATIONAL`, `SUCCESS`, `REDIRECTION`, `CLIENT_ERROR`, `SERVER_ERROR`, `UNKNOWN`.
- `exception`: `none`, or the class name of an error thrown synchronously by the request handler.
- No label ever carries a user id, an email, a token, a raw path or a client IP.

The health checker's own probe of `${API_URL}/users` uses an un-instrumented client and does not
appear in the `http_client_*` families.

## Logs

Every line the process writes goes to **stdout** as one JSON object with ECS field names (flat,
dotted keys); `LOG_FORMAT=text` switches to one human-readable line per event. Fixed fields:
`@timestamp` (UTC, ISO 8601), `log.level` (`trace`|`debug`|`info`|`warn`|`error`), `log.logger`,
`message`, `ecs.version` (`8.11`), `service.name` (`auth-portal`), `service.version` (the packaged
software version).

| Logger | Lines |
|---|---|
| `bootstrap` | the startup line (name, version, revision, effective non-secret configuration), `startup complete`, health transitions (`health.check`, `health.up`), the drain sequence, fatal errors |
| `access` | one line per request on the main listener, switched by `ACCESS_LOG` (default `true`): `http.request.id`, `http.request.method`, `url.path` (the route class, never the raw path), `http.response.status_code`, `event.duration` (integer nanoseconds), `http.request.bytes`, `http.response.bytes`, and `upstream.duration` (nanoseconds spent in backend calls) when the request called the backend |
| `next` | the Next.js server's and the application's own `console.*` output, re-emitted in the same shape (the level follows the console method) |

The request id: an incoming `X-Request-Id` (1–128 characters of `A-Z a-z 0-9 . _ -`) is kept,
anything else is replaced by a generated UUID; it is echoed in the `X-Request-Id` response header,
forwarded on every backend call the request makes, and written to the access line — the
correlation key across proxy, frontend and backend.

Never logged: probe or metrics hits on the admin listener, request or response headers, bodies,
query strings, cookies, tokens, passwords, emails, client IPs. A fatal configuration error at
startup is the one exception to the JSON shape: a single plain-text line on stderr before the
logger exists.

## Deployment requirements
- Stop grace period: grant at least **15 s** before SIGKILL (3 s drain delay + 10 s drain budget + margin)
- Writable paths: `/tmp`, `/app/.next/cache` (image runs read-only; mount tmpfs)
- Init required: no (single Node process, no children)
- Capabilities required: none; no privilege escalation
- PID 1 exception: PID 1 is `node` (the service's own launcher runtime) via exec-form ENTRYPOINT;
  signal handling is owned by `ops/bootstrap.cjs` (`NEXT_MANUAL_SIG_HANDLE=true`), which drains via
  `server.close()` + idle-connection teardown

## Exit codes
0 clean shutdown · 1 drain deadline exceeded (connections force-closed) or fatal error ·
(137 observed = SIGKILL, grace period granted was below the documented requirement)

## Build host requirements
- podman with OCI image format; network to github.com, registry.npmjs.org, and
  fonts.googleapis.com (`next/font/google` downloads fonts at build time)
- the linux/arm64 half on an amd64 host requires qemu-user-static (binfmt) in the build VM;
  `pnpm build` under emulation is slow but functional

## Publishing
```
podman manifest push --all --compression-format zstd:chunked --compression-level 19 --format oci \
  localhost/auth-portal:0.0.2-node docker://<registry>/auth-portal:0.0.2-node
```
