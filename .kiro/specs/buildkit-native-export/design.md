# Design: Lifecycle BuildKit-Native Export (emit-mode + finalize)

## Overview (Option A — current design)

The lifecycle provides two additive, opt-in capabilities for the BuildKit-native
backend:

1. **Emit-mode** — the exporter computes the ordered **Layer Plan** (which layers,
   order, new-vs-reused, intended diffID, identity, history, run-image boundary)
   WITHOUT assembling or pushing an image. This plan is surfaced onto the built
   image as the `io.buildpacks.buildkit.native.build-metadata` LABEL (a build-phase
   artifact, distinct from the final CNB metadata label).
2. **Finalize** — a library API (+ subcommand) that, given a built+pushed image
   reference, reads the image's ACTUAL produced layer diffIDs plus the
   `io.buildpacks.lifecycle.prepared-metadata` label, and AUTHORS the correct
   `io.buildpacks.lifecycle.metadata` (per-layer SHAs = produced diffIDs; RunImage
   boundary), re-pushing ONLY the config + manifest (+ index).

> NOTE (as-implemented): the build-phase label is
> `io.buildpacks.lifecycle.prepared-metadata` (builder-agnostic), NOT the spike-era
> `io.buildpacks.buildkit.native.build-metadata` used in places below; and the
> keep-label flag is `-keep-prepared-metadata-label`. The as-built assembly is
> pack-side `llb.Copy` from emitted layer SOURCE REFS — there is NO custom BuildKit
> frontend and NO persisted layer tars. The "AS-BUILT (MVP)" section further down
> describes an intermediate spike (frontend + host-side SHA rewrite +
> `io.buildpacks.native.layer-order`) that was SUPERSEDED; a correction banner marks
> it.

The flow (Option A): BuildKit builds + pushes a normal image (runnable, not yet
CNB-compliant) carrying the build-metadata label; then finalize (called by pack,
like `phase.Rebaser`) authors the real metadata from the produced layers. No
frontend, no per-layer re-extraction, no post-push LAYER changes.

Decision record + rejected alternatives:
`cnb-lifecycle/.kiro/specs/cnb-buildkit-frontend/spike-eliminate-metadata-rewrite.md`.

### What changed from the earlier emit design (SUPERSEDED transport)

The earlier design transported the plan/config as FILES (`plan.json` +
`config.json`) plus PERSISTED layer tars under `<emit-dir>/buildkit/layers/`, for a
frontend to re-assemble and pack to REWRITE metadata SHAs post-push. That file-based
transport + tar persistence + the `io.buildpacks.native.layer-order` temp label are
SUPERSEDED. The plan is now surfaced as the single
`io.buildpacks.buildkit.native.build-metadata` LABEL, no tars are persisted, and
metadata is AUTHORED by finalize (not patched). The RecordingImage plan-computation
below is still used to COMPUTE the plan; only the transport + the post-step differ.

## Finalize: author `io.buildpacks.lifecycle.metadata` from the produced image

Inputs: a built+pushed image reference (single image or manifest list); read access
to the registry (go-containerregistry, as rebase uses).

Steps (per image; for a manifest list, per child then re-push the index):

1. Read the image config → `RootFS.DiffIDs` (the PRODUCED diffIDs, in order) + the
   existing labels.
2. Read `io.buildpacks.buildkit.native.build-metadata` → the ordered plan (new-vs-
   reused, identity, intended diffID, history, run-image reference/topLayer).
3. Map plan entries → produced diffIDs positionally: the NEW layers occupy the
   trailing `len(new layers)` positions of `RootFS.DiffIDs`, in plan order; reused
   layers correspond to the run-image base diffIDs below the boundary. This yields,
   for each CNB layer identity, its ACTUAL produced diffID.
4. Build `files.LayersMetadata` with every per-layer `sha` set to the produced
   diffID (`App[]`, `Launcher`, `Config`, `ProcessTypes`, each
   `Buildpacks[].layers[<name>].sha`, `sbom`) and `RunImage.TopLayer`/`Reference`
   from the plan. Author the other labels (`build.metadata`, `project.metadata`,
   exec-env) from data carried in the build-metadata label.
