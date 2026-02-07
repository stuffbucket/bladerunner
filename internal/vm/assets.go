package vm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

func ensureVMDir(cfg *config.Config) error {
	start := time.Now()
	if err := os.MkdirAll(cfg.VMDir, 0o755); err != nil {
		return fmt.Errorf("create vm directory %s: %w", cfg.VMDir, err)
	}
	logging.L().Info("ensured VM directory", "path", cfg.VMDir, "elapsed", time.Since(start).Round(time.Millisecond).String())
	return nil
}

func ensureBaseImage(ctx context.Context, cfg *config.Config) (string, error) {
	if cfg.BaseImagePath != "" {
		if !fileExists(cfg.BaseImagePath) {
			return "", fmt.Errorf("base image path does not exist: %s", cfg.BaseImagePath)
		}
		if err := ensureRawDiskImage(cfg.BaseImagePath); err != nil {
			return "", err
		}
		logging.L().Info("using provided base image", "path", cfg.BaseImagePath)
		return cfg.BaseImagePath, nil
	}

	path := filepath.Join(cfg.VMDir, "base-image.raw")
	if fileExists(path) {
		if err := ensureRawDiskImage(path); err != nil {
			return "", err
		}
		logging.L().Info("using cached base image", "path", path)
		return path, nil
	}

	if cfg.BaseImageURL == "" {
		return "", fmt.Errorf("base image url is empty")
	}

	logging.L().Info("downloading base image", "url", cfg.BaseImageURL, "destination", path)
	if err := downloadFile(ctx, cfg.BaseImageURL, path); err != nil {
		return "", err
	}

	if err := ensureRawDiskImage(path); err != nil {
		return "", err
	}

	logging.L().Info("downloaded base image", "path", path)
	return path, nil
}

func ensureMainDisk(cfg *config.Config, baseImagePath string) error {
	if fileExists(cfg.DiskPath) {
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

func ensureRawDiskImage(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open disk image: %w", err)
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(f, header); err == nil {
		if string(header) == "QFI\xfb" {
			_ = f.Close()
			logging.L().Info("qcow2 image detected, converting to raw format", "path", path)
			if err := convertQcow2ToRaw(path); err != nil {
				return fmt.Errorf("convert qcow2 to raw: %w", err)
			}
			logging.L().Info("conversion complete", "path", path)
			return nil
		}
	}
	_ = f.Close()
	return nil
}

func convertQcow2ToRaw(qcow2Path string) error {
	start := time.Now()

	// Check if qemu-img is available
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return fmt.Errorf("qemu-img not found in PATH (install with: brew install qemu): %w", err)
	}

	rawPath := qcow2Path + ".raw"
	logging.L().Info("converting disk image", "from", qcow2Path, "to", rawPath)

	cmd := exec.Command("qemu-img", "convert", "-f", "qcow2", "-O", "raw", qcow2Path, rawPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert failed: %w: %s", err, string(output))
	}

	// Replace original with converted image
	if err := os.Remove(qcow2Path); err != nil {
		logging.L().Warn("failed to remove qcow2 file", "path", qcow2Path, "err", err)
	}
	if err := os.Rename(rawPath, qcow2Path); err != nil {
		return fmt.Errorf("rename converted image: %w", err)
	}

	logging.L().Info("qcow2 to raw conversion complete", "path", qcow2Path, "elapsed", time.Since(start).Round(time.Millisecond).String())
	return nil
}

func downloadFile(ctx context.Context, url, path string) error {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download base image: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download base image failed: %s", resp.Status)
	}

	tmpPath := path + ".tmp"
	_ = os.Remove(tmpPath)

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create temp image file: %w", err)
	}

	progress := logging.NewByteProgress("Downloading base image", resp.ContentLength)
	if _, err := io.Copy(f, io.TeeReader(resp.Body, progress)); err != nil {
		progress.Fail(err)
		f.Close()
		return fmt.Errorf("write image to disk: %w", err)
	}
	progress.Finish()
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp image file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("move downloaded image into place: %w", err)
	}
	logging.L().Info("download complete", "url", url, "path", path, "elapsed", time.Since(start).Round(time.Millisecond).String())
	return nil
}
