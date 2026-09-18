# module-service — image contract

Image: localhost/module-service:0.0.2 (module_service 0.1.0; retag for your registry — the OCI version
label keeps the packaged-software version, the tag is the artifact version)
UID:GID baked: 10024:10024 (ad-hoc assignment; override with `--build-arg APP_UID/APP_GID`)
Checker topology: in-process (one native executable serves the API, the checker loop, the admin
endpoints, the signal handling and the probe subcommand)
Checker runtime: none — in-process (the Python runtime is linked into the executable; no
interpreter, shell or package manager in the image)
Layer format: OCI, zstd:chunked (applied at push, see Publishing)

Content: the upstream FastAPI service (uvicorn, SQLAlchemy, PyMySQL) is compiled with Nuitka into
one native executable `/app/module-service`: every Python module of the application, of the ops
layer added by the patch and of every pure-Python dependency is compiled to C and linked together
with a static `libpython` (CPython 3.14 of Alpine's `python3-dev`). Binary extension modules stay
shared objects next to the executable (pydantic-core, uvloop, httptools, SQLAlchemy's C helpers,
cryptography, cffi, the CPython standard-library extension modules) together with the libraries
they need (OpenSSL, libffi, ...). The final image is `FROM scratch` and additionally holds only
the musl loader (`/lib/ld-musl-<arch>.so.1`), the CA bundle and `/tmp`. No `.py` file, no
interpreter binary and no shell ship — the build stage asserts this on the output tree
(`patchelf --print-needed` closure, no `*.py`/`*.pyc`, no `libpython` dependency), and the
acceptance run asserts it again on the exported image.

The executable is linked with `-Wl,--export-dynamic` (passed to Nuitka through `LDFLAGS`):
with a static libpython the extension modules resolve the Python C API from the executable's
dynamic symbol table, and Nuitka adds that flag on its own only for Debian- and Arch-based
systems, not for Alpine's `/usr` Python.

Assembly (`assemble.sh`, run in the build stage after the compile): resolves the dependency
closure of the executable and every shared object with `patchelf --print-needed`, copies the
libraries the compiler left out from the base (Nuitka treats zlib as a system library), fails on
anything unresolvable, asserts that every `Py*`/`_Py*` symbol any extension module imports is
exported by the executable (`nm -D`), strips the shared objects, and prepares what vulnerability scanners need
to see the image's contents: `/lib/apk/db/installed` pruned to exactly the Alpine packages whose
files ship (musl, python3 for the linked runtime, the OpenSSL, zlib, bzip2, xz, expat, mpdecimal,
sqlite and libffi libraries, the CA bundle), `/etc/alpine-release` and `/etc/os-release`, and the
`METADATA` file of every Python distribution of the compiled dependency set under
`/app/site-packages/`. Trivy therefore reports the OS packages and the Python packages of the
image, and syft's SBOM lists both; without these files a compiled image is opaque to scanners.

Base choice (recorded per the build standard): the build stage is `docker.io/library/alpine:3.24`
(digest-pinned), chosen for the environment it provides — a musl toolchain with a static
`libpython3.14.a` and musllinux wheels for every binary dependency of the lock. No purpose-built
Python image ships that archive (the official `python:*-alpine` images delete it) and a musl
toolchain cannot be assembled on the AlmaLinux (glibc) base. The final stage is `scratch`.

Source: `github.com/yagan93/module_service` @ `e86baa4aa0f0bc53b0685edbb05a1c3a8d4b9494`, cloned
during the build and patched with `patches/0001-container-build-standard.patch`: the `app.ops`
package (env configuration, ECS JSON logging, OpenMetrics registry, cached health checker, admin
listener, route instrumentation, probe command, drain sequence), `app/__main__.py` as the entry
point, `DATABASE_URL_FILE`, `MYSQL_SSL_MODE`/`MYSQL_SSL_CA` and the pool knobs in `app/config.py`
and `app/database.py` (the upstream boolean `MYSQL_SSL_DISABLED` became the `MYSQL_SSL_MODE`
enumeration), and the removal of upstream's `logging.basicConfig`; then with
`patches/0002-database-readiness.patch` (startup completes without the database, see Health
checks). The REST API, models, schemas and repository are unchanged. Dependencies come from the
upstream `uv.lock` (hash-verified, `uv sync --frozen`) with one change made by `0001`: `starlette` is constrained to
`>=1.3.1,<2` and re-locked (0.52.1 → 1.6.0), because the locked 0.52.1 carries fixed
vulnerabilities the CI scan gate rejects; FastAPI 0.141.1 allows any Starlette `>=0.46`.
`watchfiles`, `websockets`, `pyyaml` and `greenlet` of the lock are not installed (reload
support, WebSocket protocol, YAML log configuration and SQLAlchemy's asyncio extension are
never used by the compiled service). The build stage upgrades the base's packages to the
current patch level of the Alpine 3.24 branch before installing the toolchain, so the shipped
OpenSSL, zlib and musl are the branch's current builds at build time (the scanner inventory
records the exact versions). Nuitka and patchelf are build-stage tools pinned by the
`NUITKA_VERSION` / `PATCHELF_VERSION` build args.

