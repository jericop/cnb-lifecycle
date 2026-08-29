# Tasks: Lifecycle BuildKit-Native Export Emit-Mode

Lifecycle-side half of Option C. Additive, opt-in. The pack-side half is
`jericop/cnb-pack@buildkit-native-export`. Local/MVP validation; no
`PACK_TEST_*` env-var-gated tests.

Branch: `jericop/cnb-lifecycle@buildkit-native-export`.

SCOPE NOTE (as-built): this repo provides TWO things for Option C — (1) the
exporter EMIT-MODE (`-emit-export-plan`) that records the layer plan + image config
+ persisted layer tars (tasks 1–5, 9), and (2) the CNB BuildKit GATEWAY FRONTEND
(`buildkit/cnbfrontend`) that assembles the final image from that emit output
(task 8). Pack drives the frontend in-process and does the host-side metadata-SHA
rewrite. MVP is IMPLEMENTED + VALIDATED end-to-end (cold/warm/multi-arch/rebase).

- [x] 1. Map the exporter's layer-create vs image-mutate operations
  - Enumerate, in `phase/exporter.go` (+ `layers/`), every operation that mutates
    the image (`AddLayerWithDiffIDAndHistory`, `ReuseLayerWithHistory`,
    `SetEntrypoint`, `SetCmd`, `SetEnv`, `SetWorkingDir`, `SetLabel`) and the order
    they occur, plus the `files.LayersMetadata`/`BuildMetadata` they build. This is
    the exact set emit-mode must record. Confirm API-gated variations.
  - _Requirements: 1.3, 2.1, 3.1, 3.2_

  ### Task 1 conclusion: emit-mode is FEASIBLE (recording `imgutil.Image` seam)

  **Verdict: feasible with a small, non-breaking change — possibly ZERO change to
  `phase/exporter.go`.** Every image mutation in `Export()` flows through the
  `opts.WorkingImage` field, whose type `imgutil.Image` is a fully-specified public
  interface. The exporter never constructs the image; it only calls interface
  methods on it. That interface IS the seam.

  **Enumerated `Export()` operations, in order (all on `opts.WorkingImage`):**
  1. `TopLayer()` — READ; seeds `meta.RunImage.TopLayer` (the rebase boundary).
  2. `AddLayerWithDiffIDAndHistory(tarPath, diffID, history)` — new layers
     (extension → buildpack → SBOM(≥0.8) → app slices → launcher → config →
     process-types). Via helpers `addExtensionLayer` / `addOrReuseBuildpackLayer` /
     `addAppLayers`.
  3. `ReuseLayerWithHistory(diffID, history)` — reused buildpack/app/launcher/config
     layers when digest matches `OrigMetadata` (rebase reuse path).
  4. `SetLabel(k, v)` × N — `io.buildpacks.lifecycle.metadata`,
     `io.buildpacks.build.metadata`, `io.buildpacks.project.metadata`, each
     buildpack-provided label, and (Platform ≥0.15, if `ExecEnv != ""`) the
     exec-env label. `platform/labels.go` holds the constants.
  5. `SetEnv(k, v)` × N — `CNB_LAYERS_DIR`, `CNB_APP_DIR`, `CNB_PLATFORM_API`,
     `CNB_DEPRECATION_MODE`, and `PATH` (prepended). `Env("PATH")` is a READ used to
     compute the prepend.
  6. `SetEntrypoint(entrypoint)` — process path or launcher path.
  7. `SetCmd()` — intentionally empty.
  8. `SetWorkingDir(appDir)`.
  9. `saveImage(opts.WorkingImage, opts.AdditionalNames, ...)` — UNCONDITIONAL, at
     the very end.

  **Metadata built (what emit-mode must serialize as config-side data):**
  `files.LayersMetadata` (`platform/files/analyzed.go`) — `RunImage.{TopLayer,
  Reference,Image,Mirrors}` (Image/Mirrors gated Platform ≥0.12), `Stack.RunImage`,
  `Buildpacks[]`, `App[]`, `Launcher`, `Config`, `ProcessTypes`, `BOM`. Plus
  `files.BuildMetadata` (`platform/files/metadata.go`) for the build.metadata label.

  **API-gated variations:** SBOM launch layer gated ≥0.8; `RunImage.Image/Mirrors`
  ≥0.12; exec-env label ≥0.15; `makeBuildReport` only populates BOM below 0.9.

  **Two reads must be satisfied by a real run-image backing:** `TopLayer()` and
  `Env("PATH")`. This is why the recording image must wrap a run-image-backed image
  (forward reads, capture writes) rather than an empty one.

  **The construction seam lives in `cmd/lifecycle/exporter.go`, not `phase/`:** a
  `switch` on `e.UseLayout`/`e.UseDaemon` selects `initLayoutAppImage` /
  `initDaemonAppImage` / `initRemoteAppImage`, each returning an `imgutil.Image`
  built `FromBaseImage(runImage)` and passed as `WorkingImage`. Emit-mode adds a 4th
  case that builds a **recording `imgutil.Image` backed by the same run image**.
  `phase/exporter.go` mutation logic is untouched.

  **Reference impl already exists:** `imgutil/fakes/image.go` implements every
  needed method as recording no-ops that keep in-memory state (`SetLabel`,
  `SetEnv`, `SetWorkingDir`, `SetEntrypoint`, `AddLayerWithDiffIDAndHistory`,
  `ReuseLayerWithHistory`, `Save`) — a working template for the emit recorder.

  **The one real friction — the unconditional `saveImage`:** `Export()` always
  calls `saveImage` at the end. Two non-breaking resolutions, pick in Task 2:
  - **(a) Recording image, no exporter change:** the recorder's `Save()` captures
    the accumulated plan+config and writes `plan.json`/`config.json` instead of
    pushing. `phase/exporter.go` unchanged. Preferred for MVP.
  - **(b) Tiny opt-in flag:** add `ExportOptions.SkipSave bool` (or an `EmitSink`),
    default false → today's behavior byte-for-byte; when set, skip `saveImage`.
    One backward-compatible field; existing callers unaffected.

  Both preserve all existing contracts (default callers pass a real `imgutil.Image`
  and behave exactly as today). Recommendation: pursue (a) first; fall back to (b)
  only if capturing inside `Save()` proves awkward for multi-arch config emission.

