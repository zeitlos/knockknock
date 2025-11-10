package supervisor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/zeitlos/knockknock/config"
	"github.com/zeitlos/knockknock/oras"
)

type Supervisor struct {
	oras oras.Client

	currentVersion *semver.Version
	config         *config.Config
	basePath       string
	socketPath     string
}

type HistoricVersion struct {
	Version       semver.Version
	LastInstalled time.Time
}

const socketEnv = "KNOCKKNOCK_SOCKET"

func New(config *config.Config) (*Supervisor, error) {
	if config.BinaryName == "" {
		return nil, fmt.Errorf("binary name is required")
	}

	if config.Repo == "" {
		return nil, fmt.Errorf("repo is required")
	}

	if config.Version == "" {
		return nil, fmt.Errorf("version is required")
	}

	currentVersion, err := semver.NewVersion(config.Version)

	if err != nil {
		return nil, fmt.Errorf("invalid current version '%s': %w", config.Version, err)
	}

	oras, err := oras.NewClient(config)

	if err != nil {
		return nil, err
	}

	return &Supervisor{
		oras:           *oras,
		config:         config,
		currentVersion: currentVersion,
		basePath:       filepath.Join(config.InstallationDir, config.BinaryName),
		socketPath:     fmt.Sprintf("/tmp/knockknock-%d.sock", os.Getpid()),
	}, nil
}

func (s *Supervisor) CurrentVersion() *semver.Version {
	return s.currentVersion
}

func (s *Supervisor) CheckForUpdate(ctx context.Context) (update *semver.Version, allVersions []semver.Version, err error) {
	allVersions, err = s.oras.Versions(ctx)

	if err != nil {
		return
	}

	if len(allVersions) == 0 {
		err = fmt.Errorf("no versions found in repository")
		return
	}

	latest := allVersions[len(allVersions)-1]

	if latest.GreaterThan(s.currentVersion) {
		// Update available
		update = &latest
		return
	}

	// No update available
	return
}

func (s *Supervisor) Update(ctx context.Context, version string) error {
	versionsDir := filepath.Join(s.basePath, "versions")

	if err := os.MkdirAll(versionsDir, 0755); err != nil {
		return fmt.Errorf("failed to create versions directory: %w", err)
	}

	versionDir := filepath.Join(versionsDir, version)

	if err := os.MkdirAll(versionDir, 0755); err != nil {
		return fmt.Errorf("failed to create version directory: %w", err)
	}

	if err := s.oras.DownloadUpdate(ctx, version, versionDir); err != nil {
		return fmt.Errorf("failed to download version %s: %w", version, err)
	}

	binaryPath := filepath.Join(versionDir, s.config.BinaryName)

	if err := verifyBinary(binaryPath); err != nil {
		return fmt.Errorf("binary verification failed: %w", err)
	}

	currentLink := filepath.Join(s.basePath, "current")

	if _, err := os.Lstat(currentLink); err == nil {
		timestamp := time.Now().Format("20060102-150405")
		backupLink := filepath.Join(s.basePath, fmt.Sprintf("previous-%s", timestamp))

		target, err := os.Readlink(currentLink)

		if err != nil {
			return fmt.Errorf("failed to read current symlink: %w", err)
		}

		if err := os.Symlink(target, backupLink); err != nil {
			return fmt.Errorf("failed to create backup symlink: %w", err)
		}
	}

	tempLink := filepath.Join(s.basePath, fmt.Sprintf("current.tmp.%d", time.Now().Unix()))

	if err := os.Symlink(versionDir, tempLink); err != nil {
		return fmt.Errorf("failed to create temporary symlink: %w", err)
	}

	// Atomically replace the symlink
	if err := os.Rename(tempLink, currentLink); err != nil {
		os.Remove(tempLink) // Clean up temp link

		return fmt.Errorf("failed to swap symlink: %w", err)
	}

	if err := s.cleanupOldBackups(3); err != nil {
		slog.Warn("failed to cleanup old backups", "error", err)
	}

	// Kill the current process - systemd will restart it with the new version
	pid := os.Getpid()

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("failed to send termination signal: %w", err)
	}

	return nil
}

