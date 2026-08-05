//go:build !darwin

package vm

import (
	"context"
	"errors"
)

// ErrMissingVirtualizationEntitlement exists on every platform so callers need
// no build tag. Off macOS nothing produces it: code signing entitlements are a
// macOS concept, and Virtualization.framework is not present to demand one.
var ErrMissingVirtualizationEntitlement = errors.New("the Virtualization entitlement is a macOS concept")

// CheckSelfEntitlement always succeeds off macOS. See the darwin build for what
// this answers and why it is asked before a VM is spawned rather than after.
func CheckSelfEntitlement(context.Context) error { return nil }
