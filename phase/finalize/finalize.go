// Package finalize implements the FINALIZE step of the BuildKit-native export
// (Option A). After BuildKit builds and pushes a normal (runnable but not-yet-CNB-
// compliant) image carrying the emit.BuildMetadataLabel, Finalize reads the pushed
// image's ACTUAL produced layer diffIDs plus that label and AUTHORS the correct
// io.buildpacks.lifecycle.metadata (per-layer SHAs = produced diffIDs; run-image
// rebase boundary), re-pushing ONLY the image config + manifest (+ index for a
// manifest list). No layer blobs are read, added, removed, or re-uploaded.
//
// This is the lifecycle's home for CNB metadata authorship for the buildkit-native
// backend; pack consumes it as a library the way it consumes phase.Rebaser. A thin
// subcommand wrapper (cmd/lifecycle) allows standalone/self-healing use.
//
// See jericop/cnb-lifecycle/.kiro/specs/buildkit-native-export/design.md and the
// decision record spike-eliminate-metadata-rewrite.md.
package finalize

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	ggcrname "github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/pkg/errors"

	"github.com/buildpacks/lifecycle/phase/emit"
)

// Logger is the minimal logging surface Finalize uses. It matches the shape both
// pack and the lifecycle already have, so callers can pass their own logger.
type Logger interface {
	Debugf(format string, v ...interface{})
	Infof(format string, v ...interface{})
}

type noopLogger struct{}

func (noopLogger) Debugf(string, ...interface{}) {}
func (noopLogger) Infof(string, ...interface{})  {}

// Options configures Finalize.
type Options struct {
	// Insecure allows plain-HTTP registries (local/dev). When true the reference is
	// parsed with name.Insecure and remote uses HTTP.
	Insecure bool
	// KeepBuildMetadataLabel, when true, LEAVES emit.BuildMetadataLabel on the
	// finalized image (durable) so a later self-healing/repair run can re-finalize.
	// When false the label is removed after authoring the final metadata.
	KeepBuildMetadataLabel bool
	// Keychain overrides the registry auth keychain. Defaults to
	// authn.DefaultKeychain.
	Keychain authn.Keychain
	// Logger receives progress/debug messages. Defaults to a no-op.
	Logger Logger
}

func (o *Options) keychain() authn.Keychain {
	if o.Keychain != nil {
		return o.Keychain
	}
	return authn.DefaultKeychain
}

func (o *Options) logger() Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return noopLogger{}
}

// Finalize makes the pushed image at imageRef buildpacks-compliant by authoring
// io.buildpacks.lifecycle.metadata (and the other CNB labels) from the image's
// ACTUAL produced layer diffIDs and the emit.BuildMetadataLabel, then re-pushing
// config+manifest(+index) only. It handles both a single image and a manifest
// list. It is idempotent: finalizing an already-finalized image authors identical
// metadata (a no-op re-push at worst).
func Finalize(ctx context.Context, imageRef string, opts Options) error {
	log := opts.logger()
	ref, err := ggcrname.ParseReference(imageRef, nameOpts(opts.Insecure)...)
	if err != nil {
		return fmt.Errorf("parse image ref %q: %w", imageRef, err)
	}
	remoteOpts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(opts.keychain()),
	}

	desc, err := remote.Get(ref, remoteOpts...)
	if err != nil {
		return fmt.Errorf("fetch %q: %w", imageRef, err)
	}

	if desc.MediaType.IsIndex() {
		return finalizeIndex(ref, desc, remoteOpts, opts, log)
	}

	img, err := desc.Image()
	if err != nil {
		return fmt.Errorf("resolve image %q: %w", imageRef, err)
	}
	finalized, changed, err := finalizeImage(img, opts, log)
	if err != nil {
		return err
	}
	if !changed {
		log.Debugf("finalize: %s already compliant, nothing to do", imageRef)
		return nil
	}
	if err := remote.Write(ref, finalized, remoteOpts...); err != nil {
		return fmt.Errorf("push finalized image %q: %w", imageRef, err)
	}
	log.Infof("Finalized CNB metadata for %s", imageRef)
	return nil
}

