//go:build darwin

package vm

import (
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/util"
)

// AssessGrubRiskForConfig reads host state (persisted runtimeMetadata + disk
// presence) for cfg and assesses the pre-boot grub-stall risk. runtimeMetadata
// is darwin-only, so the wrapper is too; grubcheck_other.go provides a GrubSafe
// stub for the unsupported-platform build.
func AssessGrubRiskForConfig(cfg *config.Config, restoring bool) GrubRisk {
	md := peekMetadata(cfg)
	return AssessGrubRisk(GrubCheckInput{
		DiskExists:        util.FileExists(cfg.DiskPath),
		GrubHardened:      md.GrubHardened,
		LastShutdownClean: md.LastShutdownClean,
		Restoring:         restoring,
	})
}
