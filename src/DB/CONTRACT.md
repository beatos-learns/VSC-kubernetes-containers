# postgresql — image contract

Image: localhost/postgresql:0.0.1 (PostgreSQL 16.15; retag for your registry — the OCI version
label keeps the packaged-software version, the tag is the artifact version)
UID:GID baked: 10020:10020, passwd entry `postgres` (ad-hoc assignment; override with `--build-arg APP_UID/APP_GID`)
Checker topology: checker is parent (`pgsupervisor`, PID 1), postgres postmaster is its child
Checker runtime: static Go binary (no interpreter or extra runtime in the image)
Layer format: OCI, zstd:chunked (applied at push, see Publishing)

Content: PostgreSQL 16.15 from the PGDG EL-10 RPMs (GPG-verified, exact NVR
`postgresql16-server-16.15-1PGDG.rhel10.2` pinned), assembled into a self-contained rootfs on a
digest-pinned AlmaLinux 10 minimal build stage and shipped `FROM scratch`. The RPM database is kept
in the image for SBOM/vulnerability scanners.

## Ports
| Env        | Default | Purpose                                  |
|------------|---------|------------------------------------------|
| PORT       | 5432    | PostgreSQL wire protocol                 |
| ADMIN_PORT | 9090    | /startupz /livez /readyz /metrics        |

## Configuration (all runtime-overridable)
| Env                    | Required        | Default                    | Notes                                          |
|------------------------|-----------------|----------------------------|------------------------------------------------|
| POSTGRES_PASSWORD_FILE | first init only | —                          | preferred: path to a mounted secret file       |
| POSTGRES_PASSWORD      | first init only | —                          | fallback when no file is given                 |
| POSTGRES_USER          | optional        | postgres                   | superuser name created by initdb               |
| POSTGRES_DB            | optional        | (postgres)                 | extra database created on first init           |
| PGDATA                 | optional        | /var/lib/postgresql/data   | must be, or lie under, a writable mount (created 0700 when missing) |
| POSTGRES_EXTRA_ARGS    | optional        | —                          | extra `-c key=value` args for the postmaster   |
| INITDB_SCRIPT_DIR      | optional        | /docker-entrypoint-initdb.d| `*.sql` run once on first init, sorted         |
| PG_BINDIR              | optional        | /usr/pgsql-16/bin          |                                                |
| BIND_ADDR              | optional        | 0.0.0.0                    | becomes `listen_addresses`                     |
| LOG_LEVEL              | optional        | info                       | supervisor logs; postgres logs are its own     |
| LOG_FORMAT             | optional        | json                       | supervisor logs; postgres logs stay text on stderr |
| HEALTH_CHECK_INTERVAL  | optional        | 5 s                        |                                                |
| HEALTH_CHECK_TIMEOUT   | optional        | 3 s                        | must be < interval (validated at startup)      |
| HEALTH_STALE_FACTOR    | optional        | 3                          |                                                |
| SHUTDOWN_DRAIN_DELAY   | optional        | 0 s                        | clients reconnect; nothing needs discovery time|
| SHUTDOWN_TIMEOUT       | optional        | 30 s                       | budget for fast shutdown incl. checkpoint      |

First initialization (empty PGDATA) requires the password; initdb runs with UTF8 / C.UTF-8 and
scram-sha-256 auth for host and local connections. An already-initialized PGDATA is started as-is —
init scripts and POSTGRES_DB never run again. First-init is failure-safe: an error or a
SIGTERM/SIGINT during initialization wipes the partial PGDATA (an in-progress marker makes an
interrupted provisioning phase reinitialize on the next start). Only a SIGKILL landing inside the
few-second initdb window can leave an undetected partial cluster — if the first start was
hard-killed and the server refuses to start, clear the volume. During initialization `/livez` is
200 and `/startupz`/`/readyz` are 503 — probe liveness only after startup succeeded, or grant the
startup probe the worst-case duration of your init scripts.

## Health checks registered
| Check      | Verifies                                             |
|------------|------------------------------------------------------|
| postgresql | `pg_isready` handshake against 127.0.0.1:PORT        |

The check is performed in-process (PostgreSQL startup handshake over TCP), so the supervisor spawns
no per-cycle child processes. A dead postmaster additionally terminates the container immediately
with the child's exit status — no lying green. Probe command:
`/usr/local/bin/pgsupervisor healthcheck --endpoint=<startupz|livez|readyz>` (exit 0/1);
it targets 127.0.0.1 for wildcard binds, otherwise the configured BIND_ADDR.

## Deployment requirements
- Stop grace period: grant at least **40 s** before SIGKILL (0 s drain delay + 30 s drain budget
  + 5 s SIGQUIT escalation window + margin)
- Writable paths: PGDATA mount (named volume or chown-ed to 10020:10020; mounting the writable
  volume at the PARENT of PGDATA also works — the supervisor creates a missing PGDATA 0700, which
  is the layout for orchestrator-provisioned volumes whose mount root stays root-owned and may
  contain `lost+found`), `/tmp` (unix socket + temp files; tmpfs); image runs read-only
- Init required: no (the supervisor reaps)
- Capabilities required: none; no privilege escalation (verified with `--read-only --cap-drop=ALL --security-opt=no-new-privileges`)
- Shutdown semantics: SIGTERM and SIGINT both trigger drain, then a PostgreSQL *fast* shutdown
  (SIGINT to the postmaster); budget exhaustion escalates to SIGQUIT and exits 1
- Seeding for user-mgmt-service: mount a `.sql` file into /docker-entrypoint-initdb.d to create the
  `USER_MODIFY` / `USER_DELETE` authorities the backend expects but does not seed

## Exit codes
0 clean shutdown · non-zero: postgres exit status propagated, or 1 when the drain budget was
exceeded / a fatal error occurred · (137 observed = SIGKILL, grace period granted was below the
documented requirement)

## Build host requirements
- podman with OCI image format; network to download.postgresql.org and the Go build stage's base
  image registries (all bases digest-pinned)
- the linux/arm64 half on an amd64 host requires qemu-user-static (binfmt) in the build VM
  (the Go supervisor cross-compiles natively; only the RPM assembly stage runs emulated)

## Publishing
```
podman manifest push --all --compression-format zstd:chunked --compression-level 19 --format oci \
  localhost/postgresql:0.0.1 docker://<registry>/postgresql:0.0.1
```
