//go:build darwin

package vm

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
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

// freeSpaceBytes returns the bytes available to an unprivileged writer at path
// (or its nearest existing parent, since the target file may not exist yet).
func freeSpaceBytes(path string) (int64, error) {
	probe := path
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(probe, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", probe, err)
	}
	// Bavail is blocks available to non-superuser; Bsize is the fundamental
	// block size — their product is the free bytes we can actually write.
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func ensureMainDisk(cfg *config.Config, baseImagePath string) error {
	if util.FileExists(cfg.DiskPath) {
		// Gate the existing disk: a truncated/corrupt disk (from a prior
		// interrupted copy/resize) is rebuilt rather than silently booted.
		if err := verifyImageIntegrity(cfg.DiskPath); err != nil {
			logging.L().Warn("existing VM disk failed integrity check; rebuilding",
				"path", cfg.DiskPath, "err", err)
			_ = os.Remove(cfg.DiskPath)
		} else {
			logging.L().Info("reusing existing VM disk", "path", cfg.DiskPath)
			return nil
		}
	}

	// qemu-img is required for the resize below; surface a clear install hint now
	// rather than a cryptic "no such file" after the copy has already run.
	if err := RequireQemuImg(); err != nil {
		return err
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

	sourceInfo, _ := in.Stat()
	sourceSize := int64(0)
	if sourceInfo != nil {
		sourceSize = sourceInfo.Size()
	}

	// Pre-flight the free space before writing anything: an ENOSPC mid-copy or
	// mid-resize otherwise surfaces as an opaque IO error, and a sparse resize
	// that over-commits fails later at guest runtime. Require headroom for the
	// full logical disk (the base copy plus the resize target). Skip when the
	// requested disk is smaller than the base — resize can't shrink below it and
	// the copy dominates.
	needBytes := max(int64(cfg.DiskSizeGiB)*bytesPerGiB, sourceSize)
	avail, spaceErr := freeSpaceBytes(cfg.DiskPath)
	if spaceErr != nil {
		logging.L().Warn("could not determine free disk space; skipping preflight", "err", spaceErr)
	} else if err := checkDiskSpace(needBytes, avail, cfg.DiskPath); err != nil {
		return err
	}

	out, err := os.OpenFile(cfg.DiskPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create disk image: %w", err)
	}

	progress := logging.NewByteProgress("Creating main disk", sourceSize)
	_, err = io.Copy(out, io.TeeReader(in, progress))
	if err != nil {
		progress.Fail(err)
		_ = out.Close()
		_ = os.Remove(cfg.DiskPath)
		return fmt.Errorf("copy base image to disk: %w", err)
	}
	progress.Finish()
	if err := out.Close(); err != nil {
		_ = os.Remove(cfg.DiskPath)
		return fmt.Errorf("close disk image: %w", err)
	}

	// Use qemu-img to resize the disk. This correctly updates the GPT backup
	// header and avoids corrupting the partition table (unlike raw truncate).
	targetSize := fmt.Sprintf("%dG", cfg.DiskSizeGiB)
	logging.L().Info("resizing disk image", "path", cfg.DiskPath, "target", targetSize)
	cmd := exec.Command("qemu-img", "resize", "-f", "raw", cfg.DiskPath, targetSize)
	if output, err := cmd.CombinedOutput(); err != nil {
		// A half-resized disk left in place would fail the integrity gate on the
		// next boot; remove it now so the disk is rebuilt clean.
		_ = os.Remove(cfg.DiskPath)
		return fmt.Errorf("qemu-img resize failed: %w: %s", err, string(output))
	}

	logging.L().Info("created VM disk image", "path", cfg.DiskPath, "size", targetSize)
	return nil
}
