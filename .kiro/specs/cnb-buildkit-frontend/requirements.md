# Requirements: CNB BuildKit Gateway Frontend (`cnbfrontend`)

> **SUPERSEDED / RETIRED.** The frontend approach is retired in favor of Option A
> (build-then-finalize) — see `buildkit-native-export` (both repos) and the decision
> record `cnb-buildkit-frontend/spike-eliminate-metadata-rewrite.md`. Under Option A
> BuildKit builds+pushes a normal image and a lifecycle FINALIZE step authors the CNB
> metadata; no gateway frontend is needed. This spec is kept for history + the spike.

## Introduction

This spec documents the **CNB BuildKit gateway frontend** — the component that
assembles a Cloud Native Buildpacks app image natively inside BuildKit for the
experimental `buildkit-native` export backend (Option C). It lives in
`jericop/cnb-lifecycle` at `buildkit/cnbfrontend` (importable library) with a thin
standalone entrypoint at `cmd/cnb-frontend`.

This is an AS-BUILT spec: the frontend is already implemented and validated as part
of the `buildkit-native-export` MVP. It is captured here so the design and
implementation are documented in their own right (the frontend is a distinct,
reusable component from both the lifecycle emit-mode and the pack backend).

Prior art: **EricHripko/cnbp** — a BuildKit frontend that runs the CNB lifecycle
phases as LLB and assembles the final image `FROM run-image`. Our frontend adopts
cnbp's assembly PATTERN but, unlike cnbp, KEEPS the real lifecycle exporter (via
emit-mode) so it retains full CNB fidelity (metadata labels, SBOM, layer reuse,
process types) instead of reimplementing export. See the
`cnbp-buildkit-frontend` steering note for the prior-art comparison.

### Relationship to the other specs

- `cnb-lifecycle/.kiro/specs/buildkit-native-export` — the exporter EMIT-MODE that
  produces the plan/config/tars this frontend consumes.
- `cnb-pack/.kiro/specs/buildkit-native-export` — the pack `buildkit-native`
  backend that DRIVES this frontend in-process and performs the host-side
  metadata-SHA rewrite after push.

## Glossary

- **Frontend**: a BuildKit gateway program (a `client.BuildFunc`) that builds an
  LLB graph and returns per-platform image references + image configs to BuildKit,
  which then exports the image.
- **In-process driving**: pack calls `client.Client.Build(ctx, opt, product, Build, ch)`
  with the frontend's `Build` function directly, so no separate frontend IMAGE is
  required for the pack integration.
- **Standalone frontend image**: the same `Build` function packaged as a `#syntax=`
  frontend image via `cmd/cnb-frontend`, usable outside pack.
- **Emit contract**: the `plan.json` + `config.json` + persisted layer tars the
  lifecycle exporter writes in emit-mode (`buildkit-native-export/v1`).
- **Assembly base**: the run image, used as the `llb.Image` base the emitted layers
  are extracted onto. The run image is NEVER modified.
- **Layer-order label**: a temporary label (`io.buildpacks.native.layer-order`) the
  frontend records, carrying the ordered emitted diffIDs of the new layers, for the
  pack host-side metadata-SHA rewrite.

## Requirements

### Requirement 1: Importable + standalone dual use

**User Story:** As pack, I want to consume the frontend as a Go library so I can
drive it in-process without publishing/pulling a frontend image; as an external
user, I want the same logic available as a standalone `#syntax=` frontend image.

#### Acceptance Criteria

1. THE frontend logic SHALL be implemented in an importable package
   (`buildkit/cnbfrontend`) exposing a `Build(ctx, client.Client) (*client.Result, error)`
   gateway function.
2. THE standalone entrypoint (`cmd/cnb-frontend`) SHALL be a thin wrapper that runs
   the same `Build` via `grpcclient.RunFromEnvironment`.
3. WHEN pack drives the frontend in-process via `client.Client.Build`, THE build
   SHALL NOT require a separately published frontend image.
4. Option keys and label constants the pack consumer must match SHALL be EXPORTED
   from the package (single source of truth, no drift).

### Requirement 2: Inputs via frontend options

**User Story:** As pack, I want to pass the builder image, run image, platforms,
CNB uid/gid, platform API, order, and registry auth as frontend options so the
frontend can run the lifecycle and assemble the image.

#### Acceptance Criteria

1. THE frontend SHALL read its inputs from the gateway `BuildOpts().Opts` using the
   exported option keys (builder image, run image, optional lifecycle-overlay image,
   platforms, platform API, uid, gid, optional order.toml, optional registry auth,
   image name, optional insecure registries).
2. IF the required builder-image or run-image option is missing, THEN THE frontend
   SHALL return a descriptive error.
3. WHERE the platforms option is empty, THE frontend SHALL default to the host
   platform.
4. WHERE the platform-API option is empty, THE frontend SHALL default to a
   supported version.

