package disk

import "github.com/stuffbucket/bladerunner/internal/config"

// ImageCacheDir returns the shared, content-addressed base-image cache:
// <DefaultStateDir>/cache/images. Shared across disks/slots (NOT per-VMDir),
// so the same qcow2 is downloaded once and reused instantly. Thin wrapper over
// config.ImageCacheDir (the single source of truth, also used by internal/vm
// to avoid a vm->disk import cycle).
func ImageCacheDir() string { return config.ImageCacheDir() }

// CachePath returns the content-addressed slot for a given downloaded-artifact
// SHA-256: <ImageCacheDir>/<sha256>.raw (the post-conversion raw image).
func CachePath(sha256hex string) string { return config.ImageCachePath(sha256hex) }
