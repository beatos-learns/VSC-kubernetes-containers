# auth-portal — image contract

**Default variant.** A Node.js-runtime variant of this image exists at `src/Frontend-node`
(tag `:0.0.1-node`); this build replaces it as the default.

Image: localhost/auth-portal:0.0.1 (auth_portal 0.1.0; retag for your registry — the OCI version
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

Source: `github.com/yagan93/auth_portal` @ `599f7f8b0abcac739456d7bd95215c95b26706b6`, cloned during
the build and patched with `patches/0001-static-export.patch` (static export enabled, api routes and
middleware removed in favor of the Go server, signup routed same-origin, root page made a client
redirect, third-party CDN asset localized).

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
| LOG_FORMAT            | optional | json        |                                                          |
| HEALTH_CHECK_INTERVAL | optional | 5 s         | sized for an interactive UI behind the proxy             |
| HEALTH_CHECK_TIMEOUT  | optional | 2 s         | must be < interval (validated at startup)                |
| HEALTH_STALE_FACTOR   | optional | 3           |                                                          |
| SHUTDOWN_DRAIN_DELAY  | optional | 3 s         | lets the proxy discover not-ready                        |
| SHUTDOWN_TIMEOUT      | optional | 10 s        | bound for draining in-flight requests                    |

Startup validates the whole configuration; a missing/invalid value logs one clear error and exits
non-zero immediately. One line logs name, version, revision, and the effective configuration when
the server starts listening.

## Health checks registered
| Check       | Verifies                                                                     |
|-------------|------------------------------------------------------------------------------|
| static-root | the exported bundle is present and readable (STATIC_DIR/index.html)          |
| backend-api | `${API_URL}/users` answers ANY HTTP response (reachability; 401/403 count as reachable) |

Probe command: `/app/auth-portal-server healthcheck --endpoint=<startupz|livez|readyz>` (exit 0/1);
it targets 127.0.0.1 for wildcard binds, otherwise the configured BIND_ADDR.

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
- Login/logout/me/signup responses mirror the upstream route handlers' status codes and bodies.
- HTTPS to the backend is supported (CA trust bundle baked, `SSL_CERT_FILE` set).

## Build host requirements
- podman with OCI image format; network to github.com, registry.npmjs.org, and
  fonts.googleapis.com (`next/font/google` downloads fonts at build time — build stage only)
- the linux/arm64 half on an amd64 host requires qemu-user-static (binfmt) in the build VM for the
  Node build stage (the Go server cross-compiles natively)

## Publishing
```
podman manifest push --all --compression-format zstd:chunked --compression-level 19 --format oci \
  localhost/auth-portal:0.0.1 docker://<registry>/auth-portal:0.0.1
```
