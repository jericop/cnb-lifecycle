// Package emit implements the lifecycle side of the "BuildKit-native export"
// (Option C) effort: an additive, opt-in export path that RECORDS the operations
// the exporter would perform on an image and EMITS them as a small contract
// (plan.json + config.json) instead of assembling and pushing an image.
//
// The pack-side consumer (jericop/cnb-pack@buildkit-native-export) reads this
// contract to assemble the final CNB app image natively in BuildKit
// (FROM <run-image> + add the emitted layers + apply the emitted config).
//
// The seam is the imgutil.Image interface: phase.Exporter.Export only ever calls
// interface methods on opts.WorkingImage, so a RecordingImage that implements
// imgutil.Image (by embedding a run-image-backed image for reads and overriding
// the mutators) captures everything the exporter does with NO change to
// phase/exporter.go. Its Save() writes the contract files.
//
// See the design doc:
// jericop/cnb-lifecycle/.kiro/specs/buildkit-native-export/design.md
package emit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/buildpacks/imgutil"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/pkg/errors"
)

// Schema is the emit-contract version. Bump on any breaking shape change and
// reflect the change in the pack-side spec.
const Schema = "buildkit-native-export/v1"

// RecorderDir is the subdirectory (relative to the emit output dir) under which
// the BuildKit-native recorder writes its files. Namespacing by recorder keeps
// the door open for future recorders (buildah/podman) without collision.
const RecorderDir = "buildkit"

// PlanFileName / ConfigFileName are the contract file names.
const (
	PlanFileName   = "plan.json"
	ConfigFileName = "config.json"
	// BuildMetadataFileName is the file emit-mode writes carrying the serialized
	// BuildMetadata (plan + emitted config labels). For Option A (build-then-
	// finalize), the build phase sets this JSON as the BuildMetadataLabel on the
	// produced image, and finalize consumes it. It is the single artifact the
	// finalize step needs from the build (finalize reads produced diffIDs from the
	// image itself, not from any tar).
	BuildMetadataFileName = "build-metadata.json"
	// LayersSubdir is the subdirectory (under <outputDir>/<RecorderDir>/) where the
	// emitted layer tars are persisted so the consumer can read them after the
	// export step (the exporter's own temp artifacts dir is cleaned up).
	//
	// NOTE (Option A): finalize does NOT need these tars (it reads produced diffIDs
	// from the pushed image). Tar persistence is retained only for the SUPERSEDED
	// frontend re-assembly path and may be removed once that path is gone.
	LayersSubdir = "layers"
)

// LayerOp is one recorded layer operation, in the order the exporter emitted it.
type LayerOp struct {
	// ID identifies the layer. For the MVP this is derived from History.CreatedBy
	// (the exporter already encodes buildpack/layer identity there). See the
	// design doc "Follow-up items" for making this a first-class id.
	ID string `json:"id"`
	// Reused is true when this is a run-image/base layer the consumer must
	// reference by digest (no tar is emitted for it).
	Reused bool `json:"reused"`
	// DiffID is the layer's uncompressed digest (sha256:...). Always present.
	DiffID string `json:"diffID"`
	// TarPath is the path to the ACTUAL layer tar the lifecycle built. Present
	// only when Reused is false. It is recorded verbatim (never re-tarred), so the
	// emitted diffID matches a normal export exactly (rebase/diffID parity).
	TarPath string `json:"tar,omitempty"`
	// History is the exact v1.History the exporter recorded for this layer.
	History v1.History `json:"history"`
}

// Plan is the ordered layer plan (plan.json).
type Plan struct {
	Schema   string       `json:"schema"`
	RunImage PlanRunImage `json:"runImage"`
	Layers   []LayerOp    `json:"layers"`
}

// PlanRunImage carries the run-image rebase boundary, consistent with the
// io.buildpacks.lifecycle.metadata label the exporter computes.
type PlanRunImage struct {
	Reference string `json:"reference"`
	TopLayer  string `json:"topLayer"`
}

// ImageConfig is the accumulated container image config (config.json) the
// exporter would have set.
type ImageConfig struct {
	Schema     string            `json:"schema"`
	Entrypoint []string          `json:"entrypoint"`
	Cmd        []string          `json:"cmd"`
	WorkingDir string            `json:"workingDir"`
	Env        map[string]string `json:"env"`
	Labels     map[string]string `json:"labels"`
}