5. Set `io.buildpacks.lifecycle.metadata` (+ the other labels) on the config;
   optionally KEEP the build-metadata label (durable, for self-healing) — do not add
   or change layers.
6. Re-push config + manifest (+ index). Tag-atomic; idempotent (re-running authors
   identical metadata).

Why this is authoring, not patching: the metadata is built FROM the produced
diffIDs the first time, so there is no "emitted vs produced" mismatch to reconcile —
the earlier design emitted metadata with the WRONG (pre-produce) SHAs and patched
them; finalize never writes wrong SHAs.

### Finalize is a library (consumed like Rebaser)

The finalize logic lives in the lifecycle as an importable package (e.g.
`phase/finalize` or similar) with a subcommand wrapper. Pack imports and calls it in
`NativeBackend` after the push, the way `pkg/client/rebase.go` calls `phase.Rebaser`.
The subcommand wrapper enables a standalone/self-healing use later. This keeps CNB
metadata authorship in one place; pack does not duplicate it.

## The build-metadata label

`io.buildpacks.buildkit.native.build-metadata` (JSON, with a `schema` field) is the
serialized ordered Layer Plan (see "Emit the ordered Layer Plan" below). It is the
plan surfaced as a config LABEL rather than files. It carries everything finalize
needs; new fields may be added without adding image layers. It is namespaced
`io.buildpacks.buildkit.native.*` and is DISTINCT from the final
`io.buildpacks.lifecycle.metadata` (the build phase never pre-writes a valid final
label).

## Layer SOURCE REFS: emit assembles-by-copy, not by tar (decision)

Emit-mode records each NEW layer as a SOURCE REFERENCE (the filesystem source the
lifecycle layer factory already has) instead of building/persisting a tar, so the
consumer (pack) assembles the image with `llb.Copy` from those sources — no tar
build, no extraction, no materialization of large layers, and no run-image
shell/tar. Decision record + rationale:
`cnb-buildkit-frontend/spike-eliminate-metadata-rewrite.md` (part 8).

WHY: the layer factory receives each layer's SOURCE at build time —
`DirLayer(id, fromDir)`, `SliceLayers(dir, slices)`, `LauncherLayer(path)`. That
source (and, for app slices, the exact per-slice file selection the lifecycle
computes) is authoritative. Emitting it lets BuildKit copy the files natively;
recomputed diffIDs are reconciled by finalize. This avoids reverse-engineering paths
from layer IDs (fragile) and avoids materializing the app/dependency layers.

Each `LayerOp` gains an optional `Source` (populated in emit-mode for
filesystem-backed layers; the tar is NOT built for these):

```jsonc
"source": {
  "dir":     "/layers/<bp>/<layer>",   // built-state path to copy from
  "include": ["..."],                  // optional (app slices): only these paths
  "uid": 1001, "gid": 1001,            // normalization to apply on copy
  "mode":    493,                      // optional (launcher: 0755)
  "dest":    "/cnb/lifecycle/launcher" // optional: destination if != dir
}
```

SYNTHESIZED layers with NO filesystem source (e.g. `ProcessTypesLayer`, which builds
symlinks in-memory) fall back to a SMALL emitted tar (kilobytes) that the consumer
`llb.Copy`s from a tiny extracted tree. Only these synthesized layers are
materialized; the large real layers are copied from their sources.

Normal (non-emit) export is unchanged — it still builds tars and pushes an image.
The source-ref behavior is emit-mode-only.

---

## (Retained) Emit-mode plan computation — how the ordered plan is produced

The plan computation below is still used. Only the TRANSPORT (now a label, not
files/tars) and the downstream step (now finalize authoring, not frontend
re-assembly + rewrite) have changed per the Option A overview above.

## Where this plugs into the existing exporter

`phase/exporter.go` `Export(opts ExportOptions)` currently INTERLEAVES two things:

1. **Layer creation** via `layers.Factory` (`DirLayer`/`TarLayer`/`SliceLayers`/
   `LauncherLayer`/`ProcessTypesLayer`) — tars dirs into layers with diffIDs.
