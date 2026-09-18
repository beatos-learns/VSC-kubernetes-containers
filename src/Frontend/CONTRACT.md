# auth-portal — image contract

**Default variant.** A Node.js-runtime variant of this image exists at `src/Frontend-node`
(tag `:0.0.3-node`); this build is the default. Both serve the same metric
families with the same labels, so swapping the variant changes no dashboard query.

Image: localhost/auth-portal:0.0.3 (auth_portal 0.1.0; retag for your registry — the OCI version
label keeps the packaged-software version, the tag is the artifact version)
UID:GID baked: 10022:10022 (ad-hoc assignment; override with `--build-arg APP_UID/APP_GID`)
Checker topology: in-process (one static Go binary serves the app, the checker loop, the admin
endpoints, the signal handling, and the probe subcommand)
Checker runtime: none — in-process
Layer format: OCI, zstd:chunked (applied at push, see Publishing)

Content: the Next.js app is built once as a **static export** (`output: 'export'`) in a Node build
stage and shipped as plain files; at runtime a small compiled Go server (`/app/auth-portal-server`)
serves the bundle `FROM scratch` — no Node runtime, no shell, no package manager in the final image.
The upstream server-side features are reproduced by the Go server:
- `/api/login`, `/api/logout`, `/api/me`, `/api/signup` — proxying to the backend with the httpOnly
  `jwt` cookie exactly as the upstream Next route handlers did
- the upstream middleware's redirects: `/` -> `/dashboard`; `/dashboard*` without the cookie ->
  `/login`; `/login` with the cookie -> `/dashboard`

It also serves the two routes of the dashboard's Modules view, relayed to the backend with the
cookie as bearer token, status and body unchanged:
- `/api/modules` (GET) — the module catalog (the backend's `/modules`)
- `/api/users/{id}/modules/{moduleId}` (POST subscribes, DELETE unsubscribes; both ids UUIDs,
  anything else 404) — the backend's route of the same shape, which decides who may change whose
  subscriptions (the user itself, or `USER_MODIFY`) and answers with the updated user

Source: `github.com/yagan93/auth_portal` @ `599f7f8b0abcac739456d7bd95215c95b26706b6`, cloned during
the build and patched with `patches/0001-static-export.patch` and
`patches/0002-module-subscriptions.patch`. The first enables the static export, drops the upstream
api routes and middleware (the Go server provides them), routes signup same-origin, makes the root
page a client redirect, and localizes the third-party CDN asset. The second turns the dashboard's
placeholder content into the Modules view: every module the module service offers as a card with
subscribe / unsubscribe, the subscription state from `/api/me`'s `moduleIds`, and inline states for
loading, an empty catalog, an unavailable catalog (retry) and an expired session (log in again).
The server's only dependency beyond the Go standard library is `prometheus/client_golang` (metrics
exposition).

## Ports
| Env        | Default | Purpose                                  |
|------------|---------|------------------------------------------|
| PORT       | 3000    | HTTP UI + same-origin /api routes        |
| ADMIN_PORT | 9090    | /startupz /livez /readyz /metrics        |

## Configuration (all runtime-overridable)
| Env                   | Required | Default     | Notes                                                    |
|-----------------------|----------|-------------|----------------------------------------------------------|
| API_URL               | required | —           | base URL of user-mgmt-service as reachable FROM THIS CONTAINER, e.g. `http://backend:8080` (trailing slashes stripped) |
| API_TIMEOUT           | optional | 30 s        | per-request bound for proxied backend calls              |
| COOKIE_SECURE         | optional | true        | set `false` only for plain-HTTP local development        |
| COOKIE_MAX_AGE        | optional | 604800 s    | jwt cookie lifetime (upstream hardcoded 7 days)          |
| STATIC_DIR            | optional | /app/static | location of the exported bundle                          |
| BIND_ADDR             | optional | 0.0.0.0     |                                                          |
| LOG_LEVEL             | optional | info        | trace/debug/info/warn/error                              |
| LOG_FORMAT            | optional | json        | `json` (ECS) or `text`                                   |
| ACCESS_LOG            | optional | true        | one JSON line per request on PORT (see Logs); never for ADMIN_PORT |
| HEALTH_CHECK_INTERVAL | optional | 5 s         | sized for an interactive UI behind the proxy             |
| HEALTH_CHECK_TIMEOUT  | optional | 2 s         | must be < interval (validated at startup)                |
| HEALTH_STALE_FACTOR   | optional | 3           |                                                          |
| SHUTDOWN_DRAIN_DELAY  | optional | 3 s         | lets the proxy discover not-ready                        |
| SHUTDOWN_TIMEOUT      | optional | 10 s        | bound for draining in-flight requests                    |

