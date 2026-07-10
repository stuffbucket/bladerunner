//go:build darwin

package vm

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/util"
)

// ensureVMDir and ensureMainDisk prepare the on-disk VM working set. They are
// only invoked by the darwin VM runner (runner_darwin.go); on other platforms
// the VM runner is an unsupported stub, so they live in this darwin-tagged file
// to keep them out of those builds.

func ensureVMDir(cfg *config.Config) error {
	start := time.Now()
	if err := os.MkdirAll(cfg.VMDir, 0o755); err != nil {
		return fmt.Errorf("create vm directory %s: %w", cfg.VMDir, err)
	}
	logging.L().Info("ensured VM directory", "path", cfg.VMDir, "elapsed", time.Since(start).Round(time.Millisecond).String())
	return nil
}

func ensureMainDisk(cfg *config.Config, baseImagePath string) error {
	if util.FileExists(cfg.DiskPath) {
		logging.L().Info("reusing existing VM disk", "path", cfg.DiskPath)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(cfg.DiskPath), 0o755); err != nil {
		return fmt.Errorf("create disk parent: %w", err)
	}

	// Copy base image to disk location.
	in, err := os.Open(baseImagePath)
	if err != nil {
		return fmt.Errorf("open base image: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(cfg.DiskPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create disk image: %w", err)
	}

	sourceInfo, _ := in.Stat()
	sourceSize := int64(0)
	if sourceInfo != nil {
		sourceSize = sourceInfo.Size()
	}
	progress := logging.NewByteProgress("Creating main disk", sourceSize)
	_, err = io.Copy(out, io.TeeReader(in, progress))
	if err != nil {
		progress.Fail(err)
		return fmt.Errorf("copy base image to disk: %w", err)
	}
	progress.Finish()
	if err := out.Close(); err != nil {
		return fmt.Errorf("close disk image: %w", err)
	}

	// Use qemu-img to resize the disk. This correctly updates the GPT backup
	// header and avoids corrupting the partition table (unlike raw truncate).
	targetSize := fmt.Sprintf("%dG", cfg.DiskSizeGiB)
	logging.L().Info("resizing disk image", "path", cfg.DiskPath, "target", targetSize)
	cmd := exec.Command("qemu-img", "resize", "-f", "raw", cfg.DiskPath, targetSize)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img resize failed: %w: %s", err, string(output))
	}

	logging.L().Info("created VM disk image", "path", cfg.DiskPath, "size", targetSize)
	return nil
}
