package emit

import (
	"encoding/json"
	"fmt"
)

// BuildMetadataLabel is the image LABEL under which the BuildKit-native BUILD
// phase surfaces the ordered layer plan + emitted config labels for the FINALIZE
// step to consume.
//
// It is intentionally NAMESPACED under io.buildpacks.buildkit.native.* and is
// DISTINCT from the final io.buildpacks.lifecycle.metadata label: the build phase
// must NOT pre-write a valid final CNB metadata label, because the per-layer SHAs
// it would contain are the INTENDED (pre-produce) diffIDs, not the diffIDs BuildKit
// actually assigns at export. Finalize reads this label + the image's ACTUAL
// produced diffIDs and AUTHORS the correct io.buildpacks.lifecycle.metadata.
//
// The label is a build-phase artifact and is only PARTIALLY valid on its own
// (its per-layer diffIDs are the intended ones). Finalize reconciles it against
// the produced image.
const BuildMetadataLabel = "io.buildpacks.buildkit.native.build-metadata"

// BuildMetadata is the payload of BuildMetadataLabel. It carries everything the
// finalize step needs to author correct CNB metadata WITHOUT re-reading any layer
// data:
//   - the ordered Layer Plan (order, new-vs-reused, INTENDED diffID, identity,
//     history) — from which finalize maps INTENDED -> PRODUCED diffIDs positionally,
//   - the run-image rebase boundary, and
//   - the emitted image config LABELS (io.buildpacks.lifecycle.metadata with the
//     intended SHAs, plus io.buildpacks.build.metadata / project.metadata /
//     exec-env). Finalize remaps the SHAs in the lifecycle-metadata label to the
//     produced diffIDs and writes the other labels verbatim.
//
// New fields may be added over time without adding any image LAYER.
type BuildMetadata struct {
	// Schema is the contract version (Schema). Finalize validates it.
	Schema string `json:"schema"`
	// Plan is the ordered layer plan the exporter emitted.
	Plan Plan `json:"plan"`
	// Labels are the image config labels the exporter would have set, including the
	// INTENDED io.buildpacks.lifecycle.metadata. Finalize remaps the per-layer SHAs
	// in the lifecycle-metadata label to the produced diffIDs and applies the rest
	// verbatim.
	Labels map[string]string `json:"labels"`
	// Env / Entrypoint / Cmd / WorkingDir mirror the emitted image config so a
	// standalone finalize can reassert them if needed. Normally BuildKit already
	// set these on the built image; finalize does not need to change them.
	Env        map[string]string `json:"env,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	WorkingDir string            `json:"workingDir,omitempty"`
}

// NewBuildMetadata builds a BuildMetadata from a recorded Plan + ImageConfig.
func NewBuildMetadata(plan Plan, config ImageConfig) BuildMetadata {
	return BuildMetadata{
		Schema:     Schema,
		Plan:       plan,
		Labels:     config.Labels,
		Env:        config.Env,
		Entrypoint: config.Entrypoint,
		Cmd:        config.Cmd,
		WorkingDir: config.WorkingDir,
	}
}

// Marshal serializes the BuildMetadata to the JSON string stored in
// BuildMetadataLabel.
func (m BuildMetadata) Marshal() (string, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal build-metadata: %w", err)
	}
	return string(data), nil
}

// ParseBuildMetadata parses the BuildMetadataLabel value and validates the schema.
func ParseBuildMetadata(labelValue string) (BuildMetadata, error) {
	var m BuildMetadata
	if err := json.Unmarshal([]byte(labelValue), &m); err != nil {
		return BuildMetadata{}, fmt.Errorf("parse build-metadata: %w", err)
	}
	if m.Schema != Schema {
		return BuildMetadata{}, fmt.Errorf("build-metadata schema %q unsupported (want %q)", m.Schema, Schema)
	}
	if len(m.Plan.Layers) == 0 {
		return BuildMetadata{}, fmt.Errorf("build-metadata plan has no layers")
	}
	return m, nil
}

// NewLayerDiffIDs returns the INTENDED diffIDs of the plan's NEW (non-reused)
// layers, in plan order. Finalize pairs these positionally with the image's
// trailing produced diffIDs to build the intended->produced map.
func (m BuildMetadata) NewLayerDiffIDs() []string {
	var out []string
	for _, l := range m.Plan.Layers {
		if l.Reused {
			continue
		}
		out = append(out, l.DiffID)
	}
	return out
}