2. **Image mutation** — `opts.WorkingImage.AddLayerWithDiffIDAndHistory(...)`,
   `ReuseLayerWithHistory(...)`, `SetEntrypoint/SetCmd/SetEnv/SetWorkingDir`,
   `SetLabel(io.buildpacks.*)` — done through the `imgutil.Image` interface.

The helper `addBuildpackLayers`/`addLauncherLayers`/`addAppLayers`/`setLabels`
(and `addOrReuseBuildpackLayer`) build a `files.LayersMetadata` and mutate the
image in one pass.

Emit-mode DECOUPLES these: keep step 1 (produce the layer tars + diffIDs +
`files.LayersMetadata`), but replace step 2's image mutation with RECORDING the
same operations into a plan + config, then serialize them.

### Approach: a "sink" abstraction (minimal, additive)

Introduce an internal export "sink" the exporter writes layer/config operations to.
Today's sink mutates an `imgutil.Image` (existing behavior). Emit-mode provides an
alternative sink that records:
- `AddLayer(diffID, tarPath, history, id)` → append a NEW-layer plan entry.
- `ReuseLayer(diffID, history, id)` → append a REUSED-layer plan entry (by digest).
- `SetEntrypoint/SetCmd/SetEnv/SetWorkingDir/SetLabel` → accumulate the Image
  Config.
This keeps ordering and metadata computation identical; only the destination
changes. (If a full sink refactor is too large for the MVP, an equivalent is to run
the existing flow against an in-memory/recording `imgutil.Image` implementation that
captures the same calls — reusing `imgutil` fakes — and then serialize what it
captured. Either way, no CNB logic is duplicated.)

## The `RecordingImage` type (the MVP seam)

Lives in cnb-lifecycle (proposed package `phase/emit`). It implements the full
`imgutil.Image` interface by embedding a run-image-backed `imgutil.Image` (so READS
like `TopLayer()`, `Env("PATH")`, `Labels()` return real run-image values) and
OVERRIDING the mutating methods to record them into an in-memory plan + config.
Its `Save()` serializes to `plan.json` + `config.json` instead of pushing.

