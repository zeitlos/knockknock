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

	// dataDir is where versions and symlinks are stored
	// e.g., /usr/local/lib/my-binary
	dataDir string

	// binPath is the path to the actual binary symlink users invoke
	// e.g., /usr/local/bin/my-binary
	binPath string

	socketPath string
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

	if config.VersionsDir == "" {
		return nil, fmt.Errorf("versions directory is required")
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
		dataDir:        filepath.Join(config.VersionsDir, config.BinaryName),
		binPath:        filepath.Join(config.BinaryDir, config.BinaryName),
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
		update = &latest
		return
	}

	return
}

func (s *Supervisor) Update(ctx context.Context, version string) error {
	versionsDir := filepath.Join(s.dataDir, "versions")

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

	currentLink := filepath.Join(s.dataDir, "current")

	// Backup existing current symlink if it exists
	if _, err := os.Lstat(currentLink); err == nil {
		timestamp := time.Now().Format("20060102-150405")
		backupLink := filepath.Join(s.dataDir, fmt.Sprintf("previous-%s", timestamp))

		target, err := os.Readlink(currentLink)
		if err != nil {
			return fmt.Errorf("failed to read current symlink: %w", err)
		}

		if err := os.Symlink(target, backupLink); err != nil {
			return fmt.Errorf("failed to create backup symlink: %w", err)
		}
	}

	// Atomically swap the current symlink to point to new version
	tempLink := filepath.Join(s.dataDir, fmt.Sprintf("current.tmp.%d", time.Now().Unix()))

	if err := os.Symlink(versionDir, tempLink); err != nil {
		return fmt.Errorf("failed to create temporary symlink: %w", err)
	}

	if err := os.Rename(tempLink, currentLink); err != nil {
		os.Remove(tempLink)
		return fmt.Errorf("failed to swap symlink: %w", err)
	}

	// Update the binary symlink in the bin directory
	if err := s.updateBinSymlink(); err != nil {
		return fmt.Errorf("failed to update bin symlink: %w", err)
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

// updateBinSymlink atomically updates the binary symlink in the bin directory
// to point to the current version's binary. If the existing binary is a regular
// file (legacy installation), it will be migrated to the versions directory.
func (s *Supervisor) updateBinSymlink() error {
	currentBinary := filepath.Join(s.dataDir, "current", s.config.BinaryName)

	if _, err := os.Stat(currentBinary); err != nil {
		return fmt.Errorf("current binary does not exist: %w", err)
	}

	// Check if existing binary is a regular file (legacy) or symlink
	if info, err := os.Lstat(s.binPath); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			// It's a regular file (legacy install) - migrate it to versions dir
			if err := s.migrateLegacyBinary(); err != nil {
				return fmt.Errorf("failed to migrate legacy binary: %w", err)
			}
		}
	}

	tempBinLink := fmt.Sprintf("%s.tmp.%d", s.binPath, time.Now().UnixNano())

	if err := os.Symlink(currentBinary, tempBinLink); err != nil {
		return fmt.Errorf("failed to create temp bin symlink: %w", err)
	}

	if err := os.Rename(tempBinLink, s.binPath); err != nil {
		os.Remove(tempBinLink)
		return fmt.Errorf("failed to swap bin symlink: %w", err)
	}

	return nil
}

// migrateLegacyBinary moves a legacy (non-symlink) binary installation into
// the versions directory so it can be rolled back to if needed.
func (s *Supervisor) migrateLegacyBinary() error {
	slog.Info("migrating legacy binary installation", "path", s.binPath)

	legacyVersionDir := filepath.Join(s.dataDir, "versions", "legacy")

	if err := os.MkdirAll(legacyVersionDir, 0755); err != nil {
		return fmt.Errorf("failed to create legacy version directory: %w", err)
	}

	legacyBinaryPath := filepath.Join(legacyVersionDir, s.config.BinaryName)

	if err := os.Rename(s.binPath, legacyBinaryPath); err != nil {
		return fmt.Errorf("failed to move legacy binary: %w", err)
	}

	// Create a backup symlink pointing to the legacy version
	timestamp := time.Now().Format("20060102-150405")
	backupLink := filepath.Join(s.dataDir, fmt.Sprintf("previous-%s", timestamp))

	if err := os.Symlink(legacyVersionDir, backupLink); err != nil {
		// Log but don't fail - the binary has been moved successfully
		slog.Warn("failed to create backup symlink for legacy version", "error", err)
	}

	slog.Info("legacy binary migrated successfully", "from", s.binPath, "to", legacyBinaryPath)

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

	currentLink := filepath.Join(s.dataDir, "current")
	tempLink := filepath.Join(s.dataDir, fmt.Sprintf("current.tmp.%d", time.Now().UnixNano()))

	if err := os.Symlink(target, tempLink); err != nil {
		return fmt.Errorf("failed to create temporary symlink: %w", err)
	}

	if err := os.Rename(tempLink, currentLink); err != nil {
		os.Remove(tempLink)
		return fmt.Errorf("failed to swap symlink: %w", err)
	}

	// Update the binary symlink in the bin directory
	if err := s.updateBinSymlink(); err != nil {
		return fmt.Errorf("failed to update bin symlink: %w", err)
	}

	if err := os.Remove(latestBackup); err != nil {
		slog.Warn("failed to remove backup symlink", "symlink", latestBackup, "error", err)
	}

	pid := os.Getpid()

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
			// Could be "legacy" version - handle specially
			if versionName == "legacy" {
				history = append(history, HistoricVersion{
					Version:       *semver.MustParse("0.0.0-legacy"),
					LastInstalled: parsePreviousTimestamp(filepath.Base(backup)),
				})
			}
			continue
		}

		history = append(history, HistoricVersion{
			Version:       *version,
			LastInstalled: parsePreviousTimestamp(filepath.Base(backup)),
		})
	}

	// Sort by last installed date in descending order (most recent first)
	sort.Slice(history, func(i, j int) bool {
		return history[i].LastInstalled.After(history[j].LastInstalled)
	})

	return history
}

// parsePreviousTimestamp extracts the timestamp from a backup symlink name
// (format: previous-20060102-150405)
func parsePreviousTimestamp(name string) time.Time {
	if len(name) > 9 && name[:9] == "previous-" {
		timestamp := name[9:]
		if t, err := time.Parse("20060102-150405", timestamp); err == nil {
			return t
		}
	}
	return time.Time{}
}

// getBackupSymlinks returns a sorted list of backup symlink paths
func (s *Supervisor) getBackupSymlinks() ([]string, error) {
	entries, err := os.ReadDir(s.dataDir)

	if err != nil {
		return nil, err
	}

	var backups []string
	for _, entry := range entries {
		// Look for symlinks named "previous-*"
		if entry.Type()&os.ModeSymlink != 0 {
			name := entry.Name()
			if len(name) > 9 && name[:9] == "previous-" {
				backups = append(backups, filepath.Join(s.dataDir, name))
			}
		}
	}

	sort.Strings(backups)

	return backups, nil
}

// cleanupOldBackups removes old backup symlinks, keeping only the most recent N
func (s *Supervisor) cleanupOldBackups(keep int) error {
	backups, err := s.getBackupSymlinks()
	if err != nil {
		return err
	}

	if len(backups) > keep {
		toRemove := backups[:len(backups)-keep]
		for _, backup := range toRemove {
			if err := os.Remove(backup); err != nil {
				slog.Warn("failed to remove old backup", "backup", backup, "error", err)
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
