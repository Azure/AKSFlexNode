// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package oci

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	cplatforms "github.com/containerd/platforms"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/umoci"
	"github.com/opencontainers/umoci/oci/casext"
	"github.com/opencontainers/umoci/oci/layer"

	"github.com/Azure/unbounded/internal/ociutil"
)

func init() {
	ociutil.RegisterDockerParsers()
}

// unpackOCILayout opens an OCI image layout at layoutDir and unpacks the image
// tagged with the given tag into destDir. It uses umoci for spec-compliant
// layer extraction including whiteout processing, xattr handling, and uid/gid
// mapping.
func unpackOCILayout(ctx context.Context, log *slog.Logger, hostArch, layoutDir, tag, destDir string) error {
	engine, err := umoci.OpenLayout(layoutDir)
	if err != nil {
		return fmt.Errorf("open OCI layout %q: %w", layoutDir, err)
	}
	defer engine.Close() //nolint:errcheck // best effort close

	descriptorPaths, err := engine.ResolveReference(ctx, tag) // FIXME: can we move the validation to oras side?
	if err != nil {
		return fmt.Errorf("resolve tag %q: %w", tag, err)
	}

	if len(descriptorPaths) == 0 {
		return fmt.Errorf("tag %q not found in OCI layout", tag)
	}

	// Select the descriptor matching the current platform. For single-platform
	// images there will be exactly one result. For multi-platform (OCI index)
	// images, ResolveReference walks through the index and returns one
	// DescriptorPath per platform manifest — we must pick the right one.
	dp, err := selectPlatformDescriptor(hostArch, descriptorPaths)
	if err != nil {
		return fmt.Errorf("select platform for tag %q: %w", tag, err)
	}

	// Fetch and parse the manifest from the CAS.
	blob, err := engine.FromDescriptor(ctx, dp.Descriptor())
	if err != nil {
		return fmt.Errorf("read manifest blob for tag %q: %w", tag, err)
	}
	defer blob.Close() //nolint:errcheck // best effort close

	manifest, ok := blob.Data.(ispec.Manifest)
	if !ok {
		return fmt.Errorf("tag %q does not point to an OCI manifest (got %T)", tag, blob.Data)
	}

	// Convert Docker media types to OCI equivalents so that umoci's strict
	// media-type checks pass. Docker V2 images use different MIME types for
	// the config and layer blobs but are structurally identical to OCI.
	ociutil.ConvertDockerMediaTypes(&manifest)

	log.Info("unpacking OCI image layers",
		slog.Int("layers", len(manifest.Layers)),
		slog.String("tag", tag),
		slog.String("dest", destDir))

	unpackOpts := &layer.UnpackOptions{
		OnDiskFormat: layer.DirRootfs{
			MapOptions: layer.MapOptions{
				Rootless: true,
			},
		},
	}

	if err := layer.UnpackRootfs(ctx, engine, destDir, manifest, unpackOpts); err != nil {
		return fmt.Errorf("unpack rootfs: %w", err)
	}

	return nil
}

func selectPlatformDescriptor(hostArch string, paths []casext.DescriptorPath) (casext.DescriptorPath, error) {
	want := cplatforms.Normalize(ispec.Platform{
		OS:           "linux",
		Architecture: hostArch,
	})
	m := cplatforms.NewMatcher(want)

	var checked []string

	for _, dp := range paths {
		// Walk through the descriptor path entries — the index child
		// descriptors carry Platform metadata that we can match against.
		for _, step := range dp.Walk {
			if step.Platform == nil {
				continue
			}

			if m.Match(*step.Platform) {
				return dp, nil
			}

			checked = append(checked, fmt.Sprintf("%s/%s", step.Platform.OS, step.Platform.Architecture))
		}
	}

	// Single-platform images pushed without a manifest index have no
	// platform metadata in the descriptor walk.  If there is exactly one
	// descriptor and we found no platform annotations at all, assume the
	// image matches (the common single-arch case).
	if len(paths) == 1 && len(checked) == 0 {
		return paths[0], nil
	}

	err := fmt.Errorf(
		"no manifest found for platform linux/%s, available %q",
		hostArch,
		strings.Join(checked, ","),
	)

	return casext.DescriptorPath{}, err
}