## Contract facts (authoritative; the tables below are its rendering)

```json contract-facts
{
  "contract_version": 1,
  "image": {"name": "localhost/module-service", "version": "0.0.2"},
  "identity": {"uid": 10024, "gid": 10024},
  "ports": {"http": 8080, "admin": 9090},
  "probes": {"port": "admin", "startup": "/startupz", "liveness": "/livez",
             "readiness": "/readyz", "command": ["/app/module-service", "healthcheck"]},
  "metrics": {"path": "/metrics", "port": "admin",
              "format": "application/openmetrics-text; version=1.0.0"},
  "shutdown": {"grace_seconds": 15},
  "filesystem": {"read_only_root": true, "writable_paths": ["/tmp"]},
  "security": {"capabilities": [], "init_required": false},
  "env": {"PORT": "8080", "ADMIN_PORT": "9090", "BIND_ADDR": "0.0.0.0",
          "LOG_LEVEL": "info", "LOG_FORMAT": "json", "ACCESS_LOG": "false",
          "HEALTH_CHECK_INTERVAL": "5", "HEALTH_CHECK_TIMEOUT": "2", "HEALTH_STALE_FACTOR": "3",
          "SHUTDOWN_DRAIN_DELAY": "3", "SHUTDOWN_TIMEOUT": "10",
          "DATABASE_URL": "", "DATABASE_URL_FILE": "",
          "MYSQL_SSL_MODE": "preferred", "MYSQL_SSL_CA": "",
          "DB_CONNECT_TIMEOUT": "5", "DB_POOL_SIZE": "5", "DB_POOL_MAX_OVERFLOW": "10",
          "DB_POOL_RECYCLE": "300", "APP_VERSION": "0.1.0"}
}
```

## Ports
| Env        | Default | Purpose                                                              |
|------------|---------|----------------------------------------------------------------------|
| PORT       | 8080    | HTTP REST API: `/api/v1/modules`, `/api/v1/modules/{module_id}`, `/docs`, `/openapi.json`, `/redoc` |
| ADMIN_PORT | 9090    | /startupz /livez /readyz /metrics                                    |

## Configuration (all runtime-overridable)
| Env                   | Required          | Default   | Notes                                                    |
|-----------------------|-------------------|-----------|----------------------------------------------------------|
| DATABASE_URL_FILE     | one of the two    | —         | preferred: path to a mounted file holding the connection URL (the URL carries the password); wins over DATABASE_URL |
| DATABASE_URL          | one of the two    | —         | `mysql+pymysql://<user>:<password>@<host>:<port>/<database>?charset=utf8mb4`; `sqlite://` is accepted for local use only |
| MYSQL_SSL_MODE        | optional          | preferred | `disabled` (plain TCP), `preferred` (TLS when the server offers it, certificate not verified), `required` (TLS mandatory, not verified), `verify-ca` (chain verified against MYSQL_SSL_CA), `verify-identity` (chain and host name) — the vocabulary of the MySQL client's `--ssl-mode` |
| MYSQL_SSL_CA          | verify-* modes    | —         | path to a PEM CA file (mount it from a Secret); validated as a readable file at startup |
| DB_CONNECT_TIMEOUT    | optional          | 5 s       | bound per connection attempt (TCP + handshake)           |
| DB_POOL_SIZE          | optional          | 5         | SQLAlchemy pool size (connections kept open)             |
| DB_POOL_MAX_OVERFLOW  | optional          | 10        | additional connections opened under load and closed again |
| DB_POOL_RECYCLE       | optional          | 300 s     | connections older than this are replaced (-1 never); every checkout is pre-pinged |
| APP_VERSION           | baked             | 0.1.0     | packaged-software version (build arg VERSION); shown by `/docs` and `build_info` |
| BIND_ADDR             | optional          | 0.0.0.0   |                                                          |
| LOG_LEVEL             | optional          | info      | trace/debug/info/warn/error                              |
| LOG_FORMAT            | optional          | json      | `json` (ECS) or `text`                                   |
| ACCESS_LOG            | optional          | false     | one JSON line per request on PORT (see Logs); never for ADMIN_PORT hits |
| HEALTH_CHECK_INTERVAL | optional          | 5 s       | sized for a small API in front of a managed database     |
| HEALTH_CHECK_TIMEOUT  | optional          | 2 s       | must be < interval (validated at startup)                |
| HEALTH_STALE_FACTOR   | optional          | 3         |                                                          |
| SHUTDOWN_DRAIN_DELAY  | optional          | 3 s       | lets the callers' endpoint slices discover not-ready     |
| SHUTDOWN_TIMEOUT      | optional          | 10 s      | bound for draining in-flight requests                    |

