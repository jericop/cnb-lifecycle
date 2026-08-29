# Spike: can we eliminate the post-push metadata-SHA rewrite?

**Question:** Is there a way for the lifecycle + BuildKit to cooperate so the pushed
image's layer diffIDs already match the lifecycle's emitted
`io.buildpacks.lifecycle.metadata` per-layer SHAs — removing the need for pack's
host-side, post-push metadata rewrite?

**Verdict:** The rewrite is NOT fundamental to buildpacks. It is an artifact of HOW
the current frontend assembles layers (`RUN tar -xf` → BuildKit re-snapshots →
new diffIDs). It CAN be eliminated, but not via a "hand BuildKit pre-built blobs
through the gateway result" trick — the gateway API does not allow that. The viable
path is to make the layers in the LLB result BE the lifecycle's exact layers, which
means importing a lifecycle-produced OCI layout via `llb.OCILayout` (Option 2).
There is a real tradeoff (in-build disk materialization + run image readable
in-build), which is exactly why the MVP chose the rewrite.

Investigated against `github.com/moby/buildkit v0.32.2` (the version the lifecycle +
pack build against).

## Root cause of the mismatch (confirmed)

The frontend assembles by extracting each emitted layer tar with `RUN tar -xf` and
letting BuildKit snapshot the resulting filesystem. BuildKit computes ITS OWN diffID
for that snapshot, which differs from the lifecycle's emitted diffID because the
re-created tar is not byte-identical (tar header ordering, timestamps, whiteout
encoding, xattrs, etc.). The lifecycle already wrote the CORRECT SHAs into
`io.buildpacks.lifecycle.metadata`, but the produced blobs differ, so the label no
longer matches the image. Pack fixes the label afterward.

Normal buildpacks builds have none of this: the lifecycle exporter writes the exact
tars it computed SHAs for and pushes them. The "fuss" comes entirely from inserting
BuildKit's content store between the lifecycle and the registry AND re-assembling
layers inside it.

## Why "add-by-blob via the gateway result" (Option 1) is NOT possible

Verified in the BuildKit source:

- **Gateway result API** (`frontend/gateway/client/client.go`): a frontend returns a
  `*client.Result` whose refs are `Reference`s. `Reference` exposes only
  `ToState / Evaluate / ReadFile / StatFile / ReadDir`. There is NO API to attach
  pre-built layer blobs/descriptors as the output image's layers. A frontend can
  only return a solved LLB state; BuildKit snapshots + diffs that state to make
  layers.
- **Image exporter** (`exporter/containerimage/writer.go`): the final
  `rootfs.DiffIDs` are built in `patchImageConfig` PURELY from the exported layer
  descriptors of the result ref (`desc.Annotations[LabelUncompressed]`), via
  `ic.exportLayers(ctx, ..., ref)`. The exporter derives diffIDs from the ref's
  actual blob chain — there is no "use these diffIDs / these blobs" override.
- **`ExporterImageBaseConfigKey` ("containerimage.base.config")** does NOT substitute
  base layers. It is consulted only in `rewriteRemoteWithEpoch` / `patchImageConfig`
  to RETAIN timestamps+history for the portion of the result that matches the base
  image (epoch/reproducibility). It does not make BuildKit reuse the base's blobs in
  place of produced ones.

Conclusion: BuildKit ALWAYS derives the output diffIDs from the result ref's real
layer chain. The only way produced diffIDs == emitted diffIDs is if the result ref's
layers ARE literally the lifecycle's layers (same blobs) — i.e. BuildKit ingested
those exact layers as cache refs.

## The viable path: Option 2 — import a lifecycle-produced OCI layout (`llb.OCILayout`)

`llb.OCILayout(ref, OCIStore(session, store))` (`client/llb/source.go`) imports an
existing image from an OCI-layout store as a source op. Its layers enter BuildKit's
content store as cache refs with their ORIGINAL diffIDs preserved (this is the same
machinery BuildKit uses to mount/reuse base-image layers everywhere). If the
imported layout is the FINISHED CNB image the lifecycle assembled, then:

