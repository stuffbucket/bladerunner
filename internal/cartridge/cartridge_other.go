//go:build !darwin

package cartridge

// On non-darwin hosts there is no hdiutil and no Virtualization.framework, so
// hostSupported() reports false and every hdiutil-backed cartridge operation
// (defined in cartridge.go) returns ErrUnsupported. These helpers keep the
// platform-neutral workers referenced on Linux CI, avoiding an unused-code trap,
// while never actually invoking hdiutil.
//
// Note that the layout and format-version half of the package (version.go) is
// deliberately NOT gated: it is plain file I/O over an already-mounted
// directory, so Verify/ReadMetadata/WriteMetadata behave identically on every
// platform and stay unit-testable in Linux CI.

// hostSupported reports that cartridges are unavailable off macOS.
func hostSupported() bool { return false }

// isMountpoint is unreachable off macOS because the public entry points
// short-circuit on hostSupported(); it exists solely to satisfy the neutral
// isAttached helper.
func isMountpoint(_ string) bool { return false }

// lookupMount is unreachable off macOS for the same reason; statfs mount
// identity is a darwin concern. It reports ErrUnsupported so any future caller
// that forgets the hostSupported() gate fails loudly rather than silently
// treating an unknown volume as a cartridge.
func lookupMount(_ string) (MountInfo, error) { return MountInfo{}, ErrUnsupported }
