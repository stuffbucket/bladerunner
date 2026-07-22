// GrubRisk and AssessGrubRisk are pure host-state logic with no platform
// dependency, so this file carries no build tag: it compiles on every platform,
// including the unsupported-platform stub build. The config-reading wrapper that
// needs the darwin-only runtimeMetadata lives in grubcheck_darwin.go, with a
// no-op stub in grubcheck_other.go so callers (cmd/bladerunner) build anywhere.
package vm

// GrubRisk classifies the pre-boot risk that an un-hardened guest grub.cfg
// stalls the boot menu on the recordfail flag. It is derived from host state
// alone (persisted metadata + disk presence); the host cannot read or edit the
// guest ext4, so this is a heuristic that INFORMS only — it never blocks a boot.
type GrubRisk int

const (
	GrubSafe GrubRisk = iota
	GrubAtRisk
	GrubKnownWedged
)

// String renders the risk for operator-facing messages.
func (r GrubRisk) String() string {
	switch r {
	case GrubSafe:
		return "safe"
	case GrubAtRisk:
		return "at-risk"
	case GrubKnownWedged:
		return "known-wedged"
	default:
		return "unknown"
	}
}

// GrubCheckInput is the host-observable state used to assess grub-stall risk.
type GrubCheckInput struct {
	DiskExists        bool
	GrubHardened      bool
	LastShutdownClean bool
	Restoring         bool
}

// AssessGrubRisk classifies pre-boot grub-stall risk from host state alone.
func AssessGrubRisk(in GrubCheckInput) GrubRisk {
	switch {
	case in.Restoring:
		return GrubSafe // resume from saved RAM never re-enters GRUB
	case in.GrubHardened:
		return GrubSafe // recordfail can't stall a hardened disk
	case !in.DiskExists:
		return GrubSafe // fresh disk: baked-hardened image or first-boot update-grub; no recordfail yet
	case in.LastShutdownClean:
		return GrubAtRisk // unhardened, clean stop -> recordfail likely unset; boot re-hardens
	default:
		return GrubKnownWedged // unhardened + unclean -> recordfail=1 likely, may stall
	}
}
