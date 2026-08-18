# traefik — image contract

Image: localhost/traefik:0.0.1 (Traefik v3.7.10; retag for your registry — the OCI version
label keeps the packaged-software version, the tag is the artifact version)
UID:GID baked: 10023:10023 (ad-hoc assignment; override with `--build-arg APP_UID/APP_GID`)
Checker topology: checker is parent (`traefiksupervisor`, PID 1), traefik is its child
Checker runtime: static Go binary (no interpreter or extra runtime in the image)
Layer format: OCI, zstd:chunked (applied at push, see Publishing)

Content: the official Traefik v3.7.10 release binary (per-architecture tarball, sha256 pinned from
the release's checksums file), CA trust bundle and zoneinfo from a digest-pinned AlmaLinux 10
minimal stage, shipped `FROM scratch`. v3.7.10 includes the 2026-07-31 ACME/middleware security
fixes.

## Ports
| Port | Purpose                                                             |
|------|---------------------------------------------------------------------|
| 8080 | entrypoint `web` (publish as 80; HTTP + ACME HTTP-01 challenge)     |
| 8443 | entrypoint `websecure` (publish as 443; TLS + ACME TLS-ALPN-01)     |
| 9090 | ADMIN_PORT: /startupz /livez /readyz /metrics (supervisor)          |
| 9101 | entrypoint `metrics`: Traefik's own Prometheus metrics              |
| 8082 | internal only (127.0.0.1): entrypoint `ping` backing the checker    |

High ports keep the image non-root and capability-free; publishing 80->8080 / 443->8443 (or
`ip_unprivileged_port_start=0`, or NET_BIND_SERVICE) is the deployer's choice.

## Configuration (all runtime-overridable)
Traefik's entire static configuration is env-driven (`TRAEFIK_*`). Baked defaults:

| Env                                      | Default             | Notes                          |
|------------------------------------------|---------------------|--------------------------------|
| TRAEFIK_ENTRYPOINTS_WEB_ADDRESS          | :8080               |                                |
| TRAEFIK_ENTRYPOINTS_WEBSECURE_ADDRESS    | :8443               |                                |
| TRAEFIK_ENTRYPOINTS_PING_ADDRESS         | 127.0.0.1:8082      | keep aligned with PING_URL     |
| TRAEFIK_PING / TRAEFIK_PING_ENTRYPOINT   | true / ping         | required by the checker        |
| TRAEFIK_ENTRYPOINTS_METRICS_ADDRESS      | :9101               |                                |
| TRAEFIK_METRICS_PROMETHEUS(_ENTRYPOINT)  | true / metrics      |                                |
| TRAEFIK_PROVIDERS_FILE_DIRECTORY / _WATCH| /etc/traefik/dynamic / true | socket-free dynamic config |
| TRAEFIK_LOG_FORMAT / TRAEFIK_LOG_LEVEL   | json / INFO         |                                |

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

Supervisor knobs:

| Env                   | Required | Default                      | Notes                              |
|-----------------------|----------|------------------------------|------------------------------------|
| ADMIN_PORT            | optional | 9090                         |                                    |
| BIND_ADDR             | optional | 0.0.0.0                      | admin listener bind                |
| PING_URL              | optional | http://127.0.0.1:8082/ping   | must match the ping entrypoint     |
| ACME_STORAGE_PREPARE  | optional | /data/acme.json              | pre-created 0600; `off` disables   |
| LOG_LEVEL / LOG_FORMAT| optional | info / json                  | supervisor logs; traefik logs its own JSON |
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

## Deployment requirements
- Stop grace period: grant at least **20 s** before SIGKILL (3 s drain delay + 15 s drain budget + margin)
- Writable paths: `/data` (ACME storage; persistent mount, owner 10023), `/tmp` (tmpfs); image runs read-only
- Init required: no (the supervisor reaps)
- Capabilities required: none on the baked high ports; NET_BIND_SERVICE only if you reconfigure
  entrypoints to bind below 1024 inside the container
- Shutdown semantics: SIGTERM/SIGINT -> drain latch, delay, SIGTERM to traefik (its own graceful
  lifecycle applies); budget exhaustion escalates to SIGKILL and exits 1

## Exit codes
0 clean shutdown · non-zero: traefik exit status propagated, or 1 when the drain budget was
exceeded / a fatal error occurred · (137 observed = SIGKILL, grace period granted was below the
documented requirement)

## Build host requirements
- podman with OCI image format; network to github.com (release tarball) and the digest-pinned base
  registries
- the linux/arm64 half on an amd64 host requires qemu-user-static (binfmt) in the build VM
  (the Go supervisor cross-compiles natively; only the fetch stage runs emulated)

## Publishing
```
podman manifest push --all --compression-format zstd:chunked --compression-level 19 --format oci \
  localhost/traefik:0.0.1 docker://<registry>/traefik:0.0.1
```