func (s *Supervisor) Rollback() error {
	backups, err := s.getBackupSymlinks()

	if err != nil {
		return fmt.Errorf("failed to find backup symlinks: %w", err)
	}

	if len(backups) == 0 {
		return fmt.Errorf("no backup symlinks found, cannot rollback")
	}

	// Get the most recent backup (last in sorted list)
	latestBackup := backups[len(backups)-1]

	target, err := os.Readlink(latestBackup)

	if err != nil {
		return fmt.Errorf("failed to read backup symlink: %w", err)
	}

	binaryPath := filepath.Join(target, s.config.BinaryName)

	if err := verifyBinary(binaryPath); err != nil {
		return fmt.Errorf("backup version binary verification failed: %w", err)
	}

	currentLink := filepath.Join(s.basePath, "current")
	tempLink := filepath.Join(s.basePath, fmt.Sprintf("current.tmp.%d", time.Now().Unix()))

	if err := os.Symlink(target, tempLink); err != nil {
		return fmt.Errorf("failed to create temporary symlink: %w", err)
	}

	// Atomically replace the symlink
	if err := os.Rename(tempLink, currentLink); err != nil {
		os.Remove(tempLink)
		return fmt.Errorf("failed to swap symlink: %w", err)
	}

	if err := os.Remove(latestBackup); err != nil {
		// Log but don't fail the rollback
		slog.Warn("failed to remove backup symlink", "symlink", latestBackup, "error", err)
	}

	pid := os.Getpid()

	// Kill the current process - systemd will restart it with the rolled-back version
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("failed to send termination signal: %w", err)
	}

	return nil
}

func (s *Supervisor) History() []HistoricVersion {
	backups, err := s.getBackupSymlinks()
	if err != nil {
		return []HistoricVersion{}
	}

	var history []HistoricVersion

	for _, backup := range backups {
		target, err := os.Readlink(backup)

		if err != nil {
			continue
		}

		versionName := filepath.Base(target)
		version, err := semver.NewVersion(versionName)

		if err != nil {
			continue
		}

		// Parse timestamp from backup symlink name (format: previous-20060102-150405)
		backupName := filepath.Base(backup)
		var lastInstalled time.Time

		if len(backupName) > 9 && backupName[:9] == "previous-" {
			timestamp := backupName[9:]
			lastInstalled, err = time.Parse("20060102-150405", timestamp)

			if err != nil {
				lastInstalled = time.Time{}
			}
		}

		history = append(history, HistoricVersion{
			Version:       *version,
			LastInstalled: lastInstalled,
		})
	}

	// Sort by last installed date in descending order (most recent first)
	sort.Slice(history, func(i, j int) bool {
		return history[i].LastInstalled.After(history[j].LastInstalled)
	})

	return history
}

// getBackupSymlinks returns a sorted list of backup symlink paths
func (s *Supervisor) getBackupSymlinks() ([]string, error) {
	entries, err := os.ReadDir(s.basePath)
	if err != nil {
		return nil, err
	}

	var backups []string
	for _, entry := range entries {
		// Look for symlinks named "previous-*"
		if entry.Type()&os.ModeSymlink != 0 {
			name := entry.Name()
			if len(name) > 9 && name[:9] == "previous-" {
				backups = append(backups, filepath.Join(s.basePath, name))
			}
		}
	}

	// Sort by name (which includes timestamp)
	sort.Strings(backups)

	return backups, nil
}

// cleanupOldBackups removes old backup symlinks, keeping only the most recent N
func (s *Supervisor) cleanupOldBackups(keep int) error {
	backups, err := s.getBackupSymlinks()
	if err != nil {
		return err
	}

	// If we have more backups than we want to keep, remove the oldest ones
	if len(backups) > keep {
		toRemove := backups[:len(backups)-keep]
		for _, backup := range toRemove {
			if err := os.Remove(backup); err != nil {
				// Log but continue
				slog.Warn("failed to remove old backup %s: %v\n", backup, err)
			}
		}
	}

	return nil
}

// verifyBinary performs basic verification that the binary is valid
func verifyBinary(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("binary not found: %w", err)
	}

	if info.Size() == 0 {
		return fmt.Errorf("binary is empty")
	}

	if info.Mode()&0111 == 0 {
		return fmt.Errorf("binary is not executable")
	}

	// Check if it's a valid ELF binary (basic check)
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open binary: %w", err)
	}
	defer file.Close()

	// Read ELF magic number
	magic := make([]byte, 4)
	if _, err := io.ReadFull(file, magic); err != nil {
		return fmt.Errorf("failed to read binary header: %w", err)
	}

	// Check for ELF magic number (0x7F 'E' 'L' 'F')
	if magic[0] != 0x7F || magic[1] != 'E' || magic[2] != 'L' || magic[3] != 'F' {
		return fmt.Errorf("binary is not a valid ELF file")
	}

	return nil
}
