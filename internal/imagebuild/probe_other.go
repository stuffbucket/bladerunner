//go:build !linux && !darwin

package imagebuild

import "context"

// This file keeps the package building on platforms with no mechanic at all —
// notably Windows, where the supported route is WSL2, which is Linux and so
// compiles the linux file instead. Every capability is false, so policy reports
// an unsupported platform rather than choosing something that cannot run.

// nativeAttachAvailable reports no block-device attach on an unsupported platform.
func nativeAttachAvailable() bool { return false }

// applianceUsable reports no libguestfs on an unsupported platform.
func applianceUsable(context.Context) bool { return false }

// vmRuntimeUsable reports no VM runtime on an unsupported platform.
func vmRuntimeUsable() bool { return false }
