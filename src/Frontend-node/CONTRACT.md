# auth-portal (Node.js variant) — image contract

**Alternative variant.** The default auth-portal build is `src/Frontend` (static export served by a
compiled Go server, no Node runtime, ~10x smaller). Use this variant only when the full Next.js
server runtime is explicitly wanted.

Image: localhost/auth-portal:0.0.1-node (auth_portal 0.1.0; retag for your registry — the OCI
version label keeps the packaged-software version, the tag is the artifact version)
UID:GID baked: 10022:10022 (ad-hoc assignment; override with `--build-arg APP_UID/APP_GID`)
Checker topology: in-process (`ops/bootstrap.cjs` runs the Next.js standalone server, the checker
loop, the admin endpoints, and the signal handling in one Node process)
Checker runtime: reuses image runtime: Node.js 24
Layer format: OCI, zstd:chunked (applied at push, see Publishing)

Source: `github.com/yagan93/auth_portal` @ `599f7f8b0abcac739456d7bd95215c95b26706b6`, cloned during
the build and patched with `patches/0001-runtime-injectable-config.patch`. The patch makes the image
environment-neutral: the backend URL moved from build-time-inlined `NEXT_PUBLIC_API_URL` to the
runtime env `API_URL` (server-side only), the signup call is proxied through `/api/signup` so the
browser only ever talks same-origin, the jwt cookie `secure` flag became env-derived, the
third-party CDN avatar was replaced with a local asset, and `output: 'standalone'` was enabled.

Base: `docker.io/library/node:24-slim` (digest-pinned) — documented exception to the base policy:
a purpose-built runtime image chosen for the Node.js environment it ships. Consequence: the final
image contains a shell and a package manager (documented exception to the shell-less preference).

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
| LOG_LEVEL             | optional | info    | bootstrap logs; trace/debug/info/warn/error              |
| LOG_FORMAT            | optional | json    | bootstrap logs; Next.js's own output is unstructured     |
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

Probe command: `node /app/ops/probe.cjs --endpoint=<startupz|livez|readyz>` (exit 0/1);
it targets 127.0.0.1 for wildcard binds, otherwise the configured BIND_ADDR.

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
  localhost/auth-portal:0.0.1-node docker://<registry>/auth-portal:0.0.1-node
```
