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
	"time"

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

	// The whole finalize runs HOST-SIDE after BuildKit has already pushed the image,
	// so it produces no BuildKit progress vertex. Without these lines the caller sees
	// an unexplained pause (see PLATFORM-1662 FR-9). Log start + a fetch step so the
	// gap is attributable; finalizeIndex adds per-child (per-arch) timing below.
	start := time.Now()
	log.Infof("finalize: %s: resolving pushed image", imageRef)
	desc, err := remote.Get(ref, remoteOpts...)
	if err != nil {
		return fmt.Errorf("fetch %q: %w", imageRef, err)
	}

	if desc.MediaType.IsIndex() {
		return finalizeIndex(ref, desc, remoteOpts, opts, log, start)
	}

	img, err := desc.Image()
	if err != nil {
		return fmt.Errorf("resolve image %q: %w", imageRef, err)
	}
	// Single-image (non-manifest-list) finalize: label with the image's own platform
	// so the line is arch-attributed like the manifest-list children.
	label := platformLabelFromConfig(img)
	finalized, changed, err := finalizeImage(img, opts, log, label)
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
	log.Infof("finalize %s: %s: authored CNB metadata + re-pushed config/manifest (%s)", label, imageRef, time.Since(start).Round(time.Millisecond))
	log.Infof("Finalized CNB metadata for %s", imageRef)
	return nil
}

// finalizeIndex finalizes each child image of a manifest list and re-pushes the
// index. Child layer blobs are unchanged (config+manifest only per child).
//
// Each child is finalized SEQUENTIALLY, and each finalizeImage forces a network
// fetch of that child's config blob. This is the host-side post-push work that has
// no BuildKit progress vertex, so we log a per-child, arch-labeled timing line
// (PLATFORM-1662 FR-9) so the operator can see which platform is being processed
// and how long each takes instead of an unexplained pause.
func finalizeIndex(ref ggcrname.Reference, desc *remote.Descriptor, remoteOpts []remote.Option, opts Options, log Logger, start time.Time) error {
	idx, err := desc.ImageIndex()
	if err != nil {
		return fmt.Errorf("resolve index: %w", err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return fmt.Errorf("read index manifest: %w", err)
	}

	nChildren := 0
	for _, m := range im.Manifests {
		if m.MediaType.IsImage() {
			nChildren++
		}
	}
	log.Infof("finalize: %s: manifest list with %d image(s); authoring CNB metadata per platform", ref.Name(), nChildren)

	result := idx
	anyChanged := false
	idxChild := 0
	for _, m := range im.Manifests {
		if !m.MediaType.IsImage() {
			continue
		}
		idxChild++
		label := platformLabelFromDescriptor(m.Platform)
		childStart := time.Now()
		log.Infof("finalize %s: image %d/%d (%s): fetching config + authoring metadata", label, idxChild, nChildren, shortDigest(m.Digest.String()))

		child, err := idx.Image(m.Digest)
		if err != nil {
			return fmt.Errorf("resolve child %s (%s): %w", label, m.Digest, err)
		}
		finalized, changed, err := finalizeImage(child, opts, log, label)
		if err != nil {
			return err
		}
		if !changed {
			log.Infof("finalize %s: image %d/%d already compliant (%s)", label, idxChild, nChildren, time.Since(childStart).Round(time.Millisecond))
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
		log.Infof("finalize %s: image %d/%d authored (%s)", label, idxChild, nChildren, time.Since(childStart).Round(time.Millisecond))
	}
	if !anyChanged {
		log.Debugf("finalize: index %s already compliant, nothing to do", ref.Name())
		return nil
	}
	log.Infof("finalize: %s: re-pushing manifest list index", ref.Name())
	if err := remote.WriteIndex(ref, result, remoteOpts...); err != nil {
		return fmt.Errorf("push finalized index: %w", err)
	}
	log.Infof("finalize: %s: done in %s", ref.Name(), time.Since(start).Round(time.Millisecond))
	log.Infof("Finalized CNB metadata for manifest list %s", ref.Name())
	return nil
}

// finalizeImage authors the CNB labels on a single image from its build-metadata
// label + actual produced diffIDs. Returns the updated image and whether a change
// was made. label is an "[os/arch]" tag used only for progress logging.
func finalizeImage(img v1.Image, opts Options, log Logger, label string) (v1.Image, bool, error) {
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
	log.Debugf("finalize %s: authored CNB metadata (%d new-layer diffID remap(s))", label, len(intendedToProduced))
	return out, true, nil
}

func nameOpts(insecure bool) []ggcrname.Option {
	if insecure {
		return []ggcrname.Option{ggcrname.Insecure, ggcrname.WeakValidation}
	}
	return []ggcrname.Option{ggcrname.WeakValidation}
}

// platformLabelFromDescriptor renders a manifest-list child's platform as an
// "[os/arch]" (or "[os/arch/variant]") tag matching the prefix the buildkit backend
// uses on its progress vertices, so finalize lines line up with the rest of the
// build output. Returns "[unknown-platform]" when the descriptor has no platform.
func platformLabelFromDescriptor(p *v1.Platform) string {
	if p == nil || p.OS == "" || p.Architecture == "" {
		return "[unknown-platform]"
	}
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return "[" + s + "]"
}

// platformLabelFromConfig renders a single image's own os/arch (from its config)
// as an "[os/arch]" tag. Best-effort: returns "[image]" if the config can't be read.
func platformLabelFromConfig(img v1.Image) string {
	cfg, err := img.ConfigFile()
	if err != nil || cfg == nil || cfg.OS == "" || cfg.Architecture == "" {
		return "[image]"
	}
	s := cfg.OS + "/" + cfg.Architecture
	if cfg.Variant != "" {
		s += "/" + cfg.Variant
	}
	return "[" + s + "]"
}

// shortDigest trims a "sha256:<hex>" to "sha256:<first12>" for readable logs.
func shortDigest(d string) string {
	if i := strings.IndexByte(d, ':'); i >= 0 && len(d) >= i+1+12 {
		return d[:i+1+12]
	}
	return d
}

// ensure errors import retained (wrap helper used by callers/tests)
var _ = errors.Wrap
