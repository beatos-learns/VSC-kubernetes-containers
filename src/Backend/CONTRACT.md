# user-mgmt-service — image contract

Image: localhost/user-mgmt-service:0.0.1 (user_mgmt_service 0.0.1-SNAPSHOT; retag for your
registry — the OCI version label keeps the packaged-software version, the tag is the artifact version)
UID:GID baked: 10021:10021 (ad-hoc assignment; override with `--build-arg APP_UID/APP_GID`)
Checker topology: in-process (the native binary serves endpoints, checker loop, signals, probe subcommand)
Checker runtime: none — in-process
Layer format: OCI, zstd:chunked (applied at push, see Publishing)

Source: `github.com/yagan93/user_mgmt_service` @ `747b7f4e3d1f09cc4434a75d9c991ea091fda548`, cloned during the build and
patched with `patches/0001-container-build-standard.patch` (health/ops machinery, graceful shutdown,
GraalVM native build config). Spring Boot 4.1.0-M3 (pre-release milestone), Java 25, compiled with
GraalVM native-image (no JVM in the image).

Final base (per architecture, deliberate split):
- linux/amd64: `scratch` — fully static native binary (`--static --libc=musl`)
- linux/arm64: `quay.io/almalinuxorg/10-micro` (digest-pinned) — mostly static binary (`--static-nolibc`),
  because GraalVM's musl toolchain exists for x64 only

## Ports
| Env        | Default | Purpose                                  |
|------------|---------|------------------------------------------|
| PORT       | 8080    | HTTP REST API (`/users/...`)             |
| ADMIN_PORT | 9090    | /startupz /livez /readyz /metrics        |

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

## Deployment requirements
- Stop grace period: grant at least **15 s** before SIGKILL (3 s drain delay + 10 s drain budget + margin)
- Writable paths: `/tmp` only (image runs read-only; mount tmpfs as the deployer sees fit)
- Init required: no
- Capabilities required: none; no privilege escalation
- PID 1 exception: none — the native binary is PID 1 via exec-form ENTRYPOINT
- Database schema: no migrations ship with the app; schema handling is entirely
  `SPRING_JPA_HIBERNATE_DDL_AUTO`. The authorities `USER_MODIFY` / `USER_DELETE` are not seeded —
  seed them at deployment (see the db image's init-script mechanism) or the protected
  PUT/DELETE endpoints stay unusable.

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
- first native build should be smoke-tested (register/login round-trip) before rollout: jjwt's
  ServiceLoader wiring and BouncyCastle rely on shipped/repository reachability metadata

## Publishing
```
podman manifest push --all --compression-format zstd:chunked --compression-level 19 --format oci \
  localhost/user-mgmt-service:0.0.1 docker://<registry>/user-mgmt-service:0.0.1
```
