// Package cnbfrontend implements the CNB BuildKit gateway frontend logic as an
// IMPORTABLE library so it can be consumed two ways:
//   - in-process by pack via client.Client.Build(ctx, opt, product, Build, ch)
//     (no separate frontend image needed), and
//   - as a standalone frontend image via cmd/cnb-frontend (a thin main that calls
//     grpcclient.RunFromEnvironment(ctx, Build)).
package cnbfrontend

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/frontend/gateway/client"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

// Frontend option keys (passed by pack via client.Build FrontendAttrs and read
// here via BuildOpts().Opts). EXPORTED so the pack-side consumer imports the exact
// same keys (single source of truth, no drift).
const (
	OptBuilderImage   = "cnb-builder-image"   // builder image ref (base for lifecycle phases)
	OptRunImage       = "cnb-run-image"       // run image ref (assembly base)
	OptLifecycleImage = "cnb-lifecycle-image" // optional: lifecycle image to overlay /cnb/lifecycle
	OptPlatforms      = "cnb-platforms"       // comma-separated os/arch list
	OptPlatformAPI    = "cnb-platform-api"    // CNB platform API version
	OptUID            = "cnb-uid"             // CNB user id in builder
	OptGID            = "cnb-gid"             // CNB group id in builder
	OptOrderTOML      = "cnb-order-toml"      // optional custom order.toml content
	OptRegistryAuth   = "cnb-registry-auth"   // optional CNB_REGISTRY_AUTH json
	OptImageName      = "cnb-image-name"      // target image name (used by in-build lifecycle phases)
	OptInsecureReg    = "cnb-insecure-registries" // comma-separated insecure registries
)

// buildConfig is the parsed set of frontend options for one invocation.
type buildConfig struct {
	builderImage       string
	runImage           string
	lifecycleImage     string
	platforms          []ocispecs.Platform
	platformAPI        string
	uid                int
	gid                int
	orderTOML          string
	registryAuth       string
	imageName          string
	insecureRegistries []string
}

// Build is the gateway BuildFunc entrypoint. It parses frontend options, builds
// each requested platform (emit-mode graph + FROM-run-image assembly), and
// returns per-platform refs + image configs so BuildKit exports one
// (multi-platform) image with no intermediate tags.
func Build(ctx context.Context, c client.Client) (*client.Result, error) {
	cfg, err := parseBuildConfig(c)
	if err != nil {
		return nil, err
	}

	multiPlatform := len(cfg.platforms) > 1
	res := client.NewResult()
	expPlatforms := &exptypes.Platforms{
		Platforms: make([]exptypes.Platform, len(cfg.platforms)),
	}

	eg, ctx := errgroup.WithContext(ctx)
	for i, p := range cfg.platforms {
		i, p := i, p
		eg.Go(func() error {
			ref, imgConfig, err := buildPlatform(ctx, c, cfg, p)
			if err != nil {
				return errors.Wrapf(err, "building %s", platforms.Format(p))
			}
			if !multiPlatform {
				res.AddMeta(exptypes.ExporterImageConfigKey, imgConfig)
				res.SetRef(ref)
				return nil
			}
			k := platforms.Format(p)
			res.AddMeta(fmt.Sprintf("%s/%s", exptypes.ExporterImageConfigKey, k), imgConfig)
			res.AddRef(k, ref)
			expPlatforms.Platforms[i] = exptypes.Platform{ID: k, Platform: p}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	if multiPlatform {
		dt, err := marshalJSON(expPlatforms)
		if err != nil {
			return nil, errors.Wrap(err, "marshal platforms")
		}
		res.AddMeta(exptypes.ExporterPlatformsKey, dt)
	}
	return res, nil
}

// parseBuildConfig reads the frontend options from the gateway BuildOpts.
func parseBuildConfig(c client.Client) (*buildConfig, error) {
	opts := c.BuildOpts().Opts

	cfg := &buildConfig{
		builderImage:   opts[OptBuilderImage],
		runImage:       opts[OptRunImage],
		lifecycleImage: opts[OptLifecycleImage],
		platformAPI:    opts[OptPlatformAPI],
		orderTOML:      opts[OptOrderTOML],
		registryAuth:   opts[OptRegistryAuth],
		imageName:      opts[OptImageName],
	}
	if s := strings.TrimSpace(opts[OptInsecureReg]); s != "" {
		for _, r := range strings.Split(s, ",") {
			if r = strings.TrimSpace(r); r != "" {
				cfg.insecureRegistries = append(cfg.insecureRegistries, r)
			}
		}
	}
	if cfg.imageName == "" {
		cfg.imageName = "cnb-native-build:latest" // placeholder; only used by in-build phases
	}
	if cfg.builderImage == "" {
		return nil, fmt.Errorf("frontend option %q is required", OptBuilderImage)
	}
	if cfg.runImage == "" {
		return nil, fmt.Errorf("frontend option %q is required", OptRunImage)
	}
	if cfg.platformAPI == "" {
		cfg.platformAPI = "0.13"
	}
	cfg.uid = atoiDefault(opts[OptUID], 1001)
	cfg.gid = atoiDefault(opts[OptGID], 1001)

	ps, err := parsePlatforms(opts[OptPlatforms])
	if err != nil {
		return nil, err
	}
	cfg.platforms = ps
	return cfg, nil
}

func parsePlatforms(s string) ([]ocispecs.Platform, error) {
	if strings.TrimSpace(s) == "" {
		// Default to the host platform if none specified.
		return []ocispecs.Platform{platforms.DefaultSpec()}, nil
	}
	var out []ocispecs.Platform
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := platforms.Parse(part)
		if err != nil {
			return nil, errors.Wrapf(err, "parse platform %q", part)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no platforms parsed from %q", s)
	}
	return out, nil
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
