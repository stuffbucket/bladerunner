//go:build !darwin

package vm

import "github.com/stuffbucket/bladerunner/internal/config"

// AssessGrubRiskForConfig is a no-op on unsupported platforms (no VM runner, no
// persisted metadata): there is no guest to boot, so nothing can stall at GRUB.
func AssessGrubRiskForConfig(_ *config.Config, _ bool) GrubRisk {
	return GrubSafe
}