- [x] 2. Introduce an export "sink" seam (or recording image)
  - Add an internal seam so the exporter's flow writes layer/config ops to a sink.
    Default sink = today's `imgutil.Image` mutation (unchanged behavior). Emit sink
    = records ops. (MVP alternative: run the existing flow against a recording
    `imgutil.Image` that captures the calls, reusing imgutil fakes.)
  - _Requirements: 1.1, 1.2, 1.3_
  - DONE: chose the recording-`imgutil.Image` variant (zero change to
    `phase/exporter.go`). New package `phase/emit` with `RecordingImage` that embeds
    a run-image-backed `imgutil.Image` (reads forward), overrides all mutators
    (Add/Reuse layer variants, SetEntrypoint/SetCmd/SetWorkingDir/SetLabel/
    RemoveLabel/SetEnv) to record, and overrides Save/SaveAs to emit the contract.
    `var _ imgutil.Image = (*RecordingImage)(nil)` compiles. File:
    `phase/emit/recording_image.go`.

- [x] 3. Implement emit-mode: produce plan.json + config.json (the contract)
  - Add the opt-in entry point (e.g. exporter `-emit-export-plan <dir>` or a
    library API). Serialize the ordered Layer Plan (reused vs new, diffID, tar path,
    history, runImage.reference/topLayer) and the Image Config (entrypoint/cmd/env/
    workingDir/labels) EXACTLY as the design's `schema: buildkit-native-export/v1`.
    Reference the ACTUAL built layer tars; do NOT re-tar; do NOT push; do NOT build
    a final image.
  - _Requirements: 1.1, 2.*, 3.*, 4.*, 5.*, 6.1_
  - DONE: added opt-in `-emit-export-plan <dir>` flag (`cli.FlagEmitExportPlan`) +
    `LifecycleInputs.EmitExportPlan`. In `cmd/lifecycle/exporter.go`, after the
    normal run-image-backed app image is built, when `EmitExportPlan != ""` the
    app image is WRAPPED in `emit.NewRecordingImage(appImage, runImageID, dir)`.
    `Export()` is unchanged; its unconditional `saveImage` -> `RecordingImage.SaveAs`
    -> writes `<dir>/buildkit/plan.json` + `config.json`. `RemoveLabel` also
    recorded. Lifecycle builds (`go build ./...` OK); flag is accepted at parse time
    (verified) while unknown flags are rejected.