```go
package emit

import (
	"github.com/buildpacks/imgutil"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Schema is the emit-contract version. Bump on any breaking shape change.
const Schema = "buildkit-native-export/v1"

// LayerOp is one recorded layer operation, in the order the exporter emitted it.
type LayerOp struct {
	ID      string      `json:"id"`               // e.g. "<bp-id>:<layer>", "launcher", "app", "sbom"
	Reused  bool        `json:"reused"`           // true => reference run-image layer by digest
	DiffID  string      `json:"diffID"`           // sha256:... (always present)
	TarPath string      `json:"tar,omitempty"`    // path to the ACTUAL built tar; empty when Reused
	History v1.History  `json:"history"`          // exact history the exporter recorded
}

// ImageConfig is the accumulated container config the exporter would have set.
type ImageConfig struct {
	Entrypoint []string          `json:"entrypoint"`
	Cmd        []string          `json:"cmd"`        // intentionally empty as today
	WorkingDir string            `json:"workingDir"`
	Env        map[string]string `json:"env"`
	Labels     map[string]string `json:"labels"`
}

// RecordingImage implements imgutil.Image. It forwards READS to the embedded
// run-image-backed image and RECORDS WRITES. Save() emits the contract files.
type RecordingImage struct {
	imgutil.Image // embedded run-image-backed image: satisfies reads + any method we don't override

	OutputDir string // where Save() writes plan.json + config.json

	// captured state (ordered)
	layers []LayerOp
	config ImageConfig
}

func NewRecordingImage(runImageBacked imgutil.Image, outputDir string) *RecordingImage {
	return &RecordingImage{
		Image:     runImageBacked,
		OutputDir: outputDir,
		config: ImageConfig{
			Cmd:    []string{},
			Env:    map[string]string{},
			Labels: map[string]string{},
		},
	}
}

// --- WithEditableLayers: record instead of mutate ---

func (r *RecordingImage) AddLayerWithDiffIDAndHistory(path, diffID string, history v1.History) error {
	r.layers = append(r.layers, LayerOp{ID: idFor(history), Reused: false, DiffID: diffID, TarPath: path, History: history})
	return nil
}

func (r *RecordingImage) ReuseLayerWithHistory(diffID string, history v1.History) error {
	r.layers = append(r.layers, LayerOp{ID: idFor(history), Reused: true, DiffID: diffID, History: history})
	return nil
}

// AddLayer / AddLayerWithDiffID / AddOrReuseLayerWithHistory / ReuseLayer:
// the exporter only calls the *WithHistory variants above, but implement the rest
// as thin recorders (or delegate) so the interface is fully satisfied.

// --- WithEditableConfig: record instead of mutate ---

func (r *RecordingImage) SetEntrypoint(v ...string) error { r.config.Entrypoint = v; return nil }
func (r *RecordingImage) SetCmd(v ...string) error        { r.config.Cmd = v; return nil }
func (r *RecordingImage) SetWorkingDir(dir string) error  { r.config.WorkingDir = dir; return nil }
func (r *RecordingImage) SetLabel(k, v string) error      { r.config.Labels[k] = v; return nil }

func (r *RecordingImage) SetEnv(k, v string) error {
	r.config.Env[k] = v
	return nil
}

// NOTE: Env(key) is a READ the exporter uses to compute PATH; it is NOT overridden,
// so it forwards to the embedded run-image (returns the run image's real PATH).
// After the exporter calls SetEnv("PATH", <prepended>), our recorded PATH is the
// FINAL value — matching a normal export. See "Decision: PATH final-value" below.

// --- Save: emit the contract instead of pushing ---

func (r *RecordingImage) Save(_ ...string) error {
	plan := Plan{
		Schema:   Schema,
		RunImage: r.runImageRef(), // reference + topLayer, from reads on the embedded image
		Layers:   r.layers,
	}
	// write plan.json + config.json (with Schema) to r.OutputDir
	return writeContract(r.OutputDir, plan, ImageConfigWithSchema{Schema, r.config})
}
```

Notes on the design:
- **Reads are free.** By embedding `imgutil.Image`, every getter (`TopLayer`,
  `Env`, `Labels`, `History`, `Architecture`, ...) is satisfied by the run-image
  backing without us implementing it. We only override the ~8 mutators + `Save`.
- **`idFor(history)`** derives the plan entry `id` from the recorded history's
  `CreatedBy` (the exporter already encodes buildpack/layer identity there via
  `layers.BuildpackLayerName`/`LauncherLayerName`/etc.). This keeps the exporter
  untouched for the MVP. **FOLLOW-UP (post-MVP, tracked):** deriving the id from
  `CreatedBy` is only acceptable if it is GUARANTEED to match the id the consumer
  needs; if there is any ambiguity, thread an explicit `id` through the exporter's
  add/reuse calls (a small, additive change) so the plan carries a first-class id
  rather than a parsed one. See "Follow-up items (post-MVP)".
- **`runImageRef()`** reads `TopLayer()` off the embedded image and pairs it with
  the `RunImageRef` the caller already knows (passed in via the emit path), matching
  what `Export()` puts in `meta.RunImage`.
- **No re-tar.** `AddLayerWithDiffIDAndHistory` records the `path` it is handed —
  the lifecycle's already-built tar under `/layers` — guaranteeing diffID parity.
- **`Save()` capturing = zero change to `phase/exporter.go`** (option (a) from
  Task 1). The unconditional `saveImage` at the end of `Export()` calls this
  `Save()`, which writes the contract files instead of pushing.

The corresponding on-disk output shape follows.

## Recorded decisions (for the PR / RFC)

These are deliberate design choices to carry into the eventual PR and possibly the
RFC.

### Decision: PATH final-value (do NOT preserve assembly steps)

