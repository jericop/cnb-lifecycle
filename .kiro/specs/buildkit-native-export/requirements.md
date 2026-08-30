# Requirements: Lifecycle BuildKit-Native Export (emit-mode + finalize)

## Introduction

This is the LIFECYCLE-SIDE half of the "BuildKit-Native Export" (Option A —
build-then-finalize) effort. The pack-side half lives in
`jericop/cnb-pack@buildkit-native-export`. This spec owns the lifecycle changes; the
pack spec owns consuming them.

The design (see the frontend spec's `spike-eliminate-metadata-rewrite.md` for the
decision record): BuildKit BUILDS and PUSHES a normal app image; a lifecycle-owned
FINALIZE step then makes that pushed image buildpacks-compliant by authoring the
correct `io.buildpacks.lifecycle.metadata` from the image's ACTUAL produced layer
diffIDs plus an ordered plan the build surfaced as a label. This keeps 100% of CNB
metadata authorship in the lifecycle, requires NO frontend and NO post-push layer
changes, and produces correct metadata authored against reality (not patched from a
stale emitted label).

> NOTE (as-implemented): the build-phase label is
> `io.buildpacks.lifecycle.prepared-metadata` (builder-agnostic), NOT the spike-era
> `io.buildpacks.buildkit.native.build-metadata`; and the keep-label flag is
> `-keep-prepared-metadata-label`. This spec was written during the spike and still
> uses the older label name in places below — read it as the current name. The
> as-built assembly is pack-side `llb.Copy` from emitted layer SOURCE REFS (no
> frontend, no persisted layer tars); the finalize step AUTHORS the metadata.

The lifecycle provides two things:

1. **Emit-mode (the ordered plan).** The exporter, run in emit-mode, computes the
   ordered layer plan (which layers, order, new-vs-reused, intended diffIDs, history,
   layer identity, run-image boundary). This plan is surfaced onto the built image as
   the `io.buildpacks.lifecycle.prepared-metadata` LABEL (a build-phase artifact,
   distinct from the final CNB metadata label).
2. **Finalize (author the real metadata).** A library API (+ subcommand) that, given
   a built image ref and the build-metadata label, reads the image's ACTUAL produced
   layer diffIDs and authors the correct `io.buildpacks.lifecycle.metadata` (per-layer
   SHAs = produced diffIDs; `RunImage.TopLayer`/`Reference` = the run-image boundary),
   then re-pushes ONLY the image config + manifest.

Detector and builder are unchanged. Only the export/metadata authoring gains these
additive, opt-in modes. Default `export` behavior is unchanged.

### What changed from the earlier iteration

The earlier design had a custom frontend re-extract layers and pack REWRITE the
metadata SHAs post-push (a workaround for BuildKit reassigning diffIDs during
re-extraction). This is replaced: no re-extraction, and metadata is AUTHORED (not
patched) by a lifecycle finalize step from the produced diffIDs. Emit-mode no longer
persists layer tars or drives assembly; it only produces the ordered plan for the
label. The `io.buildpacks.native.layer-order` temp label is superseded by the richer
`io.buildpacks.buildkit.native.build-metadata` label.

## Glossary

- **emit-mode**: an opt-in export path that computes the ordered layer plan instead
  of assembling/pushing an image.
- **Layer Plan**: the ordered set of layer descriptors emit-mode produces (order,
  new-vs-reused, intended diffID, identity, history) + the run-image boundary.
- **build-metadata label**: `io.buildpacks.buildkit.native.build-metadata` — the
  serialized Layer Plan, carried as an image LABEL by the build phase for finalize to
  consume. Namespaced `io.buildpacks.buildkit.native.*`; distinct from the final
  `io.buildpacks.lifecycle.metadata`.
- **Produced diffID**: the diffID BuildKit actually assigned to a layer at export
  (read from the built image config); authoritative for the finalized metadata.
- **Finalize**: the lifecycle library API (+ subcommand) that authors
  `io.buildpacks.lifecycle.metadata` on a built image from the build-metadata label +
  the produced diffIDs, and re-pushes config+manifest only.

## Requirements

### Requirement 1: Additive, opt-in emit-mode (the ordered plan)

**User Story:** As a lifecycle maintainer, I want emit-mode additive and opt-in, so
default `export` behavior is unchanged.

#### Acceptance Criteria

1. THE lifecycle SHALL provide an OPT-IN way to run export in emit-mode (e.g.
   `-emit-export-plan <dir>` or an equivalent library entry point) that does NOT
   mutate or push an image.
2. WHEN emit-mode is NOT selected THE existing export behavior SHALL be unchanged.
3. THE emit-mode SHALL REUSE the existing layer-building and metadata logic
   (`layers.Factory`, `files.LayersMetadata`/`BuildMetadata`, `platform` labels), not
   a parallel reimplementation.

### Requirement 2: Emit the ordered Layer Plan

**User Story:** As the finalize step, I want an ordered, complete description of the
layers, so I can map the image's produced diffIDs to CNB layer identities.

#### Acceptance Criteria

1. THE emitted Layer Plan SHALL list, IN ORDER, every layer the exporter would add
   (buildpack launch layers, launcher, launcher-process-types, app slices, SBOM
   launch layer), matching the exporter's current ordering.
2. EACH plan entry SHALL indicate whether it is a REUSED run-image/base layer or a
   NEW layer produced by this build.
3. EACH entry SHALL include its intended diffID, its layer IDENTITY (which CNB
   component / buildpack+layer it is), and the `v1.History` the exporter would record.