- [x] 4. Rebase/parity guarantees
  - Ensure emitted new-layer diffIDs == the lifecycle's built tars' diffIDs, reused
    layers referenced by digest, and the emitted lifecycle-metadata label equals a
    normal export's for the same inputs.
  - _Requirements: 4.1, 4.2, 4.3_
  - DONE (code audit of `phase/exporter.go` vs `RecordingImage`): full enumeration of
    every `opts.WorkingImage` call confirms parity by construction:
    - **New-layer diffID parity:** all new layers go through
      `AddLayerWithDiffIDAndHistory(layer.TarPath, layer.Digest, layer.History)`
      (lines 475, 626, 630). RecordingImage records `TarPath`/`DiffID` verbatim — the
      lifecycle's own built tar + precomputed diffID, NO re-tar. Emitted diffIDs ==
      built tars' diffIDs.
    - **Reused-by-digest:** all reuse goes through `ReuseLayerWithHistory(sha, hist)`
      (lines 407, 472, 622); recorded as `Reused:true, DiffID:sha`, no tar. Consumer
      references the run image's original blob by digest.
    - **lifecycle-metadata label parity:** `meta` (`files.LayersMetadata`) is built by
      `Export()` independent of the image type, then `SetLabel(LifecycleMetadataLabel,
      json(meta))` (line 500) — RecordingImage records the exact string ⇒ byte-identical
      to a normal export. Same for build.metadata/project.metadata/exec-env labels.
    - **Read/write ordering safe:** the ONLY reads are `TopLayer()` (line 115, before any
      write) and `Env("PATH")` (line 564, before its own `SetEnv("PATH")` line 570). Both
      forward to the UNMUTATED run-image-backed embedded image (RecordingImage never calls
      the embedded mutators), so they return run-image values — exactly what a fresh
      export reads. Recorded final PATH == prepended value.
    - **TopLayer invariant:** `meta.RunImage.TopLayer` (seeded at Export start) and
      `plan.RunImage.TopLayer` (read again in Save) are the SAME value, because the embedded
      image is never mutated ⇒ consistent rebase boundary in both plan and label.

- [x] 5. Local validation (MVP — manual, no automated tests yet)
  - Run emit-mode against a real `/layers` from a build; eyeball the emitted plan +
    config (layer order, diffIDs, history, labels, env, entrypoint) against a normal
    export of the same inputs. This is a manual/CLI check for the MVP — NO new Go
    unit/integration tests at this stage.
  - _Requirements: 7.1_
  - DONE. Produced a real `/layers` by running the actual lifecycle phases
    (detector → analyzer → restorer → builder) inside the builder image
    (`jericop/ubuntu-noble-builder:buildkit-multi-arch-poc`, arm64) against
    `samples/go/no-imports`: 3 buildpacks participated (ca-certificates 3.12.2,
    go-dist 2.10.9, go-build 2.4.20 — all Buildpack API **0.7**, exercising the
    >=0.7 target), producing the compiled `workspace` binary. Then ran the
    emit-mode exporter (`export -emit-export-plan /emit-out`, Platform API 0.13)
    against that `/layers`.
  - RESULT: emit-mode wrote `/emit-out/buildkit/plan.json` + `config.json` and
    pushed NO image. Verified against a normal export's behavior:
    - **Layer order** matches the exporter's `Adding layer` sequence exactly:
      ca-certificates:helper → go-build:targets → launch.sbom → app slice →
      launcher → config → process-types (7 layers, all `reused:false` on a fresh
      build, each with real diffID + tar path + history — NO re-tar).
    - **Rebase-boundary invariant holds:** `plan.runImage.topLayer` ==
      the `runImage.topLayer` embedded in the `io.buildpacks.lifecycle.metadata`
      label (`sha256:8562...b51f`) — proves the embedded run-image is never mutated.
    - **lifecycle-metadata label** carries app/buildpacks/config/launcher/
      process-types/sbom SHAs that match the plan's layer diffIDs 1:1.
    - **config.json:** entrypoint `/cnb/process/workspace`; `cmd: []` (normalized
      from empty variadic); workingDir `/workspace`; all 5 CNB env vars incl
      `CNB_PLATFORM_API: 0.13`; **PATH = `/cnb/process:/cnb/lifecycle:` + run-image
      PATH** — confirms the final-value PATH decision (prepend applied, run-image
      base preserved). build.metadata + project.metadata labels present.
    - Files written under the `buildkit/` recorder subdir per the namespacing
      decision. Sample preserved at
      `/tmp/kiro-command-logs/emit-contract-sample-*/buildkit/`.
    - Fixed during validation: `SetCmd()` now normalizes nil→`[]string{}` so
      config.json emits `"cmd": []` not `null` (exact contract fidelity).

