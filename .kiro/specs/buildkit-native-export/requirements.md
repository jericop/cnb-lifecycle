# Requirements: Lifecycle BuildKit-Native Export Emit-Mode

## Introduction

This is the LIFECYCLE-SIDE half of the "BuildKit-Native Export" (Option C) effort.
The pack-side half lives in `jericop/cnb-pack@buildkit-native-export`
(`.kiro/specs/buildkit-native-export/`). This spec owns the lifecycle changes; the
pack spec owns consuming them.

Goal: add an EXPERIMENTAL, additive, OPT-IN lifecycle mode ("emit-mode") that lets
BuildKit assemble the final CNB app image natively (`FROM <run-image>` + add
builder-phase layers + apply CNB config), while the lifecycle remains the source of
buildpacks truth. Instead of the exporter mutating and pushing an `imgutil.Image`,
emit-mode EMITS the buildpacks-specific outputs that a caller (pack) turns into a
BuildKit assembly:

- an ordered **Layer Plan** (which layers, order, diffIDs, history, launch/cache
  flags, and which are reused run-image layers), and
- the final **Image Config** (entrypoint, cmd, env, workingdir, and the
  `io.buildpacks.*` labels, incl the serialized lifecycle-metadata + build-metadata
  labels).

This keeps 100% of CNB semantics in the lifecycle (no reimplementation in pack or
BuildKit), avoids egressing large layer data to the host, and avoids
intermediate-tag pushes.

This spec defines the EMIT CONTRACT (the exact shape of what emit-mode produces).
That contract is the interface the pack-side spec consumes; changes to it must be
reflected in both specs.

Detector and builder are unchanged (they still run as the normal lifecycle phases
/ binary). Only the EXPORT step gains an emit-mode alternative.

## Glossary

- **emit-mode**: an opt-in export path that emits a plan + config (+ layer tars)
  instead of assembling/pushing an image.
- **Layer Plan**: the ordered set of layer descriptors emit-mode produces.
- **Image Config**: the container image config metadata emit-mode produces
  (entrypoint/env/workingdir/labels/history).
- **Reused layer**: a run-image (base) layer referenced by digest that the caller
  must reference from the run image rather than re-add.
- **Emitted layer tar**: for non-reused layers, the actual layer tar the lifecycle
  built (already under `/layers`), plus its precomputed diffID, so the caller adds
  it by digest (guaranteeing rebase/diffID parity).

## Requirements

### Requirement 1: Additive, opt-in emit-mode

**User Story:** As a lifecycle maintainer, I want emit-mode to be additive and
opt-in, so that default `export` behavior (assemble + push an image) is unchanged.

#### Acceptance Criteria

1. THE lifecycle SHALL provide an OPT-IN way to run export in emit-mode (e.g. a new
   flag such as `-emit-export-plan <dir>` on the exporter, or an equivalent
   library entry point) that does NOT mutate or push an image.
2. WHEN emit-mode is NOT selected THE existing export behavior SHALL be unchanged.
3. THE emit-mode SHALL REUSE the existing layer-building and metadata logic
   (`layers.Factory`, `files.LayersMetadata`/`BuildMetadata`, `platform` labels),
   not a parallel reimplementation.

### Requirement 2: Emit the ordered Layer Plan

**User Story:** As a caller (pack), I want an ordered, complete description of the
layers to assemble, so that BuildKit can add them onto the run-image base in the
correct order.

#### Acceptance Criteria

1. THE emitted Layer Plan SHALL list, IN ORDER, every layer the exporter would add
   (buildpack launch layers, launcher, launcher-process-types, app slices, SBOM
   launch layer), matching the exporter's current ordering.
2. EACH plan entry SHALL indicate whether it is a REUSED run-image/base layer
   (referenced by digest) or a NEW layer produced by this build.
3. EACH NEW layer entry SHALL include its diffID and the tar location (a path to
   the actual layer tar the lifecycle built) so the caller can add it by digest.
4. EACH entry SHALL include the `v1.History` the exporter would record for that
   layer.
5. THE plan SHALL include the identifiers needed to record the run-image rebase
   boundary (runImage.reference, topLayer) consistent with the lifecycle-metadata
   label.

### Requirement 3: Emit the Image Config

**User Story:** As a caller (pack), I want the final image config the exporter
would set, so BuildKit can apply it to the assembled image.

#### Acceptance Criteria

1. THE emitted Image Config SHALL include: Entrypoint (the launcher /
   `/cnb/process/<default>`), Cmd (empty as today), Env (CNB_LAYERS_DIR,
   CNB_APP_DIR, CNB_PLATFORM_API, PATH, deprecation mode, etc. as the exporter
   sets), and WorkingDir (app dir).
