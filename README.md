# VSC-kubernetes-containers

Container build definitions, a generic Helm chart, and a CI pipeline for a small
authentication stack:
* a Spring Boot user-management API (GraalVM native)
* a Next.js auth-portal
* PostgreSQL
* Traefik

Every image is built to an engine-independent container build standard:
* the images ship mechanisms (probe endpoints, env-driven configuration, signal-driven graceful shutdown)
* all deployment policy lives in the chart

## Layout

```
src/<Component>/              one buildable image per folder
  Containerfile               multi-stage build, digest-pinned bases
  buildCommand                sh+PowerShell polyglot; THE canonical build invocation
  CONTRACT.md                 the image's complete deployment interface
  patches/ supervisor/ ...    component-specific sources
src/charts/generic-stack/     the Helm chart (same artifact conventions: buildCommand, CONTRACT.md)
.github/workflows/build.yml   CI: build, scan, publish images + chart to GHCR
renovate.json                 automated bumps for base-image digests and CI tool pins
```

Operators and CD repositories need only the `CONTRACT.md` files:
* ports
* env knobs
* probe endpoints
* writable paths
* stop grace periods
* exit-code semantics

is all specified there

## Artifacts and versioning

| Folder | Image | Artifact tag | Packaged software (OCI version label) | UID |
|---|---|---|---|---|
| src/Backend | user-mgmt-service | 0.0.1 | user_mgmt_service 0.0.1-SNAPSHOT | 10021 |
| src/DB | postgresql | 0.0.1 | PostgreSQL 16.15 | 10020 |
| src/Frontend | auth-portal | 0.0.1 | auth_portal 0.1.0 (static export + Go server) | 10022 |
| src/Frontend-node | auth-portal | 0.0.1-node | auth_portal 0.1.0 (Node.js runtime variant) | 10022 |
| src/Proxy | traefik | 0.0.1 | Traefik v3.7.10 | 10023 |
| src/charts/generic-stack | charts/generic-stack | 0.0.1 | — | — |

* The **tag is the artifact version** (this repo's build)
* the **OCI `image.version` label is the packaged software's version**
* Upstream application sources are cloned at build time
  * from commits pinned in the Containerfiles
  * patched from `patches/`
* All images run non-root with fixed UIDs, read-only rootfs, no capabilities,
and are published as OCI with zstd:chunked layers.

## Building locally

Inside a component folder:

```
sh buildCommand                          # POSIX shells
iex (Get-Content -Raw buildCommand)      # PowerShell
```

Both produce the identical multi-arch manifest `localhost/<image>:<tag>`.
The chart folder's `buildCommand` runs `helm lint` + `helm package`.

## The chart

`generic-stack` is a single generic chart: templates range over a `components:` map that is
deep-merged over `componentDefaults`, so CD repositories control everything through values —
override any field, disable a component, or add new components without touching a template.
Values are validated by `values.schema.json` . The default components wire
the full stack (Traefik file-provider routes → frontend → backend → db) and encode every
image contract's probe endpoints, grace periods, and security posture. What each deployment
must supply (secrets, registry credentials, storage, exposure) is listed in the chart's
`CONTRACT.md`.

## CI

`.github/workflows/build.yml` converges the registry to the repo state:

- **discover** hashes each build folder (`git rev-parse HEAD:src/<dir>`) and compares
  against `tree-<hash>` marker tags on GHCR — only out-of-date artifacts are rebuilt, and a
  failed run self-heals on the next push. A **consistency gate** fails the run if a
  `buildCommand` tag and the chart's pinned image tag ever disagree.
- **build** is a matrix of out-of-date images × [amd64, arm64] on native runners,
  with registry layer caching per image and architecture. Per-arch build args
  are parsed out of the folder's `buildCommand` — the workflow holds no
  build knowledge of its own. Each image passes a Trivy CRITICAL gate and emits an SPDX SBOM
  artifact before its per-arch push.
- **manifest** assembles the multi-arch lists and moves the version + tree tags.
- **chart** verifies (lint, template, kubeconform), stamps the registry from the repo name,
  and publishes to `oci://ghcr.io/<repo>/charts`.

Pull requests run everything except pushes. `workflow_dispatch` with `force` rebuilds all.

**Releasing a change:** bump the tag in the component's `buildCommand` and the matching
`components.<name>.image.tag` in the chart values (plus the chart's own `version`), push —
CI rebuilds exactly that. Forgetting one half trips the consistency gate. Renovate proposes
base-image digest bumps and CI tool updates; upstream source bumps are manual (change the
pinned commit and upstream version ARG in the Containerfile).