- [x] 6. Publish an emit-capable lifecycle image (for the pack-side e2e)
  - DONE. `jericop/lifecycle:buildkit-native-export` published (bundles emit-mode).
    For the local iterate loop, a multi-arch (amd64+arm64) updated variant is built
    and pushed to the local registry (`pack-local-registry:5000/lifecycle:native-updated`,
    via `/tmp/build-lc-multiarch.sh`) and referenced by pack's `--lifecycle-image`.
    MVP acceptance bar MET: pack drives emit-mode end-to-end (cold/warm/multi-arch/
    rebase all pass — see the pack spec's MVP OUTCOME).
  - _Requirements: 6.2_

- [~] 7. Add automated tests (MVP confirmed — now unblocked, still to be written)
  - The pack MVP is now confirmed working end-to-end, so this is UNBLOCKED. To add:
    unit tests for the recording `imgutil.Image` (captures ordered layer + config
    ops + tar persistence), unit tests for the plan/config serializer
    (`buildkit-native-export/v1` schema), tests for the frontend assembly
    (`buildkit/cnbfrontend`), and one export-parity check (emit vs normal export for
    the same inputs). Local registry like pack's testhelpers; no `PACK_TEST_*`
    env-var gating. NOT YET WRITTEN — deferred per the MVP-first decision (validate
    manually, add automated coverage once the design settled; it now has).
  - _Requirements: 7.2, 7.3, 7.4_

- [x] 8. Frontend package: the BuildKit assembly mechanism (as-built)
  - The pack-side spec originally assumed pack assembled the image via pure-LLB
    `client.Solve`. That CANNOT set the output image config/labels (needs the
    gateway result API). So the lifecycle repo now also hosts the CNB BuildKit
    GATEWAY FRONTEND that does the assembly; pack drives it in-process via
    `client.Client.Build`. Prior art: EricHripko/cnbp.
  - Files: `buildkit/cnbfrontend/build.go` (gateway `BuildFunc`, `Opt*` option
    consts, `ContextLocalName`), `buildkit/cnbfrontend/assemble.go` (`buildPlatform`,
    per-layer `tar -xf` extraction of each emitted layer with
    `llb.Network(NetMode_HOST)`, `assembleState`, run image resolved from
    `/layers/analyzed.toml`, `LayerOrderLabel = io.buildpacks.native.layer-order`
    recording ordered emitted diffIDs for the host-side rewrite),
    `cmd/cnb-frontend/main.go` (thin `grpcclient.RunFromEnvironment` wrapper for a
    standalone `#syntax=` image — nice-to-have; pack does not need it).
  - Assembly is PER-LAYER (one extracted layer per emitted CNB layer, plan order)
    onto `FROM run-image`, config/labels set via `AddMeta(ExporterImageConfigKey)`,
    per-platform refs returned for native multi-arch.
  - _Requirements: 1.1, 3.1, 3.3, 5.1_

- [x] 9. Emit-mode persists layer tars for the frontend (as-built refinement)
  - The exporter's own artifacts temp dir is `defer os.RemoveAll`'d, so the frontend
    could not read the emitted tars at their original path. `RecordingImage.Save()`
    now COPIES each new layer's tar into `<emit-dir>/buildkit/layers/NNN-<name>.tar`
    (`LayersSubdir = "layers"`, `copyFile`, `sanitizeLayerName`) and records the
    RELATIVE `TarPath` in `plan.json`, so the frontend reads persisted tars after the
    export RUN. File: `phase/emit/recording_image.go`.
  - _Requirements: 2.*, 3.*_
  - NOTE on the metadata-SHA rewrite: it is NOT done by the lifecycle/frontend —
    the gateway `Reference` API does not expose the produced diffIDs (BuildKit
    computes them at export, after the frontend returns). The frontend only records
    the ordered emitted diffIDs in the `io.buildpacks.native.layer-order` temp
    label; PACK performs the host-side rewrite post-push (see the pack spec).

## Decisions

- **RecordingImage location:** lives in `cnb-lifecycle` (the producer owns emit-mode
  end-to-end; pack consumes only the emitted `plan.json`/`config.json`/tars and does
  not link lifecycle internals).
- **Parity-test `/layers` fixture:** ULTIMATELY generated using whatever mechanism
  the lifecycle uses today to produce a `/layers` dir for its own export tests (so
  the fixture stays faithful to real lifecycle output). For the MVP's simple,
  deterministic check, use the `samples/go/no-imports` app as the initial input;
  REVISIT and switch to the lifecycle's native generation after the MVP is fully
  vetted. (Applies to the DEFERRED test task 7, not the MVP manual check.)

## Task Dependency Graph

```
1 [DONE] ─> 2 (sink seam) ─> 3 (emit plan.json+config.json) ─> 4 (rebase/parity) ─> 5 (MVP manual validation) ─> 6 (publish lifecycle image) ─> 7 (automated tests: unblocked, not yet written)
                                              │
                                              ├─> 8 (frontend package buildkit/cnbfrontend — assembly mechanism)
                                              └─> 9 (emit persists layer tars for the frontend)
                          (8 + 9 are the as-built assembly path; the host-side metadata-SHA rewrite lives in PACK, not here)
```

## Notes

- The EMIT CONTRACT (design.md `schema: buildkit-native-export/v1`) is the interface
  the pack-side spec consumes — keep both specs in sync when it changes.
- This is the linchpin that determines Option C's viability. If a clean emit-mode
  is infeasible, the fallback is Option B (lifecycle-as-library-hybrid), which needs
  no lifecycle change.
- Detector/builder unchanged; only export gains emit-mode.
