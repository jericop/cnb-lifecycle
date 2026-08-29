# Design: CNB BuildKit Gateway Frontend (`cnbfrontend`)

> **SUPERSEDED / RETIRED.** Retired in favor of Option A (build-then-finalize) — see
> `buildkit-native-export` (both repos) and `spike-eliminate-metadata-rewrite.md`.
> Kept for history.

## Overview

`cnbfrontend` is a BuildKit gateway frontend that assembles a CNB app image
natively inside BuildKit. It runs the real CNB lifecycle phases (analyzer →
detector → restorer → builder → exporter-in-emit-mode) as LLB `RUN`s, then
assembles the final image `FROM run-image` by extracting each emitted CNB layer as
its own layer, and returns per-platform references + image configs so BuildKit
exports one native (multi-arch) image.

It is implemented as an importable library so it can be used two ways:

- **In-process by pack** via `client.Client.Build(ctx, opt, product, Build, ch)` —
  no frontend image required. This is the primary path.
- **Standalone** as a `#syntax=` frontend image via `cmd/cnb-frontend` (a thin
  `grpcclient.RunFromEnvironment` wrapper).

### Prior art and how we differ

The assembly PATTERN (frontend runs lifecycle phases as LLB, then builds the final
image `FROM run-image` + copies layers, returns per-platform refs so BuildKit owns
the output and produces manifest lists with no intermediate tags) is from
**EricHripko/cnbp**. The critical difference: cnbp REPLACED the lifecycle exporter
with a hand-written export, losing CNB fidelity (no proper metadata labels, no SBOM,
no layer reuse, Platform API 0.5). We instead KEEP the real exporter via emit-mode,
so all CNB semantics are computed by the lifecycle; the frontend only assembles the
image from the exporter's emitted plan/config/tars. See the `cnbp-buildkit-frontend`
steering note.

## Architecture

```
pack (host)  ──client.Client.Build(Build)──►  BuildKit gateway
                                                     │
                                          cnbfrontend.Build(ctx, c)
                                                     │
                         ┌───────────────────────────┴───────────────────────────┐
                         │  for each platform (parallel, errgroup):                │
                         │                                                        │
                         │  buildPlatform(ctx, c, cfg, p):                        │
                         │   1. buildEmitState  → LLB graph:                      │
                         │        FROM builder [+ overlay lifecycle]              │
                         │        setup dirs; [order.toml]; COPY app              │
                         │        analyzer → detector → restorer → builder        │
                         │        exporter -emit-export-plan /emit                │
                         │      solveState → builtRef                             │
                         │   2. readEmitContract(builtRef)  (/emit/buildkit/plan.json) │
                         │   3. assembleState:                                    │
                         │        run image from /layers/analyzed.toml            │
                         │        FROM run-image; for each NON-reused plan layer: │
                         │          RUN tar -xf <persisted tar> -C /   (1 layer)  │
                         │      solveState → assembledRef                         │
                         │   4. readEmitConfig; overlay onto run-image config;    │
                         │      record io.buildpacks.native.layer-order label     │
                         │                                                        │
                         │  res.AddRef(platformKey, assembledRef)                 │
                         │  res.AddMeta(ExporterImageConfigKey[/platformKey], cfg)│
                         └───────────────────────────┬───────────────────────────┘
                                                     │  (+ ExporterPlatformsKey when multi)
                                                     ▼
                                        BuildKit ExporterImage (push=true)
                                                     │
                                                     ▼
                              pack host-side metadata-SHA rewrite (post-push)
                              reads io.buildpacks.native.layer-order, remaps SHAs
```

## Components and Interfaces

### `Build(ctx, client.Client) (*client.Result, error)` — `build.go`

The gateway `BuildFunc` entrypoint. Parses options, builds each requested platform
in parallel (`errgroup`), and assembles the `client.Result`:

- Single platform: `res.SetRef(ref)` + `res.AddMeta(exptypes.ExporterImageConfigKey, cfg)`.
- Multi platform: for each platform `res.AddRef(key, ref)` +
  `res.AddMeta(ExporterImageConfigKey + "/" + key, cfg)`, plus
  `res.AddMeta(exptypes.ExporterPlatformsKey, platforms)` so BuildKit emits one OCI
  index.

### `buildConfig` + `parseBuildConfig(c)` — `build.go`

Parsed frontend options. Exported option keys (pack imports these):

| Const | Key | Meaning |
|---|---|---|
| `OptBuilderImage` | `cnb-builder-image` | builder base (required) |
| `OptRunImage` | `cnb-run-image` | run image / assembly base (required) |
| `OptLifecycleImage` | `cnb-lifecycle-image` | optional: overlay `/cnb/lifecycle` |
| `OptPlatforms` | `cnb-platforms` | comma-separated os/arch |
| `OptPlatformAPI` | `cnb-platform-api` | CNB platform API (default `0.13`) |
| `OptUID` / `OptGID` | `cnb-uid` / `cnb-gid` | CNB user/group (default 1001) |
| `OptOrderTOML` | `cnb-order-toml` | optional custom order.toml |
| `OptRegistryAuth` | `cnb-registry-auth` | optional `CNB_REGISTRY_AUTH` json |
| `OptImageName` | `cnb-image-name` | target name (used by in-build phases) |
| `OptInsecureReg` | `cnb-insecure-registries` | comma-separated insecure registries |

`ContextLocalName = "context"` is the `llb.Local` name for the app build context
(pack wires `SolveOpt.LocalMounts` under the same key).

### `buildEmitState(cfg, p) llb.State` — `assemble.go`

Builds the emit-mode LLB graph:

1. `FROM builder` (per platform). Optionally overlay an emit-capable lifecycle
   (`rm -rf /cnb/lifecycle` then copy from the lifecycle image).
2. Setup dirs (`/cache`, `/layers`, `/platform`, `/emit`), optional `order.toml`,
   copy app to `/workspace`, fix perms.
3. Per-arch persistent cache mount (`llb.AsPersistentCacheDir("cnb-buildpacks-cache-"+arch, ...)`).
4. Run analyzer (with `-run-image` + `-skip-chown` + insecure-registry args) →
   detector → restorer → builder → exporter with `-emit-export-plan /emit`.
5. All phase RUNs use the CNB user, the platform-API env, optional
   `CNB_REGISTRY_AUTH`, and `llb.Network(pb.NetMode_HOST)` (see Design Decision 3).

### `readEmitContract` / `readEmitConfig` — `assemble.go`

Read `/emit/buildkit/plan.json` + `config.json` from the solved emit state via the
gateway `Reference.ReadFile`, unmarshal into the imported `emit.Plan` /
`emit.ImageConfig` types, and validate the schema (`buildkit-native-export/v1`) and
non-empty layers.

### `assembleState(...)` — `assemble.go`

1. Resolve the run image ref via `resolvedRunImageRef` (prefers
   `/layers/analyzed.toml`'s digest-pinned ref; falls back to normalizing the raw
   option — see Design Decision 2).
2. `resolveImageConfig` for the run image (the base config to overlay onto).
3. `state := llb.Image(runRef)`, then mount the built state read-only at
   `/emit-tars` and, for each NON-reused plan layer in order,
   `RUN tar -xf <persisted tar> -C /` — one RUN = one layer (Design Decision 1).

### `applyCNBConfig` / `mergeEnv` — `assemble.go`

Overlay the emitted entrypoint/cmd/workingDir/labels onto the run-image config and
merge env (base env preserved; CNB keys override).

### `emittedLayerOrderJSON` + `LayerOrderLabel` — `assemble.go`

Compute the ordered emitted diffIDs of the non-reused layers and store them in the
temporary `io.buildpacks.native.layer-order` label for pack's host-side rewrite.

### `cmd/cnb-frontend/main.go`

