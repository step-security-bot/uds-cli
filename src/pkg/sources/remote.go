// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2023-Present The UDS Authors

// Package sources contains Zarf packager sources
package sources

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/defenseunicorns/zarf/src/pkg/layout"
	"github.com/defenseunicorns/zarf/src/pkg/message"
	"github.com/defenseunicorns/zarf/src/pkg/oci"
	"github.com/defenseunicorns/zarf/src/pkg/packager/sources"
	zarfUtils "github.com/defenseunicorns/zarf/src/pkg/utils"
	zarfTypes "github.com/defenseunicorns/zarf/src/types"
	goyaml "github.com/goccy/go-yaml"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"

	"github.com/defenseunicorns/uds-cli/src/config"
	"github.com/defenseunicorns/uds-cli/src/pkg/cache"
	"github.com/defenseunicorns/uds-cli/src/pkg/utils"
)

// RemoteBundle is a package source for remote bundles that implements Zarf's packager.PackageSource
type RemoteBundle struct {
	PkgName        string
	PkgOpts        *zarfTypes.ZarfPackageOptions
	PkgManifestSHA string
	TmpDir         string
	Remote         *oci.OrasRemote
	isPartial      bool
}

// LoadPackage loads a Zarf package from a remote bundle
func (r *RemoteBundle) LoadPackage(dst *layout.PackagePaths, unarchiveAll bool) error {
	layers, err := r.downloadPkgFromRemoteBundle()
	if err != nil {
		return err
	}

	var pkg zarfTypes.ZarfPackage
	if err = zarfUtils.ReadYaml(dst.ZarfYAML, &pkg); err != nil {
		return err
	}

	dst.SetFromLayers(layers)

	err = sources.ValidatePackageIntegrity(dst, pkg.Metadata.AggregateChecksum, r.isPartial)
	if err != nil {
		return err
	}

	if unarchiveAll {
		for _, component := range pkg.Components {
			if err := dst.Components.Unarchive(component); err != nil {
				if layout.IsNotLoaded(err) {
					_, err := dst.Components.Create(component)
					if err != nil {
						return err
					}
				} else {
					return err
				}
			}
		}

		if dst.SBOMs.Path != "" {
			if err := dst.SBOMs.Unarchive(); err != nil {
				return err
			}
		}
	}
	return nil
}

// LoadPackageMetadata loads a Zarf package's metadata from a remote bundle
func (r *RemoteBundle) LoadPackageMetadata(dst *layout.PackagePaths, _ bool, _ bool) (err error) {
	root, err := r.Remote.FetchRoot()
	if err != nil {
		return err
	}
	pkgManifestDesc := root.Locate(r.PkgManifestSHA)
	if oci.IsEmptyDescriptor(pkgManifestDesc) {
		return fmt.Errorf("zarf package %s with manifest sha %s not found", r.PkgName, r.PkgManifestSHA)
	}

	// look at Zarf pkg manifest, grab zarf.yaml desc and download it
	pkgManifest, err := r.Remote.FetchManifest(pkgManifestDesc)
	var zarfYAMLDesc ocispec.Descriptor
	for _, layer := range pkgManifest.Layers {
		if layer.Annotations[ocispec.AnnotationTitle] == config.ZarfYAML {
			zarfYAMLDesc = layer
			break
		}
	}
	zarfYAMLBytes, err := r.Remote.FetchLayer(zarfYAMLDesc)
	if err != nil {
		return err
	}
	var zarfYAML zarfTypes.ZarfPackage
	if err = goyaml.Unmarshal(zarfYAMLBytes, &zarfYAML); err != nil {
		return err
	}
	err = zarfUtils.WriteYaml(filepath.Join(dst.Base, config.ZarfYAML), zarfYAML, 0644)

	// grab checksums.txt so we can validate pkg integrity
	var checksumLayer ocispec.Descriptor
	for _, layer := range pkgManifest.Layers {
		if layer.Annotations[ocispec.AnnotationTitle] == config.ChecksumsTxt {
			checksumBytes, err := r.Remote.FetchLayer(layer)
			if err != nil {
				return err
			}
			err = os.WriteFile(filepath.Join(dst.Base, config.ChecksumsTxt), checksumBytes, 0644)
			if err != nil {
				return err
			}
			checksumLayer = layer
			break
		}
	}

	dst.SetFromLayers([]ocispec.Descriptor{pkgManifestDesc, checksumLayer})

	err = sources.ValidatePackageIntegrity(dst, zarfYAML.Metadata.AggregateChecksum, true)
	return err
}