Startup validates the whole configuration; a missing/invalid value logs one clear error and exits
non-zero immediately. One line logs name, version, revision, and the effective configuration when
the server starts listening.

## Health checks registered
| Check       | Kind       | Verifies                                                                     |
|-------------|------------|------------------------------------------------------------------------------|
| static-root | self       | the exported bundle is present and readable (STATIC_DIR/index.html)          |
| backend-api | dependency | `${API_URL}/users` answers ANY HTTP response (reachability; 401/403 count as reachable) |

The checks run concurrently every HEALTH_CHECK_INTERVAL, each bounded by HEALTH_CHECK_TIMEOUT.
`/startupz` latches on the first cycle in which the self check passes. The dependency check gates
`/readyz` only, never `/startupz` or `/livez` (snapshot staleness only): an unreachable backend
takes the frontend out of load balancing, and the container is never restarted for it.
Probe command: `/app/auth-portal-server healthcheck --endpoint=<startupz|livez|readyz>` (exit 0/1);
it targets 127.0.0.1 for wildcard binds, otherwise the configured BIND_ADDR.

## Metrics

`GET /metrics` on ADMIN_PORT serves the stack-wide metrics contract of the repository README
("Metrics contract") as OpenMetrics (`application/openmetrics-text; version=1.0.0`, a `# TYPE`
line per family, `# EOF` terminator), served unconditionally — no content negotiation. The
baseline families are available before the application is ready.

Baseline and runtime:

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `build_info` | gauge | `version`, `revision` | always 1; the running artifact (OCI version label, VCS revision) |
| `health_check_up` | gauge | `check` = `static-root` \| `backend-api` | 1 = the check passed in the last cycle |
| `health_check_duration_seconds` | gauge | `check` | duration of the last run of that check |
| `health_check_last_success_timestamp_seconds` | gauge | `check` | unix time the check last passed; 0 until then |
| `health_snapshot_age_seconds` | gauge | — | age of the cached snapshot (staleness flips `/livez`) |
| `health_checker_cycles_total` | counter | — | completed checker cycles |
| `health_draining` | gauge | — | 1 once the shutdown drain latch is set |
| `go_*`, `process_*` | client_golang | — | Go runtime and process collectors (`process_start_time_seconds`, `process_resident_memory_bytes`, `go_goroutines`, `go_memstats_*`, `go_gc_*`, ...) |

Routes on PORT (the server side of the edge -> frontend hop):

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `http_server_requests_seconds_{count,sum,bucket}` | histogram | `method`, `uri`, `status`, `outcome`, `exception` | request rate, error rate and latency per route; buckets `0.005 0.01 0.025 0.05 0.1 0.25 0.5 1 2.5 5 10` |
| `http_server_requests_active` | gauge | `method`, `uri` | requests in flight per route |
| `http_server_request_bytes_total`, `http_server_response_bytes_total` | counter | `uri` | body bytes read / written per route (pre-created at 0 for every route class) |
| `http_server_connections_active` | gauge | `listener` = `main` \| `admin` | open TCP connections per listener |

The backend hop (client side, `peer` is the logical component name, never a host):

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `http_client_requests_seconds_{count,sum,bucket}` | histogram | `peer` = `backend`, `method`, `uri`, `status`, `outcome` | proxied calls by the backend's own route template; same buckets |
| `http_client_requests_active` | gauge | `peer`, `method`, `uri` | proxied calls in flight |
| `http_client_request_bytes_total`, `http_client_response_bytes_total` | counter | `peer`, `uri` | body bytes sent to / received from the backend (pre-created at 0 for every backend route) |

