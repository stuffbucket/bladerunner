package util

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// FileSHA256 returns the hex SHA-256 of a file's contents.
//
// It lives here because two packages needed it independently — the guest image
// bake, to record what it built, and the VM's base-image cache, to check what
// it downloaded — and a digest is not the business of either. Streaming rather
// than reading the file in matters: these are qcow2 images of several hundred
// megabytes.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s for sha256: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
