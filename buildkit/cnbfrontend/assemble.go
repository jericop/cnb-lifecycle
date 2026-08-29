package cnbfrontend

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"

	"github.com/buildpacks/lifecycle/phase/emit"
	"github.com/buildpacks/lifecycle/platform/files"
)

const (
	appDir      = "/workspace"
	layersDir   = "/layers"
	platformDir = "/platform"
	cacheDir    = "/cache"
	emitDir     = "/emit"
)

// ContextLocalName is the llb.Local name under which the app build context is
// provided. EXPORTED so the pack-side consumer uses the same key when wiring
// SolveOpt.LocalMounts.
const ContextLocalName = "context"

// buildPlatform builds one platform: it solves the emit-mode graph (producing the
// emit contract + layer tars inside BuildKit), reads the contract, then assembles
// the final image FROM the run image + the emitted layers and returns the
// assembled ref plus the marshaled image config (with CNB config + labels).
func buildPlatform(ctx context.Context, c client.Client, cfg *buildConfig, p ocispecs.Platform) (client.Reference, []byte, error) {
	// 1) Emit-mode build graph -> solve -> ref we can ReadFile from.
	built := buildEmitState(cfg, p)
	builtRef, err := solveState(ctx, c, built, p)
	if err != nil {
		return nil, nil, errors.Wrap(err, "solve emit graph")
	}

	// 2) Read the emit contract from the solved state.
	contract, err := readEmitContract(ctx, builtRef)
	if err != nil {
		return nil, nil, errors.Wrap(err, "read emit contract")
	}

	// 3) Assemble FROM run-image + copy the emitted layers.
	assembled, baseImg, err := assembleState(ctx, c, cfg, p, built, builtRef, contract)
	if err != nil {
		return nil, nil, errors.Wrap(err, "assemble image")
	}
	assembledRef, err := solveState(ctx, c, assembled, p)
	if err != nil {
		return nil, nil, errors.Wrap(err, "solve assembled image")
	}

	// 4) Read the emitted image config, apply the RUNTIME config (entrypoint / cmd
	//    / workingdir / env) onto the run-image base so the image is runnable, and
	//    set the single BUILD-METADATA label. Option A (build-then-finalize): the
	//    build phase MUST NOT write a valid final io.buildpacks.lifecycle.metadata
	//    (its per-layer SHAs would be the INTENDED, pre-produce diffIDs, not the
	//    diffIDs BuildKit assigns at export). Instead we carry the plan + emitted
	//    labels in the io.buildpacks.buildkit.native.build-metadata label; a
	//    lifecycle FINALIZE step authors the real CNB metadata from the produced
	//    diffIDs afterward. See the buildkit-native-export design.
	emitCfg, err := readEmitConfig(ctx, builtRef)
	if err != nil {
		return nil, nil, errors.Wrap(err, "read emit config")
	}
	img := *baseImg // copy the run-image config as the base
	applyRuntimeConfig(&img, emitCfg)

	if img.Config.Labels == nil {
		img.Config.Labels = map[string]string{}
	}
	bmJSON, err := readBuildMetadataJSON(ctx, builtRef)
	if err != nil {
		return nil, nil, errors.Wrap(err, "read build-metadata")
	}
	img.Config.Labels[emit.BuildMetadataLabel] = bmJSON

	imgConfig, err := marshalJSON(img)
	if err != nil {
		return nil, nil, errors.Wrap(err, "marshal image config")
	}
	return assembledRef, imgConfig, nil
}

// readBuildMetadataJSON reads the serialized BuildMetadata the emit-mode exporter
// wrote (<emitDir>/buildkit/build-metadata.json) from the built state, and returns
// it as the JSON string to set as emit.BuildMetadataLabel. It validates the schema
// via emit.ParseBuildMetadata but returns the ORIGINAL bytes (so the label value is
// exactly what the lifecycle produced).
func readBuildMetadataJSON(ctx context.Context, ref client.Reference) (string, error) {
	p := path.Join(emitDir, emit.RecorderDir, emit.BuildMetadataFileName)
	data, err := ref.ReadFile(ctx, client.ReadRequest{Filename: p})
	if err != nil {
		return "", errors.Wrapf(err, "read %s", p)
	}
	if _, err := emit.ParseBuildMetadata(string(data)); err != nil {
		return "", err
	}
	return string(data), nil
}