- the result ref's layers are the lifecycle's exact layers → produced diffIDs ==
  emitted diffIDs, and
- the lifecycle-metadata label (also produced by the lifecycle) is correct
  as-written → NO rewrite, and buildpack-layer patching works by construction.

Two ways to produce that layout in-build:

- **2a — lifecycle exports a full OCI layout in-build** (the exporter's existing
  `-layout` mode). The frontend/pack imports it via `llb.OCILayout` and returns it as
  the result (single-arch) or composes the multi-arch index from per-arch layouts.
  This is ESSENTIALLY WHAT THE EXISTING LLB `oci-layout` BACKEND ALREADY DOES today —
  and it needs NO post-push rewrite. It was set aside for the "native" backend only
  to avoid (a) needing the run image readable IN-BUILD (analyzer `-pull-run-image`
  pulls it into the layout — disk I/O) and (b) a disk-materialized two-step flow
  rather than pure content-store.

- **2b — keep emit-mode but import emitted tars as content-store blobs** (rather than
  `tar -xf`). This would require the frontend to build the image from imported blobs
  preserving diffIDs. But there is no clean LLB primitive to "make a layer whose blob
  is exactly this tar" other than importing a full image/layout (2a). Attempts to
  synthesize a layer from a tar go back through snapshot+diff → new diffID. So 2b
  collapses into 2a in practice.

## Does this mean "embed the lifecycle in the frontend" (the inversion you asked about)?

Yes — the clean solutions invert today's relationship. Today the frontend RE-ASSEMBLES
the image from a plan. The rewrite exists ONLY because of that re-assembly. If instead
the LIFECYCLE produces the FINISHED image (layout) and the frontend/pack merely
TRANSPORTS it (import layout → return/compose → push), there is nothing to rewrite.
This is Option 2a and is also Option B (lifecycle-as-library-hybrid) in spirit.

## Tradeoffs

| | Current (emit + re-extract + rewrite) | Option 2a (lifecycle layout + OCILayout import) |
|---|---|---|
| Post-push mutation | REQUIRED | NONE |
| diffID parity with other modes | differs | byte-identical |
| Layer data egress to host | none (stays in content store) | none for push, but layout is MATERIALIZED ON DISK in-build |
| Run image handling | `llb.Image` base, never pulled to disk | analyzer `-pull-run-image` pulls run image INTO the layout (disk I/O in-build) |
| Flow | one solve, pure content store | two-step: lifecycle writes layout → import → push |
| Maturity | working + validated (MVP) | proven mechanics (existing oci-layout backend), not yet wired to a "native" single-image push |
| Self-healing / durable label machinery | needed (tasks 1,3,9,11) | NOT needed — parity by construction |

## Recommendation

Two coherent end-states; pick based on which property matters more:

1. **Eliminate the rewrite (recommended if the post-push mutation is unacceptable):**
   adopt Option 2a. Have the lifecycle export a full OCI layout in-build; the
   frontend imports it via `llb.OCILayout` and returns/composes it; BuildKit pushes
   verbatim. Byte-identical parity, no rewrite, no durable-label/self-healing
   machinery. Cost: in-build disk materialization + run image pulled into the layout.
   Much of this already exists in the LLB `oci-layout` backend — the work is wiring
   it to the multi-arch native single-index push and dropping the re-extract path.

2. **Keep the current design (recommended if avoiding in-build disk + keeping pure
   content-store is the priority):** proceed with tasks 1–7, treating the rewrite as
   a deliberate, well-documented, tag-atomic + idempotent + self-healing step.

The rewrite is safe (tag-atomic, non-destructive, idempotent) but it IS an extra
post-push registry mutation. Option 2a removes it entirely at the cost of in-build
disk + pulling the run image in-build. If the goal is "buildpacks just builds and
pushes the image without post-hoc surgery," Option 2a is the way, and it does mean
letting the lifecycle produce the finished image rather than the frontend
re-assembling it.

