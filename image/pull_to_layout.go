package image

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/buildpacks/imgutil/layout"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// PullToLayout pulls an image from a registry and saves it to the layout directory
// in the format expected by the lifecycle's layout mode.
// This enables buildkit-based builds where the run image isn't pre-populated
// in the layout directory by the platform (pack).
func PullToLayout(keychain authn.Keychain, imageRef string, layoutDir string, insecureRegistries []string) error {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("parsing image reference %q: %w", imageRef, err)
	}

	// Determine the target path in the layout directory
	targetPath, err := layout.ParseRefToPath(imageRef)
	if err != nil {
		return fmt.Errorf("parsing layout path for %q: %w", imageRef, err)
	}
	fullPath := filepath.Join(layoutDir, targetPath)

	// Check if already exists
	if _, err := os.Stat(fullPath); err == nil {
		return nil // already present
	}

	// Set up auth options
	opts := []remote.Option{remote.WithAuthFromKeychain(keychain)}

	// Pull the image
	img, err := remote.Image(ref, opts...)
	if err != nil {
		return fmt.Errorf("pulling image %q: %w", imageRef, err)
	}

	// Save to layout directory using imgutil/layout
	if err := os.MkdirAll(fullPath, 0777); err != nil {
		return fmt.Errorf("creating layout directory %q: %w", fullPath, err)
	}

	layoutPath, err := layout.Write(fullPath, nil)
	if err != nil {
		return fmt.Errorf("initializing layout at %q: %w", fullPath, err)
	}

	if err := layoutPath.AppendImage(img); err != nil {
		return fmt.Errorf("writing image to layout at %q: %w", fullPath, err)
	}

	return nil
}