// buildEmitState mirrors the pack LLB backend's build graph through the builder
// phase, then runs the exporter in EMIT-MODE. The resulting state has
// /emit/buildkit/{plan.json,config.json} and /layers/<tars>.
func buildEmitState(cfg *buildConfig, p ocispecs.Platform) llb.State {
	base := llb.Image(cfg.builderImage, llb.Platform(p))

	// Overlay the emit-capable lifecycle if provided.
	if cfg.lifecycleImage != "" {
		lc := llb.Image(cfg.lifecycleImage, llb.Platform(p))
		base = base.Run(
			llb.Args([]string{"/bin/sh", "-c", "rm -rf /cnb/lifecycle"}),
			llb.WithCustomName("remove existing lifecycle"),
		).Root()
		base = base.File(
			llb.Copy(lc, "/cnb/lifecycle", "/cnb/lifecycle", &llb.CopyInfo{CreateDestPath: true}),
			llb.WithCustomName("overlay emit-capable lifecycle"),
		)
	}

	base = base.Run(
		llb.Args([]string{"/bin/sh", "-c", fmt.Sprintf("mkdir -p %s %s %s %s && chmod -R 777 %s %s %s", cacheDir, layersDir, platformDir, emitDir, cacheDir, layersDir, emitDir)}),
		llb.WithCustomName("setup directories"),
	).Root()

	if cfg.orderTOML != "" {
		orderCmd := fmt.Sprintf("cat > /cnb/order.toml << 'TOML'\n%s\nTOML", cfg.orderTOML)
		base = base.Run(
			llb.Args([]string{"/bin/bash", "-c", orderCmd}),
			llb.WithCustomName("write order.toml"),
			llb.User("0:0"),
		).Root()
	}

	// Copy app source from the build context.
	appSrc := llb.Local(ContextLocalName)
	base = base.File(
		llb.Copy(appSrc, "/", appDir, &llb.CopyInfo{CreateDestPath: true, AllowWildcard: true, AllowEmptyWildcard: true}),
		llb.WithCustomName("copy app source"),
	)
	base = base.Run(
		llb.Args([]string{"/bin/sh", "-c", "chmod -R 777 " + appDir}),
		llb.WithCustomName("fix workspace permissions"),
	).Root()

	cacheMount := llb.AddMount(cacheDir, llb.Scratch(), llb.AsPersistentCacheDir("cnb-buildpacks-cache-"+p.Architecture, llb.CacheMountShared))

	cnbUser := fmt.Sprintf("%d:%d", cfg.uid, cfg.gid)
	env := []llb.RunOption{
		llb.AddEnv("CNB_PLATFORM_API", cfg.platformAPI),
		llb.AddEnv("CNB_USER_ID", fmt.Sprintf("%d", cfg.uid)),
		llb.AddEnv("CNB_GROUP_ID", fmt.Sprintf("%d", cfg.gid)),
		llb.User(cnbUser),
		// Run lifecycle phases on the builder's HOST network so the analyzer/
		// exporter can reach registries the builder is attached to (e.g. a local
		// dev registry reachable by container name). BuildKit's default RUN
		// sandbox network uses the daemon's upstream DNS and cannot resolve the
		// builder's docker-network peers. (MVP: revisit for production network
		// isolation.)
		llb.Network(pb.NetMode_HOST),
	}
	if cfg.registryAuth != "" {
		env = append(env, llb.AddEnv("CNB_REGISTRY_AUTH", cfg.registryAuth))
	}

	base = base.Run(
		llb.Args([]string{"/bin/sh", "-c", "chmod 777 " + cacheDir}),
		llb.WithCustomName("fix cache mount permissions"), cacheMount, llb.IgnoreCache,
	).Root()

	skipChown := []string{"-skip-chown", "-uid", fmt.Sprintf("%d", cfg.uid), "-gid", fmt.Sprintf("%d", cfg.gid)}
	insecure := insecureRegistryArgs(cfg.insecureRegistries)

	// analyzer (resolves run image into analyzed.toml; sets the rebase boundary)
	analyzerArgs := append([]string{"/cnb/lifecycle/analyzer"}, skipChown...)
	analyzerArgs = append(analyzerArgs, insecure...)
	analyzerArgs = append(analyzerArgs, "-run-image", cfg.runImage, "-layers", layersDir, cfg.imageName)
	base = base.Run(append([]llb.RunOption{llb.Args(analyzerArgs), llb.WithCustomName("lifecycle: analyzer"), cacheMount}, env...)...).Root()

	// detector
	base = base.Run(append([]llb.RunOption{llb.Args([]string{"/cnb/lifecycle/detector", "-app", appDir, "-layers", layersDir}), llb.WithCustomName("lifecycle: detector")}, env...)...).Root()

	// restorer
	restorerArgs := append([]string{"/cnb/lifecycle/restorer"}, skipChown...)
	restorerArgs = append(restorerArgs, "-layers", layersDir)
	base = base.Run(append([]llb.RunOption{llb.Args(restorerArgs), llb.WithCustomName("lifecycle: restorer"), cacheMount}, env...)...).Root()

	// builder
	base = base.Run(append([]llb.RunOption{llb.Args([]string{"/cnb/lifecycle/builder", "-app", appDir, "-layers", layersDir}), llb.WithCustomName("lifecycle: builder")}, env...)...).Root()

	// exporter in EMIT-MODE
	exporterArgs := append([]string{"/cnb/lifecycle/exporter"}, skipChown...)
	exporterArgs = append(exporterArgs, insecure...)
	exporterArgs = append(exporterArgs, "-layers", layersDir, "-app", appDir, "-emit-export-plan", emitDir, cfg.imageName)
	base = base.Run(append([]llb.RunOption{llb.Args(exporterArgs), llb.WithCustomName("lifecycle: exporter (emit-mode)"), cacheMount}, env...)...).Root()

	return base
}

