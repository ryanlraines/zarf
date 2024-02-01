// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2021-Present The Zarf Authors

// Package oci contains functions for interacting with Zarf packages stored in OCI registries.
package oci

import (
	"context"
	"os"
	"path/filepath"

	"slices"

	"github.com/defenseunicorns/zarf/src/pkg/utils/helpers"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/file"
)

// FileDescriptorExists returns true if the given file exists in the given directory with the expected SHA.
func (o *OrasRemote) FileDescriptorExists(desc ocispec.Descriptor, destinationDir string) bool {
	rel := desc.Annotations[ocispec.AnnotationTitle]
	destinationPath := filepath.Join(destinationDir, rel)

	info, err := os.Stat(destinationPath)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}
	if info.Size() != desc.Size {
		return false
	}

	f, err := os.Open(destinationPath)
	if err != nil {
		return false
	}
	defer f.Close()

	actual, err := helpers.GetSHA256Hash(f)
	if err != nil {
		return false
	}
	return actual == desc.Digest.Encoded()
}

// PullLayers pulls the package from the remote repository and saves it to the given path.
// If you don't have nil paramaters for doneSaving, encounteredErr, and wg
// you must use the channels in a go routine and call wg.done after they are used
func (o *OrasRemote) PullLayers(ctx context.Context, destinationDir string, concurrency int,
	layersToPull []ocispec.Descriptor, doneSaving chan error) ([]ocispec.Descriptor, error) {
	// de-duplicate layers
	layersToPull = RemoveDuplicateDescriptors(layersToPull)

	dst, err := file.New(destinationDir)
	if err != nil {
		return nil, err
	}
	defer dst.Close()

	copyOpts := o.CopyOpts
	copyOpts.Concurrency = concurrency

	return layersToPull, o.CopyWithProgress(ctx, layersToPull, dst, copyOpts, doneSaving)
}

// CopyWithProgress copies the given layers from the remote repository to the given store.
func (o *OrasRemote) CopyWithProgress(ctx context.Context, layers []ocispec.Descriptor, store oras.Target,
	copyOpts oras.CopyOptions, doneSaving chan error) error {
	estimatedBytes := int64(0)
	shas := []string{}
	for _, layer := range layers {
		estimatedBytes += layer.Size
		if len(layer.Digest.String()) > 0 {
			shas = append(shas, layer.Digest.Encoded())
		}
	}

	if copyOpts.FindSuccessors == nil {
		copyOpts.FindSuccessors = func(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
			nodes, err := content.Successors(ctx, fetcher, desc)
			if err != nil {
				return nil, err
			}
			if desc.MediaType == ocispec.MediaTypeImageIndex {
				manifestDescs := nodes
				nodes = []ocispec.Descriptor{}
				// expand the manifests
				for _, node := range manifestDescs {
					manifest, err := o.FetchManifest(ctx, node)
					if err != nil {
						return nil, err
					}
					nodes = append(nodes, manifest.Layers...)
					nodes = append(nodes, manifest.Config)
				}
			}

			var ret []ocispec.Descriptor
			for _, node := range nodes {
				if slices.Contains(shas, node.Digest.Encoded()) {
					ret = append(ret, node)
				}
			}
			return ret, nil
		}
	}

	_, err := oras.Copy(ctx, o.repo, o.repo.Reference.String(), store, o.repo.Reference.String(), copyOpts)
	if err != nil {
		if doneSaving != nil {
			doneSaving <- err
			<-doneSaving
		}
		return err
	}

	// Send a signal to the progress bar that we're done and wait for it to finish
	if doneSaving != nil {
		doneSaving <- nil
		<-doneSaving
	}

	return nil
}

// PullLayer pulls a layer from the remote repository and saves it to `destinationDir/annotationTitle`.
// ?! Why do we pull a single layer with o.fetchLayer, but multiple layers with oras.copy
func (o *OrasRemote) PullLayer(ctx context.Context, desc ocispec.Descriptor, destinationDir string) error {
	b, err := o.FetchLayer(ctx, desc)
	if err != nil {
		return err
	}

	rel := desc.Annotations[ocispec.AnnotationTitle]

	return helpers.WriteFile(filepath.Join(destinationDir, rel), b)
}

// PullFilesAtPaths pulls multiple files from the remote repository and saves them to `destinationDir`.
func (o *OrasRemote) PullFilesAtPaths(ctx context.Context, paths []string, destinationDir string) ([]ocispec.Descriptor, error) {
	paths = helpers.Unique(paths)
	root, err := o.FetchRoot(ctx)
	if err != nil {
		return nil, err
	}
	layersPulled := []ocispec.Descriptor{}
	for _, path := range paths {
		desc := root.Locate(path)
		if !IsEmptyDescriptor(desc) {
			layersPulled = append(layersPulled, desc)
			if o.FileDescriptorExists(desc, destinationDir) {
				continue
			}
			err = o.PullLayer(ctx, desc, destinationDir)
			if err != nil {
				return nil, err
			}
		}
	}
	return layersPulled, nil
}