// RecordingImage implements imgutil.Image. It forwards READS to the embedded
// run-image-backed image (so TopLayer(), Env(), Labels(), etc. return real
// run-image values) and RECORDS WRITES into an in-memory plan + config. Save()
// emits the contract files instead of pushing.
type RecordingImage struct {
	// Image is the embedded run-image-backed imgutil.Image. By embedding it,
	// every getter and any method we do not override is satisfied for free. We
	// only override the mutators the exporter calls, plus Save.
	imgutil.Image

	// runImageRef is the run image reference the caller already knows (matches
	// what phase.Exporter puts in meta.RunImage.Reference).
	runImageRef string
	// outputDir is the emit output directory; the recorder writes into
	// <outputDir>/<RecorderDir>/.
	outputDir string

	// captured state (ordered)
	layers []LayerOp
	config ImageConfig
}

// assert RecordingImage satisfies the interface at compile time.
var _ imgutil.Image = (*RecordingImage)(nil)

// NewRecordingImage wraps a run-image-backed imgutil.Image so reads resolve
// against the run image, records writes, and (on Save) emits the contract to
// <outputDir>/<RecorderDir>/. runImageRef is the run image reference recorded in
// the plan's runImage.reference.
func NewRecordingImage(runImageBacked imgutil.Image, runImageRef, outputDir string) *RecordingImage {
	return &RecordingImage{
		Image:       runImageBacked,
		runImageRef: runImageRef,
		outputDir:   outputDir,
		config: ImageConfig{
			Schema: Schema,
			Cmd:    []string{},
			Env:    map[string]string{},
			Labels: map[string]string{},
		},
	}
}

// idFor derives a layer id from the recorded history's CreatedBy. The exporter
// encodes buildpack/layer identity there via layers.BuildpackLayerName etc.
// FOLLOW-UP (post-MVP): thread an explicit id through the exporter if this is not
// guaranteed to match the consumer's needs (see design doc).
func idFor(history v1.History) string {
	return history.CreatedBy
}

// --- WithEditableLayers: record instead of mutate ---

// AddLayerWithDiffIDAndHistory records a NEW layer by its already-built tar path
// and diffID (no re-tar).
func (r *RecordingImage) AddLayerWithDiffIDAndHistory(path, diffID string, history v1.History) error {
	r.layers = append(r.layers, LayerOp{
		ID:      idFor(history),
		Reused:  false,
		DiffID:  diffID,
		TarPath: path,
		History: history,
	})
	return nil
}

// ReuseLayerWithHistory records a REUSED run-image/base layer by digest.
func (r *RecordingImage) ReuseLayerWithHistory(diffID string, history v1.History) error {
	r.layers = append(r.layers, LayerOp{
		ID:      idFor(history),
		Reused:  true,
		DiffID:  diffID,
		History: history,
	})
	return nil
}

// AddLayer records a new layer with no known diffID/history. The exporter does
// not call this today, but we implement it for interface completeness by
// recording the path (the diffID will be empty).
func (r *RecordingImage) AddLayer(path string) error {
	r.layers = append(r.layers, LayerOp{Reused: false, TarPath: path})
	return nil
}

// AddLayerWithDiffID records a new layer with a diffID but no history.
func (r *RecordingImage) AddLayerWithDiffID(path, diffID string) error {
	r.layers = append(r.layers, LayerOp{Reused: false, DiffID: diffID, TarPath: path})
	return nil
}

// AddOrReuseLayerWithHistory records either an add or a reuse. The exporter uses
// the explicit Add/Reuse variants today; this records as a new layer to remain
// interface-complete.
func (r *RecordingImage) AddOrReuseLayerWithHistory(path, diffID string, history v1.History) error {
	return r.AddLayerWithDiffIDAndHistory(path, diffID, history)
}

// ReuseLayer records a reused layer by digest with no history.
func (r *RecordingImage) ReuseLayer(diffID string) error {
	r.layers = append(r.layers, LayerOp{Reused: true, DiffID: diffID})
	return nil
}

// --- WithEditableConfig: record instead of mutate ---

func (r *RecordingImage) SetEntrypoint(v ...string) error { r.config.Entrypoint = v; return nil }
func (r *RecordingImage) SetWorkingDir(dir string) error  { r.config.WorkingDir = dir; return nil }

// SetCmd records the command. It normalizes a nil variadic to a non-nil empty
// slice so config.json emits `"cmd": []` (matching the contract) rather than
// `null`. The exporter intentionally calls SetCmd() with no args.
func (r *RecordingImage) SetCmd(v ...string) error {
	if v == nil {
		v = []string{}
	}
	r.config.Cmd = v
	return nil
}

func (r *RecordingImage) SetLabel(k, v string) error {
	r.config.Labels[k] = v
	return nil
}

