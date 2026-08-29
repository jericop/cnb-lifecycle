# Tasks: CNB BuildKit Gateway Frontend (`cnbfrontend`)

AS-BUILT spec. The frontend is IMPLEMENTED and VALIDATED as part of the
`buildkit-native-export` MVP. Tasks 1–7 reflect the completed implementation; the
remaining tasks are follow-ups (not for the MVP).

Repo/branch: `jericop/cnb-lifecycle@buildkit-native-export`.
Package: `buildkit/cnbfrontend`; standalone entrypoint `cmd/cnb-frontend`.

## Overview

The frontend runs the CNB lifecycle phases as LLB, runs the exporter in emit-mode,
assembles the app image `FROM run-image` (one layer per emitted CNB layer), and
returns per-platform refs + configs so BuildKit exports one native multi-arch image.
Pack drives it in-process and does the host-side metadata-SHA rewrite after push.

## Tasks

- [x] 1. Importable frontend package + standalone entrypoint
  - `buildkit/cnbfrontend/build.go` exposes `Build(ctx, client.Client)`; consumed
    in-process by pack (no frontend image required) AND wrapped by
    `cmd/cnb-frontend/main.go` (`grpcclient.RunFromEnvironment`) for standalone use.
    Exported option keys + `ContextLocalName` + `LayerOrderLabel` so the pack
    consumer matches them exactly.
  - _Requirements: 1.1, 1.2, 1.3, 1.4_

- [x] 2. Parse frontend options
  - `parseBuildConfig` reads all `Opt*` keys from `BuildOpts().Opts`; validates
    required builder/run image; defaults platforms (host), platform API, uid/gid.
  - _Requirements: 2.1, 2.2, 2.3, 2.4_

- [x] 3. Emit-mode LLB graph (`buildEmitState`)
  - builder base [+ optional lifecycle overlay] → setup dirs → [order.toml] → copy
    app → analyzer → detector → restorer → builder → exporter `-emit-export-plan`.
    Per-arch persistent cache mount; CNB user + platform-API env; optional
    `CNB_REGISTRY_AUTH`; `llb.Network(HOST)` on phase RUNs.
  - _Requirements: 3.1, 3.2, 3.3, 3.4_

- [x] 4. Read + validate the emit contract
  - `readEmitContract` / `readEmitConfig` read `/emit/buildkit/{plan.json,config.json}`
    via `Reference.ReadFile`, unmarshal into imported `emit` types, validate schema
    + non-empty layers.
  - _Requirements: 3.1, 3.2_

- [x] 5. Per-layer assembly FROM the run image (`assembleState`)
  - Resolve run image from `/layers/analyzed.toml` (fallback: normalize raw option);
    `FROM run-image`; mount built state read-only at `/emit-tars`; for each
    non-reused plan layer in order, `RUN tar -xf <persisted tar> -C /` (one layer
    each). Run image never modified; reused layers skipped.
  - _Requirements: 4.1, 4.2, 4.3, 4.4, 4.5_