`grpcclient.RunFromEnvironment(appcontext.Context(), cnbfrontend.Build)` — the
standalone frontend image entrypoint.

## Data Models

Consumed from the imported `phase/emit` package (single source of truth):

- `emit.Plan { Schema, RunImage{Reference, TopLayer}, Layers []LayerOp }`
- `emit.LayerOp { ID, Reused, DiffID, TarPath, History }`
- `emit.ImageConfig { Schema, Entrypoint, Cmd, WorkingDir, Env, Labels }`
- `emit.Schema`, `emit.RecorderDir` (`buildkit`), `emit.PlanFileName`,
  `emit.ConfigFileName`, `emit.LayersSubdir` (`layers`).

Produced: `exptypes.ExporterImageConfigKey` metadata (a marshaled
`dockerspec.DockerOCIImage`) per platform + `exptypes.ExporterPlatformsKey` when
multi-platform.

## Recorded design decisions

### Decision 1: Per-layer assembly (one RUN per emitted CNB layer)

Extract each non-reused emitted tar as its own layer, in plan order, rather than a
wholesale `/layers` copy. This gives the assembled image the SAME layer boundaries
+ count as the emit plan, so each buildpack layer is individually addressable —
required for the analyzer's previous-image restore on rebuilds AND for
buildpack-layer patching. BuildKit recomputes each layer's diffID from the extracted
filesystem; pack's host-side rewrite reconciles the metadata SHAs.

### Decision 2: Run image from `analyzed.toml`, not the raw option

Use the fully-resolved, digest-pinned run-image reference the analyzer wrote to
`/layers/analyzed.toml` as the assembly base, not the raw `cnb-run-image` option.
Reasons: (a) the raw option may be an un-normalized short ref (e.g.
`paketobuildpacks/ubuntu-noble-run:latest`) that BuildKit would misparse (treating
the first path element as a registry host) — `normalizeImageRef` guards the fallback;
(b) pinning the exact base the exporter recorded keeps the rebase boundary
consistent between the plan and the image. The run image is used only as a READ-ONLY
base; it is never modified.

### Decision 3: `network.host` for the phase RUNs (MVP)

The analyzer/exporter must reach registries the builder is attached to (e.g. a local
dev registry reachable by container name). BuildKit's default RUN sandbox network
uses the daemon's upstream DNS and cannot resolve the builder's docker-network
peers, so the phase RUNs use `llb.Network(pb.NetMode_HOST)` and pack requests the
`network.host` entitlement. MVP choice; production network isolation is a follow-up.

### Decision 4: Frontend cannot rewrite metadata SHAs — pack does it host-side

The gateway `Reference` API (`ToState`/`Evaluate`/`ReadFile`/`StatFile`/`ReadDir`)
does not expose the produced layer diffIDs, and BuildKit computes them at export
time, AFTER `Build` returns. So the frontend cannot rewrite
`io.buildpacks.lifecycle.metadata` to match the produced layers. It instead records
the ordered emitted diffIDs in the temporary `io.buildpacks.native.layer-order`
label; pack performs the positional remap + rewrite host-side after push (see the
pack `buildkit-native-export` spec). This is the single most important architectural
constraint of the frontend.

## Error Handling

- Missing required options (builder/run image) → descriptive error from
  `parseBuildConfig`.
- Emit contract missing/unparyable/wrong schema/empty layers → wrapped error from
  `readEmitContract`.
- Per-platform build errors are wrapped with the platform string via the errgroup.
- Run-image resolution failure (no `analyzed.toml` AND no run-image option) → error.

## Testing Strategy

Validated end-to-end via the MVP local strategy (drive pack against
`samples/go/no-imports`, local registry): cold/warm/multi-arch/rebase all pass; all
per-layer metadata SHAs match actual diffIDs after pack's rewrite. Automated tests
for the frontend (assembly graph shape, contract parsing, per-layer boundaries,
multi-platform result shape) are a follow-up, tracked with the broader
`buildkit-native-export` test task (deferred until the design settled — it now has).
