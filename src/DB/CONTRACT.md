# postgresql — image contract

Image: localhost/postgresql:0.0.2 (PostgreSQL 16.15; retag for your registry — the OCI version
label keeps the packaged-software version, the tag is the artifact version)
UID:GID baked: 10020:10020, passwd entry `postgres` (ad-hoc assignment; override with `--build-arg APP_UID/APP_GID`)
Checker topology: checker is parent (`pgsupervisor`, PID 1), postgres postmaster is its child
Checker runtime: static Go binary (no interpreter, exporter sidecar or extra runtime in the image)
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
| POSTGRES_PASSWORD_FILE | first init only | —                          | preferred: path to a mounted secret file; at runtime it also unlocks the `pg_*` statistics (see Metrics) |
| POSTGRES_PASSWORD      | first init only | —                          | fallback when no file is given                 |
| POSTGRES_USER          | optional        | postgres                   | superuser name created by initdb; the statistics sampler connects as this user |
| POSTGRES_DB            | optional        | (postgres)                 | extra database created on first init           |
| PGDATA                 | optional        | /var/lib/postgresql/data   | must be, or lie under, a writable mount (created 0700 when missing) |
| POSTGRES_EXTRA_ARGS    | optional        | —                          | extra `-c key=value` args for the postmaster; appended last, so they override every baked setting including the logging ones below |
| INITDB_SCRIPT_DIR      | optional        | /docker-entrypoint-initdb.d| `*.sql` run once on first init, sorted         |
| PG_BINDIR              | optional        | /usr/pgsql-16/bin          |                                                |
| BIND_ADDR              | optional        | 0.0.0.0                    | becomes `listen_addresses`                     |
| LOG_LEVEL              | optional        | info                       | supervisor logs; trace/debug/info/warn/error   |
| LOG_FORMAT             | optional        | json                       | supervisor logs: `json` (ECS) or `text`; postgres logs stay text on stderr |
| LOG_MIN_DURATION_MS    | optional        | -1                         | `log_min_duration_statement` in ms: -1 off, 0 every statement, N slower than N ms |
| LOG_CONNECTIONS        | optional        | false                      | `log_connections` + `log_disconnections`       |
| HEALTH_CHECK_INTERVAL  | optional        | 5 s                        | also the statistics sampling interval          |
| HEALTH_CHECK_TIMEOUT   | optional        | 3 s                        | must be < interval (validated at startup); bounds check and sampling together |
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

## Metrics

`/metrics` on ADMIN_PORT serves OpenMetrics (`application/openmetrics-text; version=1.0.0`, a
`# TYPE` line per family, `# EOF` terminator) in the shape the repository README's *Metrics
contract* section defines for every image; the families below are the whole exposition. The
baseline families are served from process start, before the database is up.

### Baseline (supervisor state)

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `build_info` | gauge | `version`, `revision` | always 1; the running artifact (OCI version label and VCS revision) |
| `health_check_up` | gauge | `check` (`postgresql`) | 1 = the check passed in the last checker cycle |
| `health_check_duration_seconds` | gauge | `check` | duration of the last run of that check (the handshake) |
| `health_check_last_success_timestamp_seconds` | gauge | `check` | unix time the check last passed; 0 until it passed once |
| `health_snapshot_age_seconds` | gauge | — | age of the cached snapshot the probe endpoints read; 0 before the first cycle |
| `health_checker_cycles_total` | counter | — | completed checker cycles since process start |
| `health_draining` | gauge | — | 1 once SIGTERM/SIGINT latched draining (`/readyz` is 503 from then on) |
| `supervisor_child_up` | gauge | — | 1 while the postmaster child runs (0 = the container is exiting) |
| `supervisor_child_start_time_seconds` | gauge | — | unix time the postmaster was started; 0 before its start |
| `go_*`, `process_*` | runtime | — | Go runtime and process collectors of the supervisor (`process_start_time_seconds`, `process_resident_memory_bytes`, `go_goroutines`, ...) |

### Database (sampled every HEALTH_CHECK_INTERVAL)