## Impact on the remaining tasks if Option 2a is adopted

- DROP: pack task 11 (durable layer-order label), pack task 9 (self-healing fix +
  flag), and the whole `metadata_rewrite.go` path. The `io.buildpacks.native.layer-order`
  label goes away.
- KEEP/CHANGE: frontend task 10 (build-image tar) becomes moot IF layers come from the
  imported layout rather than `tar -xf` (no per-layer extraction at all).
- KEEP: rebuild/rebase validation, repeated-cycle tests, large-app timing comparison.
- NEW: wire lifecycle `-layout` output → `llb.OCILayout` import → native multi-arch
  index push (leveraging the existing oci-layout backend's assembly), for the
  buildkit-native backend.


---

# Spike part 2: BuildKit-driven approach — lifecycle generates metadata FROM BuildKit's output

Two follow-up questions:

## Q1: Do recomputed diffIDs hurt BuildKit's rebuild CACHE?

**No.** Verified against the LLB cache model + the frontend graph:

- BuildKit caches by the LLB OPERATION GRAPH (each vertex's inputs + args + mounts +
  the persistent cache mount), NOT by output layer diffIDs. The recomputed diffIDs
  are deterministic OUTPUTS of cached vertices, never cache KEYS.
- On a rebuild with unchanged app source + builder: the analyzer/detector/restorer/
  builder/exporter RUN vertices and the `tar -xf` assembly RUNs are cache HITS
  regardless of the eventual diffIDs.
- Where recomputed diffIDs DO matter: (a) the CNB analyzer's previous-image restore
  (keys on metadata SHAs — this is what the rewrite fixes), and (b) REGISTRY layer
  dedup (a different diffID = a distinct stored blob even if content is equivalent).
  Neither is a BuildKit BUILD-cache concern.

So "recomputed diffIDs" never hurt BuildKit caching; they only affect CNB-level
previous-image restore (fixed by making metadata consistent) and cross-image blob
dedup at the registry.

## Q2: Why can't the lifecycle GENERATE the metadata from what BuildKit built (instead of pack rewriting it)?

This is the right instinct: work WITH BuildKit. The lifecycle already knows how to
compute `io.buildpacks.lifecycle.metadata` (+ SBOM/history/reuse). Today it computes
it from ITS OWN emitted diffIDs (emit-mode), then BuildKit produces DIFFERENT diffIDs,
so pack patches the label. Instead, feed the lifecycle the diffIDs BuildKit actually
produced and let it emit CORRECT metadata — no patching.

The whole thing is a SEQUENCING problem: BuildKit assigns diffIDs at EXPORT time,
after the frontend's `Build` returns. So "generate metadata from produced diffIDs"
needs a stage that runs AFTER the layers exist but still writes into the FINAL image
config.

### Hard constraint (verified, v0.32.2)

The gateway `Reference` interface exposes only `ToState / Evaluate / ReadFile /
StatFile / ReadDir`. There is NO way — even INSIDE the build, after solving the
assembled state — for the frontend to ask BuildKit "what layer diffIDs did you assign
to this ref?" Combined with part-1's finding (exporter derives diffIDs from the ref's
real chain; no blob injection), this means: the produced diffIDs are not observable
to the frontend at all. They first become knowable by reading the PUSHED/EXPORTED
image (config), which is what pack does today.

### So the options for "lifecycle generates the metadata" are:

- **2c-i — Two-pass in one build (compute layers, read them back, regenerate config).**
  Pass 1: assemble the app layers as LLB (as today). The frontend then needs the
  produced diffIDs to run a lifecycle "finalize-metadata" step. But the frontend
  can't read produced diffIDs from a ref (constraint above). The only in-build way to
  learn a layer's diffID is to COMPUTE it from the layer's filesystem (sha256 of the
  uncompressed tar) — which means re-tarring in a RUN, reproducing the same
  non-determinism, so the "computed" diffID still won't match what BuildKit's
  exporter assigns unless the exporter uses the exact same tar. This does NOT
  reliably work. REJECTED.

