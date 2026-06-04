//go:build !darwin

package cartridge

// On non-darwin hosts there is no hdiutil and no Virtualization.framework, so
// hostSupported() reports false and every public cartridge operation (defined
// in cartridge.go) returns ErrUnsupported. These two helpers keep the
// platform-neutral workers referenced on Linux CI, avoiding an unused-code trap,
// while never actually invoking hdiutil.

// hostSupported reports that cartridges are unavailable off macOS.
func hostSupported() bool { return false }

// isMountpoint is unreachable off macOS because IsAttached short-circuits on
// hostSupported(); it exists solely to satisfy the neutral isAttached helper.
func isMountpoint(_ string) bool { return false }
