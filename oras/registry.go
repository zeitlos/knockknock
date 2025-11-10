package oras

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/zeitlos/knockknock/config"

	"github.com/Masterminds/semver/v3"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

type Client struct {
	oras           *remote.Repository
	currentVersion *semver.Version

	config *config.Config
}

func NewClient(config *config.Config) (*Client, error) {
	repo, err := remote.NewRepository(config.Repo)

	if err != nil {
		return nil, fmt.Errorf("invalid repository: %w", err)
	}

	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})

	if err != nil {
		return nil, err
	}

	repo.Client = &auth.Client{
		Client:     retry.DefaultClient,
		Cache:      auth.NewCache(),
		Credential: credentials.Credential(store),
	}

	currentVersion, err := semver.NewVersion(config.Version)

	if err != nil {
		return nil, fmt.Errorf("invalid current version '%s': %w", config.Version, err)
	}

	return &Client{
		oras:           repo,
		currentVersion: currentVersion,

		config: config,
	}, nil
}

func (r *Client) Versions(ctx context.Context) ([]semver.Version, error) {
	var tags []string

	err := r.oras.Tags(ctx, "", func(tagsPage []string) error {
		tags = append(tags, tagsPage...)

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to list tags: %w", err)
	}

	var versions []semver.Version
	for _, tag := range tags {
		v, err := semver.NewVersion(tag)

		if err != nil {
			// Skip non-semver tags
			continue
		}

		versions = append(versions, *v)
	}

	return versions, nil
}

func (r *Client) CheckForUpdate(ctx context.Context) (update *semver.Version, allVersions []semver.Version, err error) {
	allVersions, err = r.Versions(ctx)

	if err != nil {
		return
	}

	if len(allVersions) == 0 {
		err = fmt.Errorf("no versions found in repository")
		return
	}

	latest := allVersions[len(allVersions)-1]

	if latest.GreaterThan(r.currentVersion) {
		// Update available
		update = &latest
		return
	}

	// No update available
	return
}

func (r *Client) DownloadUpdate(ctx context.Context, version, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create destination dir: %w", err)
	}

	fs, err := file.New(destDir)
	if err != nil {
		return fmt.Errorf("failed to create fs store: %w", err)
	}
	defer fs.Close()

	if _, err := oras.Copy(ctx, r.oras, version, fs, version, oras.DefaultCopyOptions); err != nil {
		return fmt.Errorf("failed to download version %s: %w", version, err)
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		return fmt.Errorf("failed to read destination directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if entry.Name() != r.config.BinaryName {
			continue
		}

		binaryPath := filepath.Join(destDir, entry.Name())

		if err := os.Chmod(binaryPath, 0755); err != nil {
			return fmt.Errorf("failed to chmod %s: %w", entry.Name(), err)
		}
	}

	return nil
}