Label enumerations (a new value is a contract change):
- `uri` on the server side: `/`, `/login`, `/signup`, `/dashboard` (covers `/dashboard/**`),
  `/api/login`, `/api/logout`, `/api/me`, `/api/signup`, `/api/modules`,
  `/api/users/{id}/modules/{moduleId}`, `/static/**` (everything under `/_next/` and every other
  file the exported bundle resolves), `UNKNOWN` (anything else — a scan creates no series). The
  six `/api/*` routes are the only proxied ones.
- `uri` on the client side: the backend's templates `/users/register`, `/users/login`,
  `/users/me`, `/users`, `/users/{id}`, `/modules`, `/users/{id}/modules/{moduleId}`, else
  `UNKNOWN`.
- `method`: `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`, else `OTHER`.
- `status`: the numeric status code; `IO_ERROR` on the client side when the backend could not be
  reached. `outcome`: `INFORMATIONAL`, `SUCCESS`, `REDIRECTION`, `CLIENT_ERROR`, `SERVER_ERROR`,
  `UNKNOWN` (transport error). `exception`: `none`, or `panic` when a handler panicked.
- No label ever carries a user id, an email, a token, a raw path or a client IP.

## Logs

All of the server's own output is JSON on **stdout**, one object per line, ECS field names as flat
dotted keys (`LOG_FORMAT=text` switches to a one-line human form):

| Field | Value |
|---|---|
| `@timestamp` | RFC 3339 UTC, nanoseconds |
| `log.level` | `trace` `debug` `info` `warn` `error` (filtered by LOG_LEVEL) |
| `log.logger` | `server` (lifecycle, health transitions, drain) or `access` |
| `message` | human text |
| `ecs.version` | `8.11` |
| `service.name` / `service.version` | `auth-portal` / the OCI version label |
| `http.request.id` | the request id, on every line written inside a request |

Request id: an incoming `X-Request-Id` (1–128 chars of `[A-Za-z0-9._-]`) is kept, otherwise a
UUIDv4 is generated; it is echoed in the response header, forwarded to the backend on every
proxied call, and logged as `http.request.id` — the correlation key across proxy, frontend and
backend.

Access log (`ACCESS_LOG=true`, the default: this server is the edge of the application): one line
per request on PORT with `log.logger: access` and the fields `http.request.id`,
`http.request.method`, `url.path` (the route template from the enumeration above, never the raw
path), `http.response.status_code`, `event.duration` (nanoseconds), `http.request.bytes`,
`http.response.bytes`, and `upstream.duration` (nanoseconds spent in backend calls) when the request
was proxied. Probe and metrics hits on ADMIN_PORT are never logged; health transitions are logged as
state changes instead.

Never logged: request or response headers, bodies, query strings, cookies, tokens, passwords,
client IP addresses.

## Deployment requirements
- Stop grace period: grant at least **15 s** before SIGKILL (3 s drain delay + 10 s drain budget + margin)
- Writable paths: `/tmp` only (image runs read-only; mount tmpfs as the deployer sees fit)
- Init required: no (single process, no children)
- Capabilities required: none; no privilege escalation
- PID 1 exception: none — the Go binary is PID 1 via exec-form ENTRYPOINT; drain uses
  `http.Server.Shutdown` (stop intake, finish in-flight, bounded)

## Exit codes
0 clean shutdown · 1 drain deadline exceeded (connections force-closed) or fatal error ·
(137 observed = SIGKILL, grace period granted was below the documented requirement)

## Behavioral notes vs the Node variant
- Auth redirects happen on full document loads (as with Next middleware); client-side navigations
  are governed by the app's own fetch results, unchanged.
- Login/logout/me/signup responses mirror the upstream route handlers' status codes and bodies;
  the module routes relay the backend's status and body in both variants.
- HTTPS to the backend is supported (CA trust bundle baked, `SSL_CERT_FILE` set).

## Build host requirements
- podman with OCI image format; network to github.com, registry.npmjs.org, proxy.golang.org (Go
  modules, verified against the committed `go.sum`), and fonts.googleapis.com (`next/font/google`
  downloads fonts at build time — build stage only)
- the linux/arm64 half on an amd64 host requires qemu-user-static (binfmt) in the build VM for the
  Node build stage (the Go server cross-compiles natively)

## Publishing
```
podman manifest push --all --compression-format zstd:chunked --compression-level 19 --format oci \
  localhost/auth-portal:0.0.3 docker://<registry>/auth-portal:0.0.3
```