// finalizeIndex finalizes each child image of a manifest list and re-pushes the
// index. Child layer blobs are unchanged (config+manifest only per child).
func finalizeIndex(ref ggcrname.Reference, desc *remote.Descriptor, remoteOpts []remote.Option, opts Options, log Logger) error {
	idx, err := desc.ImageIndex()
	if err != nil {
		return fmt.Errorf("resolve index: %w", err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return fmt.Errorf("read index manifest: %w", err)
	}

	result := idx
	anyChanged := false
	for _, m := range im.Manifests {
		if !m.MediaType.IsImage() {
			continue
		}
		child, err := idx.Image(m.Digest)
		if err != nil {
			return fmt.Errorf("resolve child %s: %w", m.Digest, err)
		}
		finalized, changed, err := finalizeImage(child, opts, log)
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		anyChanged = true
		result = mutate.RemoveManifests(result, func(d v1.Descriptor) bool {
			return d.Digest == m.Digest
		})
		result = mutate.AppendManifests(result, mutate.IndexAddendum{
			Add:        finalized,
			Descriptor: v1.Descriptor{Platform: m.Platform},
		})
	}
	if !anyChanged {
		log.Debugf("finalize: index %s already compliant, nothing to do", ref.Name())
		return nil
	}
	if err := remote.WriteIndex(ref, result, remoteOpts...); err != nil {
		return fmt.Errorf("push finalized index: %w", err)
	}
	log.Infof("Finalized CNB metadata for manifest list %s", ref.Name())
	return nil
}

// finalizeImage authors the CNB labels on a single image from its build-metadata
// label + actual produced diffIDs. Returns the updated image and whether a change
// was made.
func finalizeImage(img v1.Image, opts Options, log Logger) (v1.Image, bool, error) {
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, false, fmt.Errorf("read image config: %w", err)
	}
	if cfg.Config.Labels == nil {
		return img, false, nil
	}
	bmJSON, ok := cfg.Config.Labels[emit.BuildMetadataLabel]
	if !ok {
		// No build-metadata label: either not a buildkit-native image, or already
		// finalized with the label removed. Nothing to do.
		return img, false, nil
	}
	bm, err := emit.ParseBuildMetadata(bmJSON)
	if err != nil {
		return nil, false, err
	}

	// Map INTENDED new-layer diffIDs -> PRODUCED diffIDs positionally. The new
	// layers occupy the trailing len(intended) positions of the image's actual
	// diffIDs, in plan order (they were added on top of the run-image base).
	intended := bm.NewLayerDiffIDs()
	produced := cfg.RootFS.DiffIDs
	if len(intended) > len(produced) {
		return nil, false, fmt.Errorf("build-metadata lists %d new layers but image has %d layers", len(intended), len(produced))
	}
	producedNew := produced[len(produced)-len(intended):]
	intendedToProduced := make(map[string]string, len(intended))
	for i, in := range intended {
		intendedToProduced[in] = producedNew[i].String()
	}

	// Author the final labels from the build-metadata label's emitted labels,
	// remapping every intended diffID to its produced diffID inside the
	// io.buildpacks.lifecycle.metadata label (and any other label that references a
	// layer sha). String replacement is safe: diffIDs are unique 71-char tokens.
	newLabels := make(map[string]string, len(cfg.Config.Labels))
	for k, v := range cfg.Config.Labels {
		newLabels[k] = v
	}
	authored := false
	for k, v := range bm.Labels {
		out := v
		for in, prod := range intendedToProduced {
			out = strings.ReplaceAll(out, in, prod)
		}
		if newLabels[k] != out {
			authored = true
		}
		newLabels[k] = out
	}

	// Drop or keep the build-metadata label per options.
	if opts.KeepBuildMetadataLabel {
		// Keep it as-is (durable for self-healing). It is already in newLabels.
	} else {
		delete(newLabels, emit.BuildMetadataLabel)
		authored = true // removing the label is a change
	}

	if !authored {
		return img, false, nil
	}

	newCfg := cfg.DeepCopy()
	newCfg.Config.Labels = newLabels
	out, err := mutate.ConfigFile(img, newCfg)
	if err != nil {
		return nil, false, fmt.Errorf("apply finalized config: %w", err)
	}
	log.Debugf("finalize: authored CNB metadata (%d new-layer diffID remap(s))", len(intendedToProduced))
	return out, true, nil
}

func nameOpts(insecure bool) []ggcrname.Option {
	if insecure {
		return []ggcrname.Option{ggcrname.Insecure, ggcrname.WeakValidation}
	}
	return []ggcrname.Option{ggcrname.WeakValidation}
}

// ensure errors import retained (wrap helper used by callers/tests)
var _ = errors.Wrap