2. THE emitted Image Config SHALL include ALL labels the exporter sets: the
   serialized `io.buildpacks.lifecycle.metadata`, `io.buildpacks.build.metadata`,
   `io.buildpacks.project.metadata`, `io.buildpacks.exec-env` (when applicable),
   and any buildpack-provided labels.
3. THE emitted config values SHALL be IDENTICAL to what the current exporter would
   set for the same inputs (so the assembled image is equivalent).

### Requirement 4: Rebase-safe, diffID-preserving emission

**User Story:** As a buildpacks user, I want rebase to keep working, so swapping
the run image later does not require a rebuild.

#### Acceptance Criteria

1. THE emitted NEW layer tars SHALL be the ACTUAL tars the lifecycle built (not a
   re-tar by the caller), and their diffIDs SHALL be emitted, so the assembled
   image's layer digests match a normal export.
2. THE emitted plan SHALL identify reused run-image layers by digest so the caller
   references the run image's ORIGINAL blobs (no re-tar).
3. THE emitted lifecycle-metadata label (runImage.reference/topLayer, layer SHAs)
   SHALL match a normal export for the same inputs, so rebase parity holds.

### Requirement 5: Do not require in-lifecycle image assembly or push

**User Story:** As the caller, I want emit-mode to avoid touching a registry or
building the final image, so BuildKit owns assembly and there are no
intermediate-tag pushes.

#### Acceptance Criteria

1. THE emit-mode SHALL NOT push to any registry.
2. THE emit-mode SHALL NOT require constructing a final `imgutil.Image`.
3. THE emit-mode SHALL run given the same inputs the exporter uses today (the
   `/layers` dir, app dir, analyzed metadata, run-image metadata, launcher config,
   platform API), producing only the plan + config (+ referenced tars).

### Requirement 6a: Platform API compatibility (>= 0.7)

**User Story:** As a buildpacks user on a current production buildpack (Buildpack
API 0.7+), I want emit-mode to work across the Platform API versions this lifecycle
supports, so I am not forced onto a newer Platform API.

#### Acceptance Criteria

1. THE emit-mode SHALL produce a correct plan + config for every Platform API this
   lifecycle supports (0.7 through 0.15), by recording exactly the operations the
   exporter performs for the negotiated Platform API (no per-version logic in the
   recorder).
2. THE emitted `config.json` SHALL include `CNB_PLATFORM_API` set to the negotiated
   Platform API (as the exporter sets today), so consumers know which API produced
   the image.
3. Platform API is negotiated between the platform and the lifecycle; it is NOT the
   buildpack-level `api` (Buildpack API). Emit-mode SHALL NOT require any change to
   how the Platform API is negotiated.

### Requirement 6b: Recorder-namespaced, extensible emit format

**User Story:** As a maintainer, I want the emit format namespaced per recorder, so
a buildah/podman recorder can be added later without breaking the BuildKit contract.

#### Acceptance Criteria

1. THE BuildKit-native recorder SHALL write its files under a recorder-specific
   location (e.g. `<emit-dir>/buildkit/`) and self-identify via a `schema` field
   (`buildkit-native-export/v1`).
2. THE format SHALL allow additional recorders (e.g. buildah/podman) to emit their
   own schema under their own subdirectory WITHOUT changing the BuildKit contract.

### Requirement 6: Documented emit contract (interface with pack)

**User Story:** As a maintainer of both repos, I want the emit output shape
documented as a stable contract, so the pack-side consumer stays in sync.

#### Acceptance Criteria

1. THE emit output shape (plan.json + config.json schemas + how layer tars are
   referenced) SHALL be documented in this spec's design as THE contract.
2. WHEN the contract changes THE change SHALL be reflected here AND in the
   pack-side `buildkit-native-export` spec.

### Requirement 7: Local, MVP validation (tests deferred until pack e2e confirms)

**User Story:** As a maintainer, I want emit-mode validated simply and locally
first, so we confirm pack can actually use it before investing in automated tests.

#### Acceptance Criteria

1. FOR the MVP, emit-mode SHALL be validated by running it against a real `/layers`
   produced by a build and driving the pack-side e2e (the two-build CLI strategy),
   NOT by new Go unit/integration tests.
2. Automated unit + integration tests (the recording-image capture, the
   plan/config serializer, and the export-parity integration test) SHALL be
   DEFERRED until AFTER the pack MVP is confirmed working end-to-end, then added.
3. WHEN automated tests are added, they SHALL NOT introduce `PACK_TEST_*`-style
   env-var-gated registry tests; use a local registry like pack's existing
   testhelpers, consistent with the pack-side testing strategy.
4. Emit-mode is a Go package consumed by pack; ONCE the pack MVP is confirmed, the
   deferred tests become required (the change is not "done" long-term without
   them), but they are explicitly out of scope for the MVP milestone.