func (r *RecordingImage) RemoveLabel(k string) error {
	delete(r.config.Labels, k)
	return nil
}

// SetEnv records the final value for a key. For PATH, the exporter READS the run
// image PATH (forwarded to the embedded image), prepends the CNB dirs, then calls
// SetEnv("PATH", <final>); we capture that FINAL value. See the design doc
// "Decision: PATH final-value".
func (r *RecordingImage) SetEnv(k, v string) error {
	r.config.Env[k] = v
	return nil
}

// --- Save: emit the contract instead of pushing ---

// Save writes <outputDir>/<RecorderDir>/plan.json and config.json instead of
// assembling/pushing an image. additionalNames are ignored (there is no image to
// tag); the consumer applies names when it assembles the image.
func (r *RecordingImage) Save(_ ...string) error {
	topLayer, err := r.TopLayer() // forwarded to the embedded run-image-backed image
	if err != nil {
		return errors.Wrap(err, "emit: get run image top layer")
	}

	dir := filepath.Join(r.outputDir, RecorderDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return errors.Wrapf(err, "emit: create output dir %s", dir)
	}

	// Persist the emitted layer tars into the output dir. The exporter builds them
	// under a temp artifacts dir that is removed after Export() returns, so the
	// consumer (which reads them from the build filesystem AFTER the export step)
	// would not find them at their original path. Copy each into
	// <outputDir>/<RecorderDir>/layers/ and rewrite TarPath to that stable
	// location so the consumer can add each layer by its own tar.
	layersOut := filepath.Join(dir, LayersSubdir)
	if err := os.MkdirAll(layersOut, 0755); err != nil {
		return errors.Wrapf(err, "emit: create layers dir %s", layersOut)
	}
	persisted := make([]LayerOp, len(r.layers))
	for i, l := range r.layers {
		persisted[i] = l
		if l.Reused || l.TarPath == "" {
			continue
		}
		dstName := fmt.Sprintf("%03d-%s.tar", i, sanitizeLayerName(l.ID))
		dstPath := filepath.Join(layersOut, dstName)
		if err := copyFile(l.TarPath, dstPath); err != nil {
			return errors.Wrapf(err, "emit: persist layer tar %s", l.TarPath)
		}
		// Record a path relative to the output dir root so the consumer can locate
		// it deterministically regardless of absolute mount point.
		persisted[i].TarPath = filepath.Join(RecorderDir, LayersSubdir, dstName)
	}

	plan := Plan{
		Schema: Schema,
		RunImage: PlanRunImage{
			Reference: r.runImageRef,
			TopLayer:  topLayer,
		},
		Layers: persisted,
	}

	if err := writeJSON(filepath.Join(dir, PlanFileName), plan); err != nil {
		return errors.Wrap(err, "emit: write plan.json")
	}
	if err := writeJSON(filepath.Join(dir, ConfigFileName), r.config); err != nil {
		return errors.Wrap(err, "emit: write config.json")
	}

	// Option A (build-then-finalize): also write the serialized BuildMetadata (the
	// plan + emitted config labels) that the build phase sets as the
	// BuildMetadataLabel on the produced image, and that finalize consumes. This is
	// the single artifact the finalize step needs from the build. The plan here
	// uses the ORIGINAL (non-persisted) layers so the intended diffIDs + reused
	// flags are exactly what the exporter produced; TarPath is irrelevant to
	// finalize.
	bmPlan := Plan{
		Schema:   Schema,
		RunImage: PlanRunImage{Reference: r.runImageRef, TopLayer: topLayer},
		Layers:   r.layers,
	}
	bm := NewBuildMetadata(bmPlan, r.config)
	if err := writeJSON(filepath.Join(dir, BuildMetadataFileName), bm); err != nil {
		return errors.Wrap(err, "emit: write build-metadata.json")
	}
	return nil
}

// SaveAs behaves like Save for emit-mode (there is no distinct name to save as).
func (r *RecordingImage) SaveAs(_ string, additionalNames ...string) error {
	return r.Save(additionalNames...)
}

// sanitizeLayerName turns a layer id/history string into a filesystem-safe token.
func sanitizeLayerName(id string) string {
	repl := strings.NewReplacer("/", "_", ":", "_", " ", "_", "'", "", ",", "", "@", "_")
	s := repl.Replace(id)
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// copyFile copies src to dst (used to persist emitted layer tars).
func copyFile(src, dst string) error {
	in, err := os.Open(filepath.Clean(src))
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(filepath.Clean(dst))
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func writeJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	return os.WriteFile(path, data, 0644)
}