// insecureRegistryArgs renders -insecure-registry flags for the given registries.
func insecureRegistryArgs(regs []string) []string {
	var out []string
	for _, r := range regs {
		out = append(out, "-insecure-registry", r)
	}
	return out
}

// readEmitContract reads and validates the emit contract from the solved state.
func readEmitContract(ctx context.Context, ref client.Reference) (*emit.Plan, error) {
	planPath := path.Join(emitDir, emit.RecorderDir, emit.PlanFileName)
	data, err := ref.ReadFile(ctx, client.ReadRequest{Filename: planPath})
	if err != nil {
		return nil, errors.Wrapf(err, "read %s", planPath)
	}
	var plan emit.Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, errors.Wrapf(err, "parse %s", planPath)
	}
	if plan.Schema != emit.Schema {
		return nil, fmt.Errorf("emit plan schema %q unsupported (want %q)", plan.Schema, emit.Schema)
	}
	if len(plan.Layers) == 0 {
		return nil, fmt.Errorf("emit plan has no layers")
	}
	return &plan, nil
}

// readEmitConfig reads the emitted image config (entrypoint/env/workingdir/labels).
func readEmitConfig(ctx context.Context, ref client.Reference) (*emit.ImageConfig, error) {
	cfgPath := path.Join(emitDir, emit.RecorderDir, emit.ConfigFileName)
	data, err := ref.ReadFile(ctx, client.ReadRequest{Filename: cfgPath})
	if err != nil {
		return nil, errors.Wrapf(err, "read %s", cfgPath)
	}
	var ic emit.ImageConfig
	if err := json.Unmarshal(data, &ic); err != nil {
		return nil, errors.Wrapf(err, "parse %s", cfgPath)
	}
	return &ic, nil
}

// assembleState builds the final image state: FROM run-image + copy each emitted
// NEW layer's files from the built state. Reused run-image layers are already in
// the base. Returns the assembled state and the base run-image config.
//
// The run-image reference used as the base is the FULLY-RESOLVED, digest-pinned
// reference the analyzer wrote to /layers/analyzed.toml (e.g.
// "index.docker.io/paketobuildpacks/ubuntu-noble-run@sha256:..."), NOT the raw
// cfg.runImage option — the raw option may be an un-normalized short ref (e.g.
// "paketobuildpacks/ubuntu-noble-run:latest") that BuildKit would misparse as a
// registry host. Using the analyzed reference also pins the exact base the
// exporter recorded, keeping the rebase boundary consistent.
func assembleState(ctx context.Context, c client.Client, cfg *buildConfig, p ocispecs.Platform, built llb.State, builtRef client.Reference, plan *emit.Plan) (llb.State, *dockerspec.DockerOCIImage, error) {
	runRef, err := resolvedRunImageRef(ctx, builtRef, cfg.runImage)
	if err != nil {
		return llb.State{}, nil, err
	}

	baseImg, err := resolveImageConfig(ctx, c, runRef, p)
	if err != nil {
		return llb.State{}, nil, errors.Wrap(err, "resolve run image config")
	}

	state := llb.Image(runRef, llb.Platform(p))

	// Assemble ONE LAYER PER EMITTED CNB LAYER, in plan order, by extracting each
	// emitted layer tar onto the run-image base with its own RUN step. This gives
	// the assembled image the SAME layer boundaries + count as the emit plan (so
	// each buildpack layer is individually addressable), which is required for:
	//   - the analyzer's previous-image restore on rebuilds, and
	//   - buildpack-contributed-layer patching (jab/buildpack-layer-patching).
	// BuildKit recomputes each layer's diffID from the extracted filesystem; the
	// caller rewrites the io.buildpacks.lifecycle.metadata per-layer SHAs to the
	// actual produced diffIDs so the metadata stays consistent with the image.
	//
	// The emitted tars live in the built state at plan layer TarPath. We mount the
	// built state read-only at /emit-tars and `tar -xf` each in order. Reused
	// (run-image) layers are already present in the base and are skipped here.
	//
	// MVP ASSUMPTION: the run image provides /bin/sh and tar (true for the
	// ubuntu-noble run image). A future hardening can extract using tooling from
	// the builder image (mounted in) so the run image needs no shell/tar.
	tarsMount := llb.AddMount("/emit-tars", built, llb.Readonly)
	for _, layer := range plan.Layers {
		if layer.Reused || layer.TarPath == "" {
			continue
		}
		// layer.TarPath is recorded RELATIVE to the emit output dir root (e.g.
		// "buildkit/layers/000-....tar"); the emit output dir is emitDir in the
		// built state, mounted at /emit-tars. So the tar is at
		// /emit-tars/<emitDir>/<TarPath>.
		src := path.Join("/emit-tars", emitDir, layer.TarPath)
		state = state.Run(
			llb.Args([]string{"/bin/sh", "-c", fmt.Sprintf("tar -xf %q -C /", src)}),
			llb.WithCustomNamef("assemble layer: %s", layer.ID),
			tarsMount,
		).Root()
	}
	return state, baseImg, nil
}

