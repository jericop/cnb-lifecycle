# Design: Lifecycle BuildKit-Native Export Emit-Mode

## Overview

Add an additive, opt-in export "emit-mode" to the lifecycle. Instead of the
exporter assembling and pushing an `imgutil.Image`, emit-mode produces the
buildpacks-specific outputs that a caller (pack) turns into a BuildKit-native image
assembly:

- an ordered **Layer Plan** (`plan.json`),
- the final **Image Config** (`config.json`), and
- for new layers, references to the ACTUAL layer tars the lifecycle built (under
  `/layers`), with precomputed diffIDs.

This spec is the LIFECYCLE half of Option C. The pack half
(`jericop/cnb-pack@buildkit-native-export`) consumes this output. The emit contract
below is the interface between them.

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

## AS-BUILT (MVP, validated) — emit-mode + frontend, and where the SHA rewrite lives

The MVP is implemented and validated end-to-end. Two things landed beyond the
emit-mode recorder described above:

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