- [x] 6. Apply image config/labels + record layer-order label
  - `applyCNBConfig` overlays emitted entrypoint/cmd/workingDir/labels + `mergeEnv`
    onto the run-image config; record `io.buildpacks.native.layer-order` (ordered
    emitted diffIDs) for pack's host-side rewrite; return marshaled config.
  - NOTE: the `io.buildpacks.native.layer-order` label is a REQUIRED build output and
    must remain DURABLE on the final pushed image (pack's rewrite must NOT remove it)
    so buildkit-native images are self-heal-capable (see pack tasks 9 + 11). The
    label is namespaced `io.buildpacks.native.*` (frontend/pack-internal), so it is
    not part of the CNB lifecycle-metadata contract and should not need a spec change.
  - _Requirements: 5.1, 5.2, 5.3_

- [x] 7. Native multi-platform result
  - `Build` builds each platform in parallel (errgroup); single → `SetRef` +
    `AddMeta(ExporterImageConfigKey)`; multi → per-platform `AddRef` +
    `AddMeta(ExporterImageConfigKey/<key>)` + `AddMeta(ExporterPlatformsKey)`. No
    intermediate tags. Validated amd64+arm64.
  - _Requirements: 6.1, 6.2, 6.3_

- [ ] 8. (FOLLOW-UP) Automated tests for the frontend
  - Assembly graph shape (per-layer boundaries in plan order), contract
    parse/validation, config overlay + layer-order label, multi-platform result
    shape. Local/MVP style; no `PACK_TEST_*` gating. Deferred with the broader
    buildkit-native-export test task.
  - LABEL RE-RECORD ACROSS REBUILDS: assert the frontend RE-RECORDS
    `io.buildpacks.native.layer-order` from the CURRENT build's emit plan on each
    build (correct contents when reused-vs-new layers change), not a stale value.
    End-to-end repeated-cycle coverage (≥2 rebuilds, ≥2 rebases, rebuild-after-rebase,
    self-heal-then-repeat) lives in the pack spec test task (pack task 8 / Req 7b).
  - _Requirements: 3.*, 4.*, 5.*, 6.*_

- [ ] 9. (FOLLOW-UP) Standalone rebuild/rebase story
  - Standalone frontend use (without pack) is first-build-only today: the host-side
    metadata-SHA rewrite lives in pack, so a rebuild fails at the analyzer
    previous-image restore. Make standalone use support the full rebuild/rebase
    lifecycle (self-apply the rewrite via a post-export hook/wrapper) OR clearly
    document + fail fast on a detected rebuild. Because the frontend records the
    DURABLE `io.buildpacks.native.layer-order` label (task 6), an external tool (or
    pack's self-healing build-time check + fix flag — pack tasks 9/11) can also heal
    a standalone-built image. Mirrors pack task 10.
  - _Requirements: 7.2_

- [ ] 10. (REQUIRED — correctness) Extract with `tar` from the BUILD image, not the run image
  - The current per-layer assembly runs `tar` via the run image's own shell
    (`RUN /bin/sh -c "tar -xf ... -C /"`), which assumes the run image has `/bin/sh`
    + `tar`. That assumption is INVALID in general — distroless/static run images
    have neither. FIX: extract using `tar` MOUNTED IN from the BUILD image (standard
    tooling) against the run-image rootfs. This also avoids the shared-library
    (glibc/libstdc++) loading problems of executing a run-image binary. The run image
    is still never modified — only the extraction TOOLING source changes.
  - Not a "future hardening" — required for correctness on shell-less run images.
    Update `assembleState` accordingly.
  - _Requirements: 4.3, 7.3_

## Task Dependency Graph

```
1 (package + standalone) ─> 2 (parse opts) ─> 3 (emit LLB graph) ─> 4 (read contract) ─> 5 (per-layer assembly) ─> 6 (config + layer-order label) ─> 7 (multi-platform result)  [ALL DONE]
                                                                                                                          │
                                                        10 (REQUIRED: extract with build-image tar; run images may have no shell/tar)
                                                                              (follow-ups) 8 (tests), 9 (standalone rebuild)
```

## Notes

- The frontend depends on the exporter EMIT-MODE
  (`cnb-lifecycle/.kiro/specs/buildkit-native-export`) and is driven by the pack
  `buildkit-native` backend (`cnb-pack/.kiro/specs/buildkit-native-export`), which
  performs the host-side metadata-SHA rewrite. Keep those specs in sync when the
  emit contract or option keys change.
- Prior art: EricHripko/cnbp (assembly pattern). We keep the real lifecycle exporter
  (emit-mode) for full CNB fidelity — see the `cnbp-buildkit-frontend` steering note.
- The frontend's final home (dedicated repo vs staying in cnb-lifecycle) is
  deferred.