4. THE plan SHALL include the run-image rebase boundary (runImage.reference,
   topLayer) consistent with a normal export's lifecycle-metadata label.

### Requirement 3: Surface the plan as the build-metadata LABEL (not a layer)

**User Story:** As the finalize step, I want the plan carried as image metadata, not
a runtime layer.

#### Acceptance Criteria

1. THE emitted plan SHALL be serializable to a single image LABEL
   `io.buildpacks.buildkit.native.build-metadata` (JSON), with a `schema` field for
   versioning.
2. THE emit-mode SHALL NOT require adding any image LAYER to carry the plan.
3. THE build phase SHALL NOT pre-write a valid final `io.buildpacks.lifecycle.metadata`
   label with stale SHAs; the build-metadata label is a distinct, explicitly
   build-phase artifact.

### Requirement 4: Finalize authors CNB metadata from the produced image

**User Story:** As a buildpacks user, I want a lifecycle-owned step that makes a
pushed image buildpacks-compliant using the image's actual layers.

#### Acceptance Criteria

1. THE lifecycle SHALL provide a FINALIZE library API (+ a subcommand wrapper) that
   takes a built image reference (single image or manifest list) and, using the
   `io.buildpacks.buildkit.native.build-metadata` label and the image's ACTUAL
   produced layer diffIDs, authors the correct `io.buildpacks.lifecycle.metadata`.
2. THE authored metadata SHALL set every per-layer `sha` (`App[]`, `Launcher`,
   `Config`, `ProcessTypes`, each `Buildpacks[].layers[<name>].sha`, and `sbom`) to
   the ACTUAL produced diffID for that layer, and SHALL set
   `RunImage.TopLayer`/`Reference` to the run-image boundary.
3. FINALIZE SHALL also author the other export labels it is responsible for
   (`io.buildpacks.build.metadata`, `io.buildpacks.project.metadata`, exec-env when
   applicable) consistent with a normal export, from data carried in the
   build-metadata label.
4. FINALIZE SHALL re-push ONLY the image config + manifest (+ index for a manifest
   list). It SHALL NOT add, remove, re-tar, or re-upload any layer.
5. FINALIZE SHALL be IDEMPOTENT: finalizing an already-finalized image re-authors
   identical metadata (no drift across repeated cycles).
6. THE finalize config+manifest re-push SHALL be TAG-ATOMIC: the tag resolves to
   either the pre-finalize or the finalized image, never a partial one; on failure
   the pre-finalize image remains pullable/runnable.

### Requirement 5: Finalize is consumable as a library (like Rebaser)

**User Story:** As pack, I want to call finalize in-process the way I call
`phase.Rebaser`, so metadata authorship stays in the lifecycle.

#### Acceptance Criteria

1. THE finalize API SHALL be a Go package/function pack can import and call directly
   (single source of truth; no duplicated CNB metadata logic in pack).
2. THE finalize API SHALL operate on a registry image reference using standard image
   access (e.g. go-containerregistry), consistent with how the lifecycle/pack already
   read+write image config for rebase.
3. THE subcommand wrapper SHALL allow the same operation to run standalone (for a
   future self-healing/repair tool), reading the durable build-metadata label from an
   already-pushed image.

### Requirement 6: Rebase-safe, diffID-consistent result

**User Story:** As a buildpacks user, I want rebase to keep working after finalize.

#### Acceptance Criteria

1. AFTER finalize THE `io.buildpacks.lifecycle.metadata` `RunImage.TopLayer` SHALL
   correctly identify the run-image/app boundary in the built image, so the Rebaser
   succeeds.
2. AFTER finalize every per-layer metadata SHA SHALL equal the actual produced layer
   diffID, so buildpack-contributed-layer patching can locate layers by sha.
3. App-layer diffIDs NEED NOT match a registry/oci-layout export of the same build;
   correctness comes from finalize authoring metadata against the ACTUAL layers.

### Requirement 7: Platform API compatibility (>= 0.7)

**User Story:** As a buildpacks user on a current buildpack (Buildpack API 0.7+), I
want emit-mode + finalize to work across the Platform API versions this lifecycle
supports.

#### Acceptance Criteria

1. THE emit-mode SHALL produce a correct plan for every Platform API this lifecycle
   supports (0.7 through 0.15) by recording exactly the operations the exporter
   performs for the negotiated Platform API (no per-version logic).
2. THE finalized `io.buildpacks.lifecycle.metadata` (and the config env
   `CNB_PLATFORM_API`) SHALL reflect the negotiated Platform API, as a normal export
   would.

### Requirement 8: Local, MVP validation (repeated cycles), tests follow

**User Story:** As a maintainer, I want emit-mode + finalize validated locally with
REPEATED rebuilds/rebases before investing in automated tests.

#### Acceptance Criteria

1. FOR the MVP, emit-mode + finalize SHALL be validated by driving the pack-side e2e
   against a local registry (the two-build strategy), covering ≥2 rebuilds, ≥2
   rebases, and a rebuild-after-rebase — NOT only the first cycle, and NOT by new Go
   unit/integration tests initially.
2. Automated unit + integration tests (emit plan serializer, the finalize authoring
   from produced diffIDs, and an export-parity check) SHALL be added AFTER the pack
   MVP is confirmed. They SHALL NOT introduce `PACK_TEST_*`-style env-var-gated
   registry tests; use a local registry like pack's testhelpers.