- **2c-ii — Make the layers deterministic so lifecycle diffID == BuildKit diffID.**
  If the assembly produced BYTE-IDENTICAL tars to what the lifecycle emitted, the
  diffIDs would match and the emitted metadata would already be correct — no rewrite.
  BuildKit's exporter tars snapshots with its own serializer, so matching it exactly
  is brittle/unsupported. REJECTED (fighting BuildKit).

- **2c-iii — A post-export FIX step that the LIFECYCLE (not pack) owns, driven BY
  BuildKit.** Rather than pack doing a host-side rewrite, add a final LLB stage that
  runs a lifecycle subcommand which: takes the produced image (config+manifest — via
  a small BuildKit primitive or a nested build that imports the just-exported image),
  reads the actual diffIDs, and REGENERATES `io.buildpacks.lifecycle.metadata` +
  history to match, emitting a corrected config. This is STILL a
  build-metadata-from-produced-diffIDs step — the difference from today is it lives in
  the lifecycle and runs inside the BuildKit graph, not as a pack post-push mutation.
  The blocker is the same sequencing wall: within one `Build`, the produced image
  doesn't exist yet to import. It works only as a SECOND build (export image #1, then
  a second frontend pass imports image #1 via `llb.OCILayout`, regenerates the config,
  re-exports). That is two solves + an intermediate image — arguably worse than the
  cheap host-side config re-push.

- **2c-iv (RECOMMENDED for "no mutation, work with BuildKit") — Lifecycle assembles
  the finished image; frontend imports it via `llb.OCILayout`; BuildKit pushes it.**
  This is Option 2a from part 1, viewed through your framing: the lifecycle GENERATES
  the metadata AND the layers together (its normal export to a layout), so metadata
  and diffIDs are consistent BY CONSTRUCTION. The frontend then does NOT re-assemble
  (no `tar -xf`, no recomputed diffIDs) — it imports the lifecycle's layout, whose
  layers keep their original diffIDs, and returns it. BuildKit pushes verbatim. No
  rewrite, ever. The cost is the in-build disk materialization + `-pull-run-image`.

### The real conclusion

"Have the lifecycle generate metadata from what BuildKit built" is appealing, but the
gateway API makes the produced diffIDs UNOBSERVABLE until an image is exported. So you
can either:

- accept a reconcile-after-produce step (today's host-side rewrite, or the same logic
  moved into a lifecycle subcommand / second pass — 2c-iii, which is heavier), OR
- avoid the divergence entirely by NOT re-assembling: let the lifecycle produce the
  finished layers+metadata together and have BuildKit adopt them via `llb.OCILayout`
  (2c-iv = 2a).

There is NO in-band way for the lifecycle to emit metadata matching BuildKit's
produced diffIDs within a single build, because those diffIDs don't exist until export
and aren't observable to the frontend. So the honest choice is between:
(1) keep the efficient content-store hybrid + a small reconcile step (current), or
(2) let the lifecycle own the finished image + `llb.OCILayout` import (no reconcile,
but disk materialization + run image pulled in-build).

### Is there a way to make 2a NOT wasteful?

Your concern with 2a is the copying/materializing. Mitigations worth prototyping:
- **Shared content store, not a disk layout round-trip.** Instead of the lifecycle
  writing an OCI LAYOUT to a filesystem and BuildKit re-importing it, have the
  lifecycle write layers directly into a containerd/BuildKit content store that the
  build shares, and reference them by digest. `llb.OCILayoutBlob` + an OCI store
  session (`OCIStore(sessionID, storeID)`) can import from a store rather than a
  tarball tree, avoiding a second full copy. Needs verification that the lifecycle
  can write to (or hand off) that store.
- **Run image NOT duplicated.** The run image layers are already content-addressed;
  if the lifecycle references them by digest (rather than copying into the layout),
  only the NEW app/launcher/config/sbom layers are materialized. The lifecycle's
  layout export can reuse-by-digest for base layers (it already reuses run-image
  layers in normal export).
