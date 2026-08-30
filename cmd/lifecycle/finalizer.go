package main

import (
	"context"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/pkg/errors"

	"github.com/buildpacks/lifecycle/auth"
	"github.com/buildpacks/lifecycle/cmd"
	"github.com/buildpacks/lifecycle/cmd/lifecycle/cli"
	"github.com/buildpacks/lifecycle/phase/finalize"
	"github.com/buildpacks/lifecycle/platform"
)

// finalizeCmd is the thin subcommand wrapper around phase/finalize.Finalize. It
// authors the CNB metadata (io.buildpacks.lifecycle.metadata et al.) on an image
// that BuildKit already built and pushed carrying the buildkit-native
// build-metadata label, re-pushing config+manifest(+index) only.
//
// It is the standalone / self-healing entry point: given only an image reference
// that still carries the build-metadata label, it can (re-)finalize the image
// without rebuilding. pack normally calls finalize.Finalize as a library
// (post-push), but this subcommand lets an operator repair an image out-of-band,
// e.g. `lifecycle finalize -insecure <ref>`.
//
// Because it operates purely on a remote image reference it does NOT use the
// platform inputs machinery (layers dir, group, plan, etc.); it embeds a minimal
// LifecycleInputs only to satisfy the cli.Command interface and to reuse the
// shared flags (log level, no-color, insecure registries).
type finalizeCmd struct {
	*platform.Platform

	imageRef               string
	keepBuildMetadataLabel bool
	keychainImages         []string
	keychain               authn.Keychain
}

// DefineFlags defines the flags that are considered valid and reads their values (if provided).
func (f *finalizeCmd) DefineFlags() {
	cli.FlagInsecureRegistries(&f.InsecureRegistries)
	cli.FlagKeepBuildMetadataLabel(&f.keepBuildMetadataLabel)
	cli.FlagLogLevel(&f.LogLevel)
	cli.FlagNoColor(&f.NoColor)
}

// Args validates arguments and flags, and fills in default values.
func (f *finalizeCmd) Args(nargs int, args []string) error {
	if nargs != 1 {
		return cmd.FailErrCode(
			errors.New("exactly one image argument is required"),
			cmd.CodeForInvalidArgs, "parse arguments",
		)
	}
	f.imageRef = args[0]
	f.keychainImages = []string{f.imageRef}
	return nil
}

func (f *finalizeCmd) Privileges() error {
	// No privilege drop / daemon: finalize only mutates a remote image's config +
	// manifest. We construct the keychain here (before any potential privilege
	// change) so registry credentials are resolved for the target image.
	var err error
	f.keychain, err = auth.DefaultKeychain(f.keychainImages...)
	if err != nil {
		return cmd.FailErr(err, "resolve keychain")
	}
	return nil
}

func (f *finalizeCmd) Exec() error {
	insecure := isInsecureRef(f.imageRef, f.InsecureRegistries)
	err := finalize.Finalize(context.Background(), f.imageRef, finalize.Options{
		Insecure:               insecure,
		KeepBuildMetadataLabel: f.keepBuildMetadataLabel,
		Keychain:               f.keychain,
		Logger:                 cmd.DefaultLogger,
	})
	if err != nil {
		return cmd.FailErr(err, "finalize image")
	}
	return nil
}

// isInsecureRef reports whether the image reference's registry host matches one of
// the configured insecure registries (prefix match on host).
func isInsecureRef(imageRef string, insecureRegistries []string) bool {
	if len(insecureRegistries) == 0 {
		return false
	}
	host := registryHostFromRef(imageRef)
	for _, r := range insecureRegistries {
		if host == r || (len(r) > 0 && len(host) >= len(r) && host[:len(r)] == r) {
			return true
		}
	}
	return false
}

// registryHostFromRef extracts the registry host (everything before the first '/'
// when it looks like a host, i.e. contains '.' or ':' or is 'localhost').
func registryHostFromRef(imageRef string) string {
	for i := 0; i < len(imageRef); i++ {
		if imageRef[i] == '/' {
			candidate := imageRef[:i]
			if candidate == "localhost" || containsAny(candidate, ".:") {
				return candidate
			}
			return ""
		}
	}
	return ""
}

func containsAny(s, chars string) bool {
	for i := 0; i < len(s); i++ {
		for j := 0; j < len(chars); j++ {
			if s[i] == chars[j] {
				return true
			}
		}
	}
	return false
}