// Collect doesn't need to be implemented
func (r *RemoteBundle) Collect(_ string) (string, error) {
	return "", fmt.Errorf("not implemented in %T", r)
}

// downloadPkgFromRemoteBundle downloads a Zarf package from a remote bundle
func (r *RemoteBundle) downloadPkgFromRemoteBundle() ([]ocispec.Descriptor, error) {
	rootManifest, err := r.Remote.FetchRoot()
	if err != nil {
		return nil, err
	}

	pkgManifestDesc := rootManifest.Locate(r.PkgManifestSHA)
	if oci.IsEmptyDescriptor(pkgManifestDesc) {
		return nil, fmt.Errorf("package %s does not exist in this bundle", r.PkgManifestSHA)
	}
	// hack Zarf media type so that FetchManifest works
	pkgManifestDesc.MediaType = oci.ZarfLayerMediaTypeBlob
	pkgManifest, err := r.Remote.FetchManifest(pkgManifestDesc)
	if err != nil || pkgManifest == nil {
		return nil, err
	}

	// only fetch layers that exist in the remote as optional ones might not exist
	// todo: this is incredibly slow; maybe keep track of layers in bundle metadata instead of having to query the remote?
	progressBar := message.NewProgressBar(int64(len(pkgManifest.Layers)), fmt.Sprintf("Verifying layers in Zarf package: %s", r.PkgName))
	estimatedBytes := int64(0)
	layersToPull := []ocispec.Descriptor{pkgManifestDesc}
	layersInBundle := []ocispec.Descriptor{pkgManifestDesc}

	for _, layer := range pkgManifest.Layers {
		ok, err := r.Remote.Repo().Blobs().Exists(context.TODO(), layer)
		if err != nil {
			return nil, err
		}
		progressBar.Add(1)
		if ok {
			estimatedBytes += layer.Size
			layersInBundle = append(layersInBundle, layer)
			digest := layer.Digest.Encoded()
			if strings.Contains(layer.Annotations[ocispec.AnnotationTitle], config.BlobsDir) && cache.Exists(digest) {
				dst := filepath.Join(r.TmpDir, "images", config.BlobsDir)
				err = cache.Use(digest, dst)
				if err != nil {
					return nil, err
				}
			} else {
				layersToPull = append(layersToPull, layer)
			}

		}
	}
	progressBar.Successf("Verified %s package", r.PkgName)

	store, err := file.New(r.TmpDir)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	// copy zarf pkg to local store
	copyOpts := utils.CreateCopyOpts(layersToPull, config.CommonOptions.OCIConcurrency)
	doneSaving := make(chan int)
	errChan := make(chan int)
	var wg sync.WaitGroup
	wg.Add(1)
	go zarfUtils.RenderProgressBarForLocalDirWrite(r.TmpDir, estimatedBytes, &wg, doneSaving, errChan, fmt.Sprintf("Pulling bundled Zarf pkg: %s", r.PkgName), fmt.Sprintf("Successfully pulled package: %s", r.PkgName))
	_, err = oras.Copy(context.TODO(), r.Remote.Repo(), r.Remote.Repo().Reference.String(), store, "", copyOpts)
	if err != nil {
		errChan <- 1
		return nil, err
	}
	doneSaving <- 1
	wg.Wait()

	if len(pkgManifest.Layers) > len(layersInBundle) {
		r.isPartial = true
	}
	return layersInBundle, nil
}