- If those hold, 2a's "waste" shrinks to: materialize only the NEW layers once into a
  shared store — which BuildKit would have snapshotted anyway. That could be
  competitive with the current re-extract path, WITHOUT the rewrite.

RECOMMENDATION: prototype 2a with a SHARED CONTENT STORE (not a disk layout
round-trip) and run-image-by-digest, and measure it against the current path. If the
extra materialization is only the new layers into a store BuildKit shares, we get
"no mutation + no meaningful waste + work-with-BuildKit" — the end-state you want. If
it forces a full disk layout + run-image copy, the current efficient-hybrid +
reconcile step remains the better tradeoff.


---

# Spike part 3: Option A (BuildKit builds+pushes; lifecycle FINALIZES) — surfacing the plan

**Decision: Option A is the chosen direction.** BuildKit builds and pushes a normal
(runnable but not-yet-CNB-compliant) image; then a LIFECYCLE library call (consumed
by pack the way pack consumes `phase.Rebaser`) interrogates the pushed image and
authors correct CNB metadata from the ACTUAL produced layers. No frontend, no
re-extraction, no stale-label-then-patch.

## Why the finalize step needs surfaced info, and what form it takes

Finalize needs: (1) the pushed image's produced per-layer diffIDs — read directly
from the pushed image config, always available; and (2) the PLAN — layer order,
new-vs-reused split, semantic identity (app/sbom/launcher/config/process-types/
buildpack layers), intended diffIDs, history, and the run-image rebase boundary.
Only (2) must be surfaced from the build.

### Verified: the final label CANNOT supply (2) on its own

`platform/files/analyzed.go` `LayersMetadata` records per-layer SHAs grouped by role
(App[]/BOM/Buildpacks[].Layers/Config/Launcher/ProcessTypes) + RunImage boundary, but
NOT each layer's POSITION in the image stack and not a clean position→entry mapping
with reused layers interleaved. So `io.buildpacks.lifecycle.metadata` alone is
insufficient for finalize to map produced diffIDs → entries. A dedicated surfaced
artifact is warranted.

### Verified: emit-mode ALREADY computes the ordering (richer than the final label)

`phase/emit` `Plan.Layers` is an ORDERED slice; each `LayerOp` carries `ID`, `Reused`
(new vs run-image base), `DiffID` (intended), `TarPath`, and `History`, plus
`Plan.RunImage{Reference, TopLayer}`. This is an ordered superset of what finalize
needs. The ordering is already produced — it just needs to be surfaced onto the image.

## Decision: surface (2) as a DISTINCT build-phase LABEL — never a layer

- **Label, not a layer.** A layer would add a real runtime blob, change the layer
  count, need its own diffID, and pollute the app image with build-only data. A
  config label is free (in the image config JSON finalize rewrites anyway), invisible
  at runtime, and removable.