The exporter computes `PATH` by READING the run image's `PATH` and prepending
`/cnb/process` + `/cnb/lifecycle` before calling `SetEnv("PATH", <final>)`.
Emit-mode records only the FINAL `PATH` value (the value passed to the last
`SetEnv("PATH", ...)`), NOT the intermediate run-image value or the prepend steps.

Rationale: the final image only needs the final `PATH`; the assembly steps are an
implementation detail of how the exporter derived it. A BuildKit/buildah/podman
consumer applies the final env verbatim. We are NOT aware of a consumer that needs
to re-derive PATH from parts.

If a future consumer DOES need the pre-prepend base PATH (e.g. to re-prepend after
its own layer that mutates PATH), we would additionally emit the base run-image PATH
as a separate field — but that is explicitly OUT of scope unless such a need is
demonstrated. This decision is documented here so it can be reviewed in the PR/RFC.

### Decision: emit format is recorder-namespaced (buildah/podman-extensible)

The emitted files are written under a RECORDER-namespaced layout so additional
assembly targets (buildah/podman, others) can define their own emit schema in the
future WITHOUT colliding with the BuildKit-native one. The `RecordingImage` is the
BuildKit-native recorder; other recorders can implement `imgutil.Image` the same
way and emit their own contract.

Layout (proposed):
```
<emit-dir>/
  buildkit/            # this recorder's output (schema: buildkit-native-export/v1)
    plan.json
    config.json
  <future: buildah/, podman/, ...>
```
Each recorder's files self-identify via their `schema` field
(`<recorder>-native-export/v<N>`), so a consumer validates both the directory it
reads AND the `schema` string. The `Schema` constant and `OutputDir` on
`RecordingImage` make the recorder responsible for its own subdirectory + schema
tag. This keeps the door open for a `buildah`/`podman` recorder later with zero
change to the BuildKit contract.

## Platform API compatibility (0.7–0.15)

**Platform API is NOT the buildpack-level `api`.** A buildpack's `buildpack.toml`
`api = "0.7"` is the **Buildpack API** (the buildpack↔lifecycle contract). The
**Platform API** is negotiated between the PLATFORM (pack) and the lifecycle at
build time and is what `phase.Exporter.PlatformAPI` holds. A buildpack pinned at
Buildpack API 0.7 does NOT pin the Platform API — pack selects the Platform API
(typically the newest the bundled lifecycle supports).

This lifecycle supports Platform APIs **0.7 … 0.15** (`api/apis.go`:
`Platform = [...]"0.7".."0.15"`). Every API-gated branch in `phase/exporter.go` is
an `AtLeast` check, so a LOWER Platform API just takes the simpler path — never a
conflicting one:

| Gate            | Behavior added at/above the version                  |
|-----------------|------------------------------------------------------|
| `AtLeast 0.8`   | SBOM launch layer                                    |
| `AtLeast 0.9`   | build report shape (below 0.9 populates BOM)         |
| `AtLeast 0.11`  | copy buildpacksio SBOMs                              |
| `AtLeast 0.12`  | extension layers + run-image mirrors metadata        |
| `AtLeast 0.15`  | exec-env label (when `ExecEnv != ""`)                |

**Implication for emit-mode: none beyond "record what the exporter does".** Because
`RecordingImage` records the operations the exporter actually performs for the
NEGOTIATED Platform API, it produces a correct plan/config for ANY supported version
0.7–0.15 with NO per-version logic in the recorder: if the exporter skips the SBOM
layer at 0.7, the recorder simply never receives that `AddLayer` call. The emitted
`config.json` carries `CNB_PLATFORM_API` (set by the exporter's
`SetEnv(EnvPlatformAPI, ...)`), so a downstream rebase/consumer knows which API
produced the image. **Compatibility target: Platform API >= 0.7**, matching current
production buildpacks; no additional effort required for the recorder.

## Follow-up items (post-MVP)

- **Explicit layer id:** replace `idFor(history)` (parsing `CreatedBy`) with an
  explicit `id` threaded through the exporter's add/reuse calls, IF history-derived
  ids are not guaranteed to match the consumer's needs. Additive, small.