Startup validates the whole configuration; a missing or invalid value prints one
`fatal configuration error: ...` line on stderr and exits 1 before any listener binds. One line
logs name, version, revision, the route templates and the effective configuration (password
masked) once both listeners are up. Upstream's `.env` file support stays in the code but no such
file exists in the image (the working directory is `/`).

## Health checks registered
| Check    | Kind       | Verifies                                                                              |
|----------|------------|---------------------------------------------------------------------------------------|
| database | dependency | `SELECT 1` through the connection pool: the MySQL is reachable and authenticates      |
| schema   | dependency | `SELECT 1 FROM modules LIMIT 1`: the upstream `schema.sql` has been applied (the service runs no migrations) |

Both run concurrently every HEALTH_CHECK_INTERVAL in dedicated threads, each bounded by
HEALTH_CHECK_TIMEOUT; a check whose previous run is still stuck is reported failed instead of
piling up threads. The checker starts once both listeners are up, and `/startupz` latches on its
first completed cycle, whatever the checks report: the database is never a startup condition.
Both checks gate `/readyz` only, never `/startupz` or `/livez` (snapshot staleness only): an
unreachable MySQL, refused credentials or a missing schema take the pod out of load balancing,
the container is never restarted for it, and the pool reconnects by itself. Transitions are
logged: `health check 'database' transitioned True -> False (...)` and
`readiness True -> False (failing checks: [...])`. `?verbose=1` on any probe returns the
per-check JSON (status, detail, duration, last success).

Probe command: `/app/module-service healthcheck --endpoint=<startupz|livez|readyz>` (exit 0/1);
it targets 127.0.0.1 for wildcard binds, otherwise the configured BIND_ADDR. The probe path
imports nothing of the application, so it answers within a few milliseconds.

## Metrics

`GET /metrics` on ADMIN_PORT serves the stack-wide metrics contract of the repository README
("Metrics contract") as OpenMetrics (`application/openmetrics-text; version=1.0.0; charset=utf-8`,
a `# HELP` and `# TYPE` line per family, `# EOF` terminator), unconditionally — no content
negotiation. The exposition is written by the service itself (no client library); the baseline
families are served from the moment the admin listener is up, before the application is ready.

Baseline and runtime:

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `build_info` | gauge | `version`, `revision` | always 1; the running artifact (OCI version label, VCS revision) |
| `health_check_up` | gauge | `check` = `database` \| `schema` | 1 = the check passed in the last cycle |
| `health_check_duration_seconds` | gauge | `check` | duration of the last run of that check |
| `health_check_last_success_timestamp_seconds` | gauge | `check` | unix time the check last passed; 0 until then |
| `health_snapshot_age_seconds` | gauge | — | age of the cached snapshot (staleness flips `/livez`) |
| `health_checker_cycles_total` | counter | — | completed checker cycles |
| `health_draining` | gauge | — | 1 once the shutdown drain latch is set |
| `process_start_time_seconds`, `process_cpu_seconds_total`, `process_resident_memory_bytes`, `process_virtual_memory_bytes`, `process_open_fds`, `process_max_fds` | gauge / counter | — | the process as `/proc` and `getrusage` report it |
| `python_info` | gauge | `implementation`, `version` | the CPython runtime linked into the executable |
| `python_threads` | gauge | — | live threads (event loop, worker pool, checker threads) |
| `python_gc_collections_total`, `python_gc_objects_collected_total`, `python_gc_objects_uncollectable_total` | counter | `generation` = `0` \| `1` \| `2` | garbage collector statistics |