- **A DISTINCT label, NOT the final `io.buildpacks.lifecycle.metadata`.** The build
  phase MUST NOT pre-write the final label with stale SHAs (the MVP's mistake). It
  writes a separate, clearly-build-phase label whose name signals "produced by the
  BuildKit phase, only partially valid until finalized." Proposed:
  `io.buildpacks.native.build-plan` (or a `.partial` suffix, e.g.
  `io.buildpacks.native.build-plan.partial`). The FINAL
  `io.buildpacks.lifecycle.metadata` only ever appears in its correct form, authored
  by finalize.
- **Contents = the serialized ordered emit `Plan`.** Since emit-mode already produces
  it, the build-phase label is essentially the `emit.Plan` (ordered layers with
  id/reused/diffID/history + runImage boundary). Finalize maps the plan's ordered
  NEW-layer intended diffIDs positionally to the image's produced diffIDs, then
  authors the real `io.buildpacks.lifecycle.metadata` (+ history/SBOM associations).
  Any future finalize input is just another field on this struct — no new label, no
  layer.
- **Lifecycle of the build-phase label:** finalize may DROP it after authoring the
  final metadata, or KEEP it (durable) to enable self-healing re-validation later.
  This is now a LABEL-only decision (never a layer). Keeping it is cheap and enables
  self-healing; that is the leaning.

## Resulting Option A shape

1. **Build (BuildKit):** run lifecycle phases; the exporter produces a normal image
   AND writes the `io.buildpacks.native.build-plan` label (the ordered plan). Push
   natively (multi-arch, no intermediate tags — mechanism TBD, see open question).
   No frontend. No `io.buildpacks.lifecycle.metadata` pre-written (or if the normal
   exporter writes one, finalize overwrites it — but the SOURCE OF TRUTH for finalize
   is the build-plan label, not the stale final label).
2. **Finalize (lifecycle library, pack calls it):** read the pushed image's produced
   diffIDs + the build-plan label → author correct `io.buildpacks.lifecycle.metadata`
   → re-push config (tag-atomic). Now rebuildable/rebaseable.
3. **Self-healing (last):** the same finalize call as a check+fix on any image, using
   the retained build-plan label.

## Where the finalize logic lives: the LIFECYCLE, as a library

Per the decision: the rewrite/finalize logic moves OUT of pack's
`metadata_rewrite.go` and INTO the lifecycle as a library API (+ a subcommand
wrapper, e.g. `finalize`/`stamp`). Pack imports and calls it the way it imports
`phase.Rebaser` for rebase. Rationale: authoring CNB metadata (schema, layer
semantics, SBOM, history) is lifecycle domain knowledge; keeping it in the lifecycle
avoids drift and lets pack + the standalone/self-healing tool share ONE
implementation. `metadata_rewrite.go` becomes a thin caller of the lifecycle API (or
is removed once the lifecycle API lands).

## Do we still need the frontend? NO.

The frontend existed ONLY to set image config/labels during the build via the gateway
result API. Option A does not set the final CNB metadata during the build, so that
reason is gone. The build phase is "run phases, assemble FROM run-image, push a
normal image + one build-plan label" — which the existing LLB backend already does
(minus the label). So Option A RETIRES `buildkit/cnbfrontend` + `cmd/cnb-frontend`
from the target design. Emit-mode SURVIVES in reduced form: it produces the ordered
plan that becomes the build-plan label (no tar persistence, no assembly driving).

## Open question to resolve when writing the plan

- **Build-phase push: normal registry export (simplest, but reintroduces per-arch
  intermediate tags) vs the existing oci-layout no-intermediate-tags path.** Option A
  wants "BuildKit builds+pushes a normal image"; preserving the no-intermediate-tags
  property may still want the oci-layout assembly machinery (or host-side manifest
  assembly via `remote.WriteIndex`, already implemented). Decide during spec rework:
  can we get a normal image push with no intermediate tags without the oci-layout
  disk round-trip? (Host-side manifest-list assembly from per-arch pushes-by-digest
  is the likely answer, reusing `PushPerArchLayoutsAsManifestList`.)


---

# Spike part 4 (implementation decision): A1 (finalize) vs A2 (oci-layout import)

While wiring Option A, a fork surfaced in HOW the build phase produces the image.
Both are "Option A" (BuildKit builds+pushes; no frontend), but they differ in
whether finalize is needed:

- **A1 — finalize path (CHOSEN).** BuildKit assembles the image via LLB such that it
  re-snapshots the new layers (diffIDs are BuildKit's, differing from the exporter's
  intended ones). The build attaches the `io.buildpacks.buildkit.native.build-metadata`
  label (plan + emitted labels). Pack pushes natively; then FINALIZE authors the real
  `io.buildpacks.lifecycle.metadata` from produced diffIDs. NO disk materialization,
  NO `-pull-run-image`, run image is an `llb.Image` base. This is the design the user
  chose; it delivers the "work with BuildKit, no disk round-trip" benefit at the cost
  of the finalize config re-push (metadata only, no layers).

- **A2 — oci-layout import (PROVEN FALLBACK, no finalize).** The lifecycle exporter
  runs in `-layout` mode and writes a COMPLETE OCI layout (correct metadata authored
  against its own diffIDs); pack imports it via `llb.OCILayout` (diffIDs preserved) and
  pushes. Metadata already matches → NO finalize, NO build-metadata label. This is
  essentially the EXISTING `buildkit-llb --buildkit-export-mode=oci-layout` backend.
  Cost: in-build OCI-layout disk materialization + analyzer `-pull-run-image` pulls the
  run image into the layout. This is the thing Option A was meant to avoid, but it is
  already implemented and needs no rewrite/finalize.

**Decision: implement A1** (finalize). Rationale: it is the design the user selected,
it avoids the disk round-trip + run-image pull that make A2 wasteful, and it exercises
the finalize library that also underpins the future self-healing tool. A2 remains a
proven fallback (the oci-layout backend) if A1's build assembly proves problematic.

## A1 build-assembly sub-decision: how BuildKit gets the layers + the label without a frontend

A1 needs (a) the app layers assembled `FROM run-image` in LLB and (b) the
`build-metadata` label on the output image config — WITHOUT a gateway frontend
(retired) and knowing plain `client.Solve` cannot set image config/labels.

Candidate mechanisms (to resolve during implementation + local testing):

1. **Exporter writes the image AND the build-metadata label in-build; pack imports
   via `llb.OCILayout` and pushes (like A2 plumbing) — but WITHOUT relying on the
   layout's diffIDs matching, i.e. still finalize.** This collapses toward A2's
   plumbing. If we import a layout, diffIDs ARE preserved and finalize is a no-op —
   so this is really A2. NOT A1.

2. **A minimal gateway BuildFunc (NOT the full cnbfrontend) that only sets the
   config/labels on the assembled state.** The retired frontend's REAL job was
   exactly this (set config via the gateway result). A1 without ANY gateway result
   cannot set the `build-metadata` label. So A1 needs a SMALL gateway result step to
   attach the label — much smaller than cnbfrontend (no per-layer tar extraction; the
   layers come straight from the exporter's produced `/layers` assembled onto the run
   image; just set the config label via `AddMeta(ExporterImageConfigKey)`).

   RESOLUTION: A1 keeps a MINIMAL gateway BuildFunc whose only jobs are (i) assemble
   `FROM run-image` + the exporter-produced layers (no re-tar gymnastics beyond what
   the exporter already did) and (ii) set the config incl the `build-metadata` label
   via the gateway result. This is a big simplification of cnbfrontend (drop the
   per-layer `tar -xf`, drop emit tar persistence, drop the layer-order label — just
   assemble + set the one label). Finalize then authors the final metadata.

   NUANCE: even "assemble FROM run-image + exporter layers" in LLB re-snapshots →
   BuildKit diffIDs → finalize needed (that is A1 by definition). Confirmed consistent.

3. **Pack sets the label host-side BEFORE finalize (extra config re-push).** BuildKit
   pushes the image without the label; pack pulls config, adds the build-metadata
   label, re-pushes; then finalize. This avoids ANY gateway result but adds a second
   config re-push. Since finalize ALREADY does a config re-push, folding the label
   into a single host-side step is possible — but pack would then need the plan from
   somewhere (it read the emit files). Viable but adds a host-side pre-step and needs
   the emit files transported out (egress of small metadata, acceptable).

DECISION for the MVP: pursue mechanism (2) — a MINIMAL gateway BuildFunc that
assembles and sets ONLY the build-metadata label — because it keeps the plan+labels
authored by the lifecycle/emit and carried on the image in one build, with finalize
authoring the final metadata. If (2) proves fiddly in local testing, fall back to
(3) (pack attaches the label host-side from the emit files, then finalizes) — and if
BOTH are problematic, fall back to A2 (oci-layout import, no finalize), which is
already proven.

This keeps the finalize library (already written) as the correctness core regardless
of which build-assembly mechanism wins, since finalize only depends on the pushed
image + the build-metadata label.