- **Automated tests:** unit tests (recorder capture + serializer) and the
  export-parity integration test — deferred until the pack MVP is confirmed
  (see requirements Req 7 + tasks Task 7).
- **PATH base value:** emit the pre-prepend run-image PATH as a separate field only
  if a real consumer needs it (see PATH decision above).

## AS-BUILT (MVP) — SUPERSEDED intermediate spike (frontend + host-side SHA rewrite)

> **SUPERSEDED — do not treat this section as the current as-built.** This describes
> an INTERMEDIATE spike that used a CNB BuildKit gateway frontend
> (`buildkit/cnbfrontend`, `cmd/cnb-frontend`) that re-extracted layer tars, emit-mode
> that PERSISTED layer tars under `<emit-dir>/buildkit/layers/`, a temp
> `io.buildpacks.native.layer-order` label, and a pack-side host-side metadata-SHA
> REWRITE after push. ALL of that has been REMOVED. The CURRENT as-built (see the
> Option A overview + the "Layer SOURCE REFS" section above) is: emit-mode records
> per-layer SOURCE REFS (no tar persistence); pack assembles `FROM run-image` via
> in-process `llb.Copy` (no frontend); and the lifecycle `phase/finalize` library
> AUTHORS `io.buildpacks.lifecycle.metadata` from the produced diffIDs post-push (no
> SHA rewrite). The build-phase label is `io.buildpacks.lifecycle.prepared-metadata`.
> The text below is retained only as a record of the spike.

The intermediate spike landed two things beyond the emit-mode recorder described
above (both now retired):

1. **The assembly mechanism is a CNB BuildKit gateway FRONTEND, not pack-side
   pure-LLB.** A plain `client.Solve` with a raw `llb.State` cannot set the output
   image config/labels (that needs the gateway result API,
   `exptypes.ExporterImageConfigKey`). So this repo adds `buildkit/cnbfrontend`
   (prior art: EricHripko/cnbp), which pack drives IN-PROCESS via
   `client.Client.Build`. The frontend runs the lifecycle phases as RUNs (with
   `llb.Network(NetMode_HOST)`), runs the exporter in EMIT-MODE, then assembles
   `FROM run-image` by extracting each emitted layer tar as its OWN layer (one RUN
   per emitted CNB layer, plan order), and sets the image config/labels from
   `config.json` via `AddMeta`. Run image is resolved from
   `/layers/analyzed.toml`. Returns per-platform refs for native multi-arch.

2. **Emit-mode PERSISTS layer tars.** The exporter's own artifacts temp dir is
   `defer os.RemoveAll`'d, so the frontend cannot read the tars at their original
   `/layers/.../*.tar` path after the export RUN. `RecordingImage.Save()` therefore
   COPIES each new layer's tar to `<emit-dir>/buildkit/layers/NNN-<name>.tar`
   (`LayersSubdir = "layers"`) and records the RELATIVE `tar` path in `plan.json`.
   That is why the contract's `tar` field is relative to the emit/layers root.

**Where the metadata-SHA rewrite lives (NOT here):** the gateway `Reference` API
does not expose the produced layer diffIDs, and BuildKit computes them at export
time (after the frontend returns), so neither the frontend nor the lifecycle can
rewrite `io.buildpacks.lifecycle.metadata` to match the actual layers. The frontend
records the ordered emitted diffIDs in a temp label
`io.buildpacks.native.layer-order`; PACK performs the rewrite host-side after push
(pull config+manifest — no layer egress — map emitted→actual diffIDs, rewrite the
label, drop the temp label, re-push). This makes buildpack-layer patching SUPPORTED
and fixes the analyzer's previous-image restore on rebuilds. Details in the pack
spec.

## THE EMIT CONTRACT (source of truth; pack consumes this)

