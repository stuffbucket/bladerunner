package vm

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// ReadGuestImageVersion reads /etc/bladerunner-image-version from the running
// guest via SSH. The file is written at image-build time by
// scripts/build-guest-image.sh and contains a YYYY.MM.DD build date.
//
// Returns an empty string and a non-nil error when:
//   - the SSH connection fails (VM not ready, network down, key mismatch)
//   - the file doesn't exist (older base image without pre-baked marker)
//
// Callers (e.g. `runner status`) should treat any error as "unknown" and render
// a fallback string; the absence of a version is informational, not fatal.
func ReadGuestImageVersion(cfg *config.Config) (string, error) {
	if cfg.SSHConfigPath == "" {
		return "", fmt.Errorf("ssh config path not set")
	}

	// Hard cap the SSH probe at 10s so a stuck VM never wedges `runner status`.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	args := []string{
		"-F", cfg.SSHConfigPath,
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"bladerunner",
		"cat", config.GuestImageVersionPath,
	}

	cmd := exec.CommandContext(ctx, "ssh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("read guest image version: timeout")
		}
		return "", fmt.Errorf("read guest image version: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	version := strings.TrimSpace(stdout.String())
	if version == "" {
		return "", fmt.Errorf("guest image version file is empty")
	}
	return version, nil
}