### Requirement 3: Run the lifecycle phases + exporter emit-mode in BuildKit

**User Story:** As a platform operator, I want the untrusted buildpack code to run
in BuildKit's sandbox via the real lifecycle so CNB semantics are preserved.

#### Acceptance Criteria

1. THE frontend SHALL build an LLB graph per platform: builder base → (optional
   lifecycle overlay) → setup dirs → (optional order.toml) → copy app source →
   analyzer → detector → restorer → builder → exporter in EMIT-MODE.
2. THE exporter SHALL be invoked with `-emit-export-plan <dir>` so it records the
   plan/config/tars instead of pushing.
3. THE lifecycle phases SHALL run as the CNB uid/gid with the CNB platform API env.
4. WHERE a persistent buildpack cache is used, it SHALL be scoped per architecture.

### Requirement 4: Assemble one layer per emitted CNB layer FROM the run image

**User Story:** As pack, I want the assembled image to have the same layer
boundaries as the emit plan so per-layer metadata SHAs map to real layers (enabling
analyzer previous-image restore and buildpack-layer patching).

#### Acceptance Criteria

1. THE frontend SHALL resolve the run image from the analyzer-written
   `/layers/analyzed.toml` (digest-pinned, fully-qualified) and use it as the
   `llb.Image` assembly base.
2. IF `analyzed.toml` is unavailable, THEN THE frontend SHALL fall back to
   normalizing the raw run-image option.
3. THE frontend SHALL assemble the image by extracting EACH non-reused emitted layer
   tar as its OWN layer, in plan order, onto the run-image base.
4. THE run image SHALL NOT be modified in any way.
5. Reused (run-image base) layers SHALL NOT be re-added (they are already present in
   the base).

### Requirement 5: Set the image config + labels from the emit contract

**User Story:** As a consumer of the final image, I want the correct entrypoint,
env, working dir, and CNB labels so the image runs and is CNB-compliant.

#### Acceptance Criteria

1. THE frontend SHALL overlay the emitted `config.json`
   (entrypoint/cmd/workingDir/env/labels including `io.buildpacks.lifecycle.metadata`)
   onto the run-image base config and return it to BuildKit via the exporter image
   config result key.
2. THE env SHALL merge the run-image base env with the emitted CNB env (CNB keys
   override).
3. THE frontend SHALL record the ordered emitted diffIDs of the new layers in the
   `io.buildpacks.native.layer-order` label for the pack host-side rewrite.
4. ON EVERY build (cold or rebuild), THE frontend SHALL RE-RECORD this label from
   THAT build's emit plan (its contents legitimately differ as reused-vs-new layers
   change build-to-build); it SHALL NOT carry forward a stale label from a previous
   image. The label is a REQUIRED, DURABLE output (pack's rewrite keeps it on the
   pushed image) so repeated rebuilds and any self-healing fix can perform the
   positional remap. It stays valid across `pack rebase` (which does not change
   app/buildpack layer diffIDs). See the pack spec Requirement 7b.

### Requirement 6: Native multi-platform output (no intermediate tags)

**User Story:** As a user, I want one native multi-arch image with no intermediate
per-arch tags.

#### Acceptance Criteria

1. THE frontend SHALL build each requested platform (in parallel) and return
   per-platform references plus per-platform image configs.
2. WHEN more than one platform is requested, THE frontend SHALL return the platforms
   metadata so BuildKit exports a single OCI image index.
3. THE frontend SHALL NOT create intermediate per-arch tags.

### Requirement 7: Known limitations (documented, not fixed here)

**User Story:** As a maintainer, I want the frontend's known limitations captured so
consumers understand the boundaries.

#### Acceptance Criteria

1. THE frontend CANNOT rewrite `io.buildpacks.lifecycle.metadata` per-layer SHAs to
   the produced diffIDs itself (the gateway `Reference` API does not expose produced
   diffIDs; BuildKit computes them at export time after the frontend returns) — this
   is done host-side by pack.
2. WHEN the frontend is used standalone (without pack's host-side rewrite), the FIRST
   build SHALL produce a runnable image, but a REBUILD SHALL fail at the analyzer
   previous-image restore (documented limitation; a standalone rebuild story is a
   follow-up).
3. THE per-layer assembly SHALL NOT assume the run image provides `/bin/sh` or
   `tar` (distroless/static run images have neither). Extraction SHALL use `tar`
   from the BUILD image (which reliably provides a shell + tar), run against the
   run-image rootfs — avoiding both the missing-tooling problem and the
   shared-library loading problems of executing a run-image binary. The run image
   supplies only the base filesystem and is never modified. NOTE: the MVP validation
   temporarily ran `tar` via the run image's own shell (ubuntu-noble has it); that
   assumption is being removed in favor of build-image `tar` (a required correctness
   item — see task 10).