Emit-mode writes to an output directory (e.g. `-emit-export-plan <dir>`). The
BuildKit-native recorder writes its files under a `buildkit/` subdirectory so other
recorders (buildah/podman) can be added later without collision (see "Decision:
emit format is recorder-namespaced"):

```
<emit-dir>/buildkit/plan.json
<emit-dir>/buildkit/config.json
```

### `plan.json`
```jsonc
{
  "schema": "buildkit-native-export/v1",
  "runImage": {
    "reference": "<run-image ref or digest>",   // for lifecycle-metadata + rebase
    "topLayer": "sha256:<diffID>"                // rebase boundary
  },
  "layers": [                                     // ORDERED, bottom to top
    {
      "id": "<buildpack-id:layer or launcher/app/sbom>",
      "reused": false,                            // true => reference run-image layer by digest
      "diffID": "sha256:<diffID>",
      "tar": "layers/.../<layer>.tar",            // path (relative to the emit/layers root) to the ACTUAL tar; present only when reused=false
      "history": { "createdBy": "...", "author": "...", "comment": "..." }
    },
    {
      "id": "<run-image base layer>",
      "reused": true,
      "diffID": "sha256:<diffID>"                 // reference the run image's original blob by this digest
      // no "tar" — caller references the run image
    }
  ]
}
```

### `config.json`
```jsonc
{
  "schema": "buildkit-native-export/v1",
  "entrypoint": ["/cnb/process/web"],             // launcher / default process
  "cmd": [],                                      // intentionally empty (as today)
  "workingDir": "/workspace",                     // app dir
  "env": {
    "CNB_LAYERS_DIR": "/layers",
    "CNB_APP_DIR": "/workspace",
    "CNB_PLATFORM_API": "0.12",
    "PATH": "...",
    "CNB_DEPRECATION_MODE": "quiet"
    // ... exactly what the exporter sets today
  },
  "labels": {
    "io.buildpacks.lifecycle.metadata": "<serialized LayersMetadata JSON>",
    "io.buildpacks.build.metadata": "<serialized BuildMetadata JSON>",
    "io.buildpacks.project.metadata": "<serialized ProjectMetadata JSON>",
    "io.buildpacks.exec-env": "<when applicable>"
    // ... plus any buildpack-provided labels
  }
}
```

### Layer tars
For each `reused: false` entry, the `tar` path points at the ACTUAL layer tar the
lifecycle built (these already exist under `/layers` as part of normal export layer
creation). The caller adds them to the image by the emitted `diffID` (no re-tar), so
layer digests match a normal export exactly. Reused entries carry only a `diffID`;
the caller references the run image's original blob by that digest.

Contract stability: `schema` is versioned (`v1`). Any change to field names,
ordering semantics, or tar referencing MUST bump the schema and be reflected in the
pack-side spec.

### Transport model (JSON is the wire format; structs are the schema)

Emit-mode runs INSIDE BuildKit (a RUN step) and writes these JSON files to the build
filesystem. The consumer (pack) runs on the HOST, in a separate process. So the data
crosses the BuildKit→host boundary as SERIALIZED JSON — that is the transport,
dictated by the process boundary, not a design preference.

The consumer IMPORTS these `emit` Go types (`Plan`/`LayerOp`/`ImageConfig`) and
unmarshals the JSON into them — it does NOT re-declare the schema. The structs are
the schema; the JSON is the transport. Pack already links the lifecycle as a library
(e.g. `phase.Rebaser`), so importing `phase/emit` adds no new dependency.

Crucially, only the small `plan.json`/`config.json` METADATA crosses to the host.
The layer tar DATA that `plan.json` references stays INSIDE BuildKit; the consumer's
assembly references those layers within BuildKit by their emitted diffID. This is
what preserves the no-egress property that motivates Option C.

## Why this preserves rebase / parity

- New layers are the lifecycle's own tars, added by their real diffIDs → identical
  layer digests to a normal export.
- Reused run-image layers are referenced by digest → the run image's original
  blobs, same rebase boundary.
- The `io.buildpacks.lifecycle.metadata` label is the SAME serialized
  `files.LayersMetadata` the exporter computes today → rebase metadata parity.

## Risks / open questions

- **Refactor size**: cleanly extracting a "sink" from the exporter's interleaved
  layer-create + image-mutate flow is the main effort. Mitigation: the
  recording-`imgutil.Image` variant reuses existing code with minimal change.
- **Determinism of tars**: the emitted tars must be the exact ones whose diffIDs are
  emitted; ensure emit-mode does not re-tar. (The lifecycle already produces these
  tars during normal export; emit-mode should reference them, not recreate them.)
- **History fidelity**: ensure every `AddLayer`/`ReuseLayer` records the same
  `v1.History` the exporter sets today.
- **Env/entrypoint completeness**: capture EVERY `SetEnv`/`SetEntrypoint`/`SetCmd`/
  `SetWorkingDir` the exporter performs (see `setLabels`/`launcherConfig` and the
  env-setting block in `Export`).
- **Platform API differences**: the exporter's behavior varies by Platform API
  (e.g. run-image metadata for >= 0.12); emit-mode must match the same API-gated
  behavior.

## Verification status (for the RFC — what was tested and how)

The buildkit-native path was validated locally against the real `pack` binary +
patched lifecycle image, publishing to a local registry. Confirmed working:

- **Cold build / rebuild / rebase** across sample apps (go/mod, python/poetry,
  nodejs/npm, java/maven, java/java-node), single- and multi-arch (amd64+arm64):
  finalized images carry a valid `io.buildpacks.lifecycle.metadata`, per-layer SHAs
  are actual produced diffIDs, and rebuild/rebase behave correctly.
- **Remote BuildKit registry cache** (`--buildkit-cache-to` / `--buildkit-cache-from
  type=registry`): a fresh (pruned) builder imports the exported cache.
- **Custom run image** (non-default `--run-image`) and **rebase onto a different
  run image**: `runImage.reference`/`topLayer` are authored correctly; app/buildpack
  layers are preserved across rebase (only the run base changes).

### App slices — verified at the seams (NOT end-to-end), by design

App slices (`[[slices]]`) come from a BUILDPACK's `launch.toml`, collected by the
builder and consumed by the exporter's `addAppLayers` → `SliceLayers`. The default
paketo buildpacks used in the sample matrix do NOT emit app slices (e.g. the npm
buildpack puts `node_modules` in a BUILDPACK layer, not an app slice; a build shows
"Added 1/1 app layer(s)"). So there is no off-the-shelf sample that exercises the
multi-app-slice path with the current builder.

Slice correctness is therefore verified by UNIT TESTS at the two seams that the
buildkit-native path introduces, which together cover producer→consumer:

- **Producer seam (lifecycle):** `layers/slices_test.go` asserts `SliceLayers`
  records each slice layer's `Source` — `Dir` = app dir, `Include` = the EXACT
  relative paths that slice contains (files land in exactly one layer), plus the
  normalized uid/gid. This is the source-ref that emit-mode surfaces.
- **Consumer seam (pack):** `internal/build/multiplatform/native_buildfunc_slices_internal_test.go`
  asserts `copyFromSource` turns a slice `LayerOp` (with `Source.Include`) into an
  `llb.Copy` whose `IncludePatterns` equal that Include list, with
  `dirCopyContents=true` and chown to the slice uid/gid.

FOLLOW-UP (tracked): a full end-to-end slice build would require a custom buildpack
that writes `[[slices]]` to its `launch.toml`. That is the only remaining slice
coverage gap and is called out here for the RFC. The two seam tests exercise the
actual fork code that implements slicing for the buildkit-native path.

## Testing (local, MVP)

Run emit-mode against a real `/layers` produced by a build and compare the emitted
plan + config against a normal export of the same inputs (layer order, diffIDs,
history, labels, env, entrypoint). No `PACK_TEST_*` env-var-gated registry tests;
keep it local/MVP. End-to-end validation (assembling the image in BuildKit and
checking it is runnable + rebase-parity) happens on the pack side.

## Relationship to the pack-side spec

- Pack (`jericop/cnb-pack@buildkit-native-export`) consumes `plan.json` +
  `config.json` + the layer tars to build the BuildKit assembly (`FROM run-image` +
  add layers + apply config) and does native multi-arch export.
- This spec is the PRODUCER of the contract; the pack spec is the CONSUMER. Keep the
  `schema` version in sync across both.