// resolvedRunImageRef returns the fully-resolved run-image reference to use as
// the assembly base. It prefers the analyzer-resolved reference from
// /layers/analyzed.toml (digest-pinned, fully-qualified); if that is unavailable
// it falls back to normalizing the raw run-image option.
func resolvedRunImageRef(ctx context.Context, builtRef client.Reference, rawRunImage string) (string, error) {
	data, err := builtRef.ReadFile(ctx, client.ReadRequest{Filename: path.Join(layersDir, "analyzed.toml")})
	if err == nil {
		var analyzed files.Analyzed
		if _, derr := toml.Decode(string(data), &analyzed); derr == nil {
			if ref := analyzed.RunImageRef(); ref != "" {
				return normalizeImageRef(ref), nil
			}
		}
	}
	// Fallback: normalize the raw option (add docker.io / library as needed).
	if rawRunImage == "" {
		return "", fmt.Errorf("no run image reference available (analyzed.toml missing and no run-image option)")
	}
	return normalizeImageRef(rawRunImage), nil
}

// normalizeImageRef fully-qualifies an image reference so BuildKit resolves it
// against the correct registry (Docker Hub for short refs) rather than treating
// the first path element as a registry host.
func normalizeImageRef(ref string) string {
	named, err := name.ParseReference(ref, name.WeakValidation)
	if err != nil {
		return ref
	}
	return named.Name()
}

// resolveImageConfig resolves an image reference's config via the gateway and
// unmarshals it into a DockerOCIImage.
func resolveImageConfig(ctx context.Context, c client.Client, ref string, p ocispecs.Platform) (*dockerspec.DockerOCIImage, error) {
	_, _, data, err := c.ResolveImageConfig(ctx, ref, sourceresolver.Opt{
		ImageOpt: &sourceresolver.ResolveImageOpt{Platform: &p},
	})
	if err != nil {
		return nil, err
	}
	var img dockerspec.DockerOCIImage
	if err := json.Unmarshal(data, &img); err != nil {
		return nil, err
	}
	return &img, nil
}

// solveState marshals + solves an llb.State for platform p and returns its ref.
func solveState(ctx context.Context, c client.Client, st llb.State, p ocispecs.Platform) (client.Reference, error) {
	def, err := st.Marshal(ctx, llb.Platform(p))
	if err != nil {
		return nil, errors.Wrap(err, "marshal state")
	}
	r, err := c.Solve(ctx, client.SolveRequest{Definition: def.ToPB()})
	if err != nil {
		return nil, errors.Wrap(err, "solve")
	}
	return r.SingleRef()
}

func marshalJSON(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// applyRuntimeConfig overlays ONLY the emitted RUNTIME config (entrypoint / cmd /
// workingdir / env) onto the image config so the built image is runnable. It does
// NOT copy the emitted CNB LABELS (io.buildpacks.lifecycle.metadata etc.): under
// Option A those are AUTHORED by the finalize step from the produced diffIDs, and
// the build phase must not pre-write a valid-looking final metadata label with the
// intended (pre-produce) SHAs. The emitted labels are carried instead inside the
// build-metadata label (via emit.BuildMetadata.Labels) for finalize to consume.
func applyRuntimeConfig(img *dockerspec.DockerOCIImage, ic *emit.ImageConfig) {
	img.Config.Entrypoint = ic.Entrypoint
	img.Config.Cmd = ic.Cmd
	img.Config.WorkingDir = ic.WorkingDir
	// Merge env: keep base env, then set/override CNB env keys.
	img.Config.Env = mergeEnv(img.Config.Env, ic.Env)
}

func mergeEnv(base []string, overlay map[string]string) []string {
	out := make([]string, 0, len(base)+len(overlay))
	seen := map[string]bool{}
	for k, v := range overlay {
		out = append(out, k+"="+v)
		seen[k] = true
	}
	for _, e := range base {
		k := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			k = e[:i]
		}
		if !seen[k] {
			out = append(out, e)
		}
	}
	return out
}