Routes on PORT (the server side of the backend -> modules hop):

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `http_server_requests_seconds_{count,sum,bucket}` | histogram | `method`, `uri`, `status`, `outcome`, `exception` | request rate, error rate and latency per route; buckets `0.005 0.01 0.025 0.05 0.1 0.25 0.5 1 2.5 5 10` |
| `http_server_requests_active` | gauge | `method`, `uri` | requests in flight per route (the route is known before the handler runs) |
| `http_server_request_bytes_total`, `http_server_response_bytes_total` | counter | `uri` | body bytes read / written per route (pre-created at 0 for every route class) |
| `http_server_connections_active` | gauge | `listener` = `main` \| `admin` | open TCP connections per listener, idle keep-alive included |

The database hop (SQLAlchemy cursor events and pool state; the MySQL is not a stack component,
so there is no `peer` label):

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `db_client_statements_seconds_{count,sum,bucket}` | histogram | `operation` = `select` \| `insert` \| `update` \| `delete` \| `other` | duration of every SQL statement, health checks included; same buckets |
| `db_client_statement_errors_total` | counter | `operation` | statements the driver answered with an error (a duplicate `code` counts as one `insert` error); pre-created at 0 |
| `db_pool_connections` | gauge | `state` = `checked_out` \| `idle` \| `overflow` | pool state at scrape time |
| `db_pool_size` | gauge | — | the configured `DB_POOL_SIZE` |

Label enumerations (a new value is a contract change):
- `uri`: `/api/v1/modules`, `/api/v1/modules/{module_id}`, `/openapi.json`, `/docs`,
  `/docs/oauth2-redirect`, `/redoc`, `UNKNOWN` (anything that matches no route — a scan creates
  no series). A known route hit with a method it does not serve (405) keeps its template.
- `method`: `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`, else `OTHER`.
- `status`: the numeric status code; `0` when the client disconnected before a response started.
  `outcome`: `INFORMATIONAL`, `SUCCESS`, `REDIRECTION`, `CLIENT_ERROR`, `SERVER_ERROR`, `UNKNOWN`.
  `exception`: `none`, or the class name of the exception that escaped the application (the
  client then received Starlette's 500).
- No label ever carries a user id, a module id, a raw path or a client IP.

## Logs

All of the process's own output is JSON on **stdout**, one object per line, ECS field names as
flat dotted keys (`LOG_FORMAT=text` switches to a one-line human form):

| Field | Value |
|---|---|
| `@timestamp` | RFC 3339 UTC, microseconds |
| `log.level` | `trace` `debug` `info` `warn` `error` (filtered by LOG_LEVEL) |
| `log.logger` | `app.ops.server` (lifecycle, drain), `app.ops.health` (startup latch, transitions), `access`, `uvicorn.error` (server warnings and unhandled exceptions), the application's own loggers |
| `message` | human text |
| `ecs.version` | `8.11` |
| `service.name` / `service.version` | `module-service` / the OCI version label |
| `process.pid`, `process.thread.name` | the process and the thread that wrote the line |
| `http.request.id` | the request id, on every line written inside a request (worker threads included) |
| `error.type`, `error.message`, `error.stack_trace` | on lines that carry an exception |

Request id: an incoming `X-Request-Id` (1–128 characters of `[A-Za-z0-9._-]`) is kept, otherwise
a UUIDv4 is generated; it is echoed in the `X-Request-Id` response header and logged as
`http.request.id` — the correlation key with the calling backend, which forwards its own id.

Access log (`ACCESS_LOG=true`; default `false`, the route metrics cover the steady state): one
`info` line per request on PORT with `log.logger: access` and `http.request.id`,
`http.request.method`, `url.path` (the route template from the enumeration above, never the raw
path), `http.response.status_code`, `event.duration` (nanoseconds), `http.request.bytes`,
`http.response.bytes`. Probe and metrics hits on ADMIN_PORT are never logged; health transitions
are logged as state changes instead. uvicorn's own lifecycle chatter is suppressed; its warnings
and errors (an unhandled exception with its stack trace, an unsupported upgrade request) pass
through in the same JSON shape.

Never logged: request or response headers, bodies, query strings, the database password (the
startup line masks it), client IP addresses.

## Deployment requirements
- Stop grace period: grant at least **15 s** before SIGKILL (3 s drain delay + 10 s drain budget + margin)
- Writable paths: `/tmp` only (image runs read-only; mount tmpfs as the deployer sees fit)
- Init required: no (single process; the checker runs in threads, not child processes)
- Capabilities required: none; no privilege escalation
- PID 1 exception: none — the native executable is PID 1 via exec-form ENTRYPOINT; drain stops
  the listener, finishes in-flight requests (bounded by SHUTDOWN_TIMEOUT), then disposes the pool
- Database: an operator-managed MySQL 8 (a DigitalOcean managed database in the target landscape)
  with the upstream `schema.sql` applied once; the service never creates or migrates tables.
  MySQL 8's `caching_sha2_password` authentication works in every `MYSQL_SSL_MODE` (RSA key
  exchange on plain connections). For the managed database use `verify-identity` with its CA.