The checker's own wire-protocol connection is reused: reaching the authentication request is the
health check; with the password at hand (`POSTGRES_PASSWORD_FILE` / `POSTGRES_PASSWORD`, always
present in a deployment that mounts its secret) the same connection then authenticates as
`POSTGRES_USER` (SCRAM-SHA-256, md5 and cleartext requests are understood; the password is used as
given, so keep it ASCII) with `application_name=pgsupervisor`, reads the views below through the
simple-query protocol and terminates — one short session per cycle, bounded by
`HEALTH_CHECK_TIMEOUT`, no exporter process. Without the password only `pg_up` and the
`pg_stats_*` state families are served (`pg_stats_up` stays 0) and one warning is logged at
startup. The `pg_stat_*`, `pg_database_*`, `pg_settings_*` and `pg_locks` families are omitted
while `pg_stats_up` is 0, never served stale.

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `pg_up` | gauge | — | 1 = the postmaster accepted the handshake in the last cycle (same signal as `health_check_up{check="postgresql"}`) |
| `pg_stats_up` | gauge | — | 1 = the last statistics sampling succeeded (authentication and all queries) |
| `pg_stats_last_success_timestamp_seconds` | gauge | — | unix time of the last successful sampling; 0 until it succeeded once |
| `pg_stats_scrape_duration_seconds` | gauge | — | duration of the last sampling (authentication and queries) |
| `pg_stat_database_numbackends` | gauge | `datname` | backends connected to the database (`pg_stat_database`; the sampler's own session counts against `postgres`) |
| `pg_stat_database_xact_commit_total`, `pg_stat_database_xact_rollback_total` | counter | `datname` | transactions committed / rolled back |
| `pg_stat_database_blks_hit_total`, `pg_stat_database_blks_read_total` | counter | `datname` | buffer-cache hits / disk reads (cache hit ratio = hit / (hit + read)) |
| `pg_stat_database_tup_{fetched,inserted,updated,deleted}_total` | counter | `datname` | rows fetched / inserted / updated / deleted |
| `pg_stat_database_deadlocks_total` | counter | `datname` | deadlocks detected |
| `pg_database_size_bytes` | gauge | `datname` | on-disk size (`pg_database_size`) |
| `pg_stat_activity_backends` | gauge | `state` | server processes by state (`pg_stat_activity`), one series per state of the fixed enumeration `active`, `idle`, `idle in transaction`, `idle in transaction (aborted)`, `fastpath function call`, `disabled`; 0 when absent |
| `pg_settings_max_connections` | gauge | — | the `max_connections` setting (saturation = sum of `numbackends` / this) |
| `pg_locks` | gauge | `mode` | locks held or awaited (`pg_locks`), one series per mode of the fixed enumeration `AccessShareLock`, `RowShareLock`, `RowExclusiveLock`, `ShareUpdateExclusiveLock`, `ShareLock`, `ShareRowExclusiveLock`, `ExclusiveLock`, `AccessExclusiveLock`; 0 when absent |

`datname` covers every non-template database (`postgres`, `POSTGRES_DB`, anything created later).
Label values are fixed enumerations; a new value is a contract change. No label ever carries a
user, a client address, a query or a password.

## Logs

Supervisor lines go to **stdout**, one JSON object per line with ECS field names
(`LOG_FORMAT=json`, the default) or one human-readable line (`LOG_FORMAT=text`):

| Field | Value |
|---|---|
| `@timestamp` | RFC 3339 UTC, nanoseconds |
| `log.level` | `trace` / `debug` / `info` / `warn` / `error` (threshold `LOG_LEVEL`) |
| `log.logger` | `supervisor` |
| `message` | the text |
| `ecs.version` | `8.11` |
| `service.name` / `service.version` | `postgresql` / the OCI version label (`APP_VERSION`) |
| `process.pid` | on the startup line: the postmaster's pid |
| `health.check`, `health.up` | on health transitions |
| `pg.stats.up` | on statistics-sampling transitions |

Events at `info`: the startup line (version, revision, postmaster pid, the effective non-secret
configuration), `empty PGDATA: running initdb`, first-init provisioning and init-script lines,
`startup complete`, health-check and sampling transitions, the drain sequence, graceful shutdown.
`warn`: interrupted-initialization recovery, drain-budget escalation, statistics disabled for lack
of a password. `error`: initdb/provisioning failures, an unexpectedly exited postmaster, fatal
configuration errors after startup (the very first configuration parse still reports
`fatal configuration error: ...` as plain text on stderr and exits 1). Probe hits are never logged.

PostgreSQL's own log stays on **stderr** as text (`log_destination=stderr` and
`logging_collector=off` are baked because the PGDG configuration template turns the collector
on, which would silently divert the log into files under PGDATA/log) with the baked prefix
`%m [%p] %q%u@%d %a %r ` — timestamp with milliseconds, backend
pid, then for client backends `user@database application_name remote_host:port` — plus
`log_checkpoints=on`, `log_lock_waits=on`, `log_temp_files=0` (every temp file),
`log_autovacuum_min_duration=0` (every autovacuum run); `LOG_MIN_DURATION_MS` and
`LOG_CONNECTIONS` map to `log_min_duration_statement` and `log_connections`/`log_disconnections`.
The sampler's cycles appear there only with `LOG_CONNECTIONS=true` (as
`POSTGRES_USER@postgres pgsupervisor 127.0.0.1(port)`). `POSTGRES_EXTRA_ARGS` overrides any of
these. initdb and provisioning output also goes to stderr.

Never logged: the password (the startup summary prints `POSTGRES_PASSWORD=<redacted>`, the initdb
password file is a temporary file removed right after initdb), statement text unless
`LOG_MIN_DURATION_MS` asks for it.

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
- Seeding for user-mgmt-service: mount a `.sql` file into /docker-entrypoint-initdb.d that creates
  the `USER_MODIFY` / `USER_DELETE` authorities and a role holding them; it runs once on first init
- Metrics: scrape ADMIN_PORT `/metrics`; keep the password mounted at runtime (not only for the
  first start) or the database families stay absent

## Exit codes
0 clean shutdown · non-zero: postgres exit status propagated, or 1 when the drain budget was
exceeded / a fatal error occurred · (137 observed = SIGKILL, grace period granted was below the
documented requirement)

## Build host requirements
- podman with OCI image format; network to download.postgresql.org, proxy.golang.org (supervisor
  modules, checksums in `supervisor/go.sum`) and the Go build stage's base image registries (all
  bases digest-pinned)
- the linux/arm64 half on an amd64 host requires qemu-user-static (binfmt) in the build VM
  (the Go supervisor cross-compiles natively; only the RPM assembly stage runs emulated)

## Publishing
```
podman manifest push --all --compression-format zstd:chunked --compression-level 19 --format oci \
  localhost/postgresql:0.0.2 docker://<registry>/postgresql:0.0.2
```