- Concurrency model: one process, one event loop (uvloop); the upstream endpoints are synchronous
  and run in the worker-thread pool (40 threads), each holding at most one pooled connection, so
  `DB_POOL_SIZE + DB_POOL_MAX_OVERFLOW` bounds the database connections. Scale vertically with
  CPU and memory limits; the process uses one core for the event loop plus the worker threads.

## Exit codes
0 clean shutdown · 1 drain deadline exceeded (open connections abandoned) or fatal configuration
error · 2 unknown command-line argument · 3 a listener could not bind (startup failure) ·
(137 observed = SIGKILL, grace period granted was below the documented requirement)

## Build host requirements
- podman with OCI image format; network to github.com, dl-cdn.alpinelinux.org, pypi.org and
  files.pythonhosted.org (wheels hash-verified against the upstream lock)
- the Nuitka compile (591 C translation units) takes roughly 13 minutes on 4 cores cold and about
  3 minutes with the Nuitka ccache mount warm; about 2 GB of memory
- the linux/arm64 half on an amd64 host requires qemu-user-static (binfmt) in the build VM and is
  slow under emulation (a native arm64 builder is the fast path; CI builds it natively)

## Verification status
Verified on linux/amd64 with the delivered image (115.7 MB, 139 files, 72 shared objects)
against a MySQL 8.4 container, under `--read-only --cap-drop=ALL
--security-opt=no-new-privileges` (image user 10024) with a tmpfs `/tmp`:
- startup without a database (the MySQL host name does not even resolve): `/startupz` and
  `/livez` 200, `/readyz` 503 with the cause in `?verbose=1`, the probe command exit 0 / 0 / 1
  (exit 1 for an unknown endpoint), no restart; a MySQL without the schema keeps `/readyz` at
  503 (`schema` failing) until `schema.sql` is applied, ready 2 s later
- readiness 503 with liveness 200 within a second of the database container stopping, both
  transitions logged, ready again 4 s after it is back
- `/metrics`: the media type, `# EOF`, `build_info`, `health_check_up` per check
- the CRUD round trip with the upstream status codes (201, 409, 200, 404, 422, 204, 405)
- SIGTERM: exit 0 inside the grace period; every log line parseable JSON
- fail-fast: invalid configurations exit 1 with one `fatal configuration error` line, an unknown
  argument exits 2
- the exported image tree: no shell, no package manager, no interpreter, no `.py`; the
  CI-equivalent Trivy gate (HIGH+CRITICAL, unfixed ignored) passes
Verified with the 0.0.1 build, whose file tree and code paths this image shares except for when
startup completes and when the checker starts: the five TLS modes against MySQL's
auto-generated certificate (`disabled` on plain TCP, `preferred`, `required` and `verify-ca`
over TLS, `verify-identity` rejecting the certificate's host-name mismatch) with
`caching_sha2_password` authentication in every mode, `# HELP`/`# TYPE` for every family of the
tables above and the shared bucket boundaries, route templates and `UNKNOWN` in the `uri` label,
the `insert` error counter on a duplicate code, request-id echo and its correlation into the
access log, password masking in the startup line, readiness 503 while the main listener still
answers during the drain delay, a full-severity scan with zero findings for the 12 OS and 22
Python packages (syft lists all 34), and the idle footprint after the round trip (78 MB RSS).
Build-stage gates shown red on violating trees: the library closure on a dist without `libz`,
the C-API gate on an executable linked without `-Wl,--export-dynamic` (721 unresolved symbols).
Blind spots: the linux/arm64 image was not built or run on this host (no emulation in the build
VM; CI builds it natively) and is unverified at runtime; no load test was run, the
vertical-scaling note above is structural; `verify-identity` was not shown to accept a matching
host name (no CA-signed certificate with the right name was available); a DigitalOcean managed
MySQL itself was not reached from this host; the `/docs` page was fetched, not rendered (its
Swagger assets come from a CDN).

## Publishing
```
podman manifest push --all --compression-format zstd:chunked --compression-level 19 --format oci \
  localhost/module-service:0.0.2 docker://<registry>/module-service:0.0.2
```
