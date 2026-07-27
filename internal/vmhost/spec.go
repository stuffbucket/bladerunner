package vmhost

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
	"github.com/stuffbucket/bladerunner/internal/instance"
)

// ErrInvalidSpec is the sentinel every Spec.Validate failure wraps, so callers
// can tell "you asked for something impossible" apart from "starting it failed".
var ErrInvalidSpec = errors.New("invalid vmhost spec")

// maxPort is the largest legal TCP port number; a preference above it can never
// be reserved.
const maxPort = 65535

// Overrides carries the resolved `br start` flag values that are layered onto
// the instance config after the persisted Settings and the disk manifest.
//
// The fields mirror the CLI flags one-for-one and are deliberately plain
// scalars: a holder process is launched by re-exec, so a Spec must survive a
// round trip through JSON with no live handles or callbacks in it.
type Overrides struct {
	CPUs         uint   `json:"cpus,omitempty"`
	MemoryGiB    uint64 `json:"memoryGiB,omitempty"`
	DiskSizeGiB  int    `json:"diskSizeGiB,omitempty"`
	GUI          bool   `json:"gui,omitempty"`
	ImageURL     string `json:"imageURL,omitempty"`
	ImagePath    string `json:"imagePath,omitempty"`
	HostedImage  bool   `json:"hostedImage,omitempty"`
	DebianImage  bool   `json:"debianImage,omitempty"`
	NoNestedVirt bool   `json:"noNestedVirt,omitempty"`

	// Timeout is `--timeout`: the budget the boot gives the guest to bring the
	// Incus API up and authorize this client. It is the ONE budget that governs
	// the readiness wait — see resolveWaitBudget, which is the only place the
	// decision is made. Zero means "not supplied", which is why it must never
	// be written onto the config verbatim: `omitempty` drops it from a Spec
	// serialized to a holder process, so a zero here has to mean "keep the
	// baseline", not "wait for no time at all".
	Timeout time.Duration `json:"timeout,omitempty"`
}

// Spec is the complete, serializable description of one instance to run.
//
// Everything a Host needs comes from here; nothing is read from process-global
// state. That is what lets two Hosts be constructed in one process and what
// lets a holder process be handed a Spec as JSON on re-exec.
type Spec struct {
	// Name is the instance name (the registry key and the ssh alias suffix).
	// Empty means "derive it from the state directory" — see
	// config.Config.InstanceName.
	Name string `json:"name,omitempty"`

	// Kind selects the teardown path: a cartridge has an image to detach, a
	// disk slot and the flat default instance do not.
	Kind instance.Kind `json:"kind"`

	// StateDir roots every per-VM path. Empty means the default state dir. For
	// a cartridge it is ignored in favor of the mountpoint.
	StateDir string `json:"stateDir,omitempty"`

	// Config, when non-nil, is used as the resolved base config instead of
	// calling config.Default(StateDir). The overlay chain (Settings, Manifest,
	// Overrides, Ports, cartridge) is still applied on top of it. It must not
	// carry live handles: HostListeners is populated by the Host itself.
	Config *config.Config `json:"config,omitempty"`

	// CartridgePath is the cartridge image the instance boots from (the
	// shipped .dmg or a runnable .sparseimage). Only valid for KindCartridge.
	CartridgePath string `json:"cartridgePath,omitempty"`

	// Mountpoint is where the cartridge image is attached. Required for
	// KindCartridge; must be empty otherwise.
	Mountpoint string `json:"mountpoint,omitempty"`

	// Manifest is the disk manifest applied as defaults after the persisted
	// Settings and before Overrides. Nil for a plain start.
	Manifest *disk.Manifest `json:"manifest,omitempty"`

	// Overrides are the CLI flag values. Driven and ChangedFlags decide which
	// of them actually land — see applyOverrides.
	Overrides Overrides `json:"overrides,omitzero"`

	// ChangedFlags names the flags the user explicitly set. On a plain start
	// only these are applied, so cobra's flag defaults cannot clobber the
	// persisted Settings baseline.
	ChangedFlags []string `json:"changedFlags,omitempty"`

	// Driven marks a boot/cartridge start, whose Overrides already carry
	// pre-resolved precedence and are therefore applied verbatim.
	Driven bool `json:"driven,omitempty"`

	// RestoreFrom is a saved-state file to resume from instead of cold-booting.
	// Cartridges always cold-boot, so it must be empty for KindCartridge.
	RestoreFrom string `json:"restoreFrom,omitempty"`

	// Ports is an explicit port preference. The zero value means "keep
	// whatever the resolved config asks for", which is the well-known set for
	// the flat default instance.
	Ports config.PortAssignment `json:"ports,omitzero"`

	// DrainTimeout bounds the orderly guest shutdown performed by Drain. Zero
	// means the vm package default.
	DrainTimeout time.Duration `json:"drainTimeout,omitempty"`

	// BinaryVersion is the build that started the instance; it answers the
	// control-plane version query and is recorded in the registry entry.
	BinaryVersion string `json:"binaryVersion,omitempty"`
}

// Validate reports why a Spec cannot be run, or nil. Every error wraps
// ErrInvalidSpec. It is pure: it reads no files and starts nothing, so a caller
// can validate a Spec long before it is willing to boot it. The one exception
// is the image-override check, which consults the same force-image environment
// variables the flags mirror (see config.ForceHostedImage).
func (s Spec) Validate() error {
	if err := s.validateIdentity(); err != nil {
		return err
	}
	if err := s.validateCartridge(); err != nil {
		return err
	}
	if err := s.validateImageOverrides(); err != nil {
		return err
	}
	if err := s.validateSizing(); err != nil {
		return err
	}
	if err := s.validatePorts(); err != nil {
		return err
	}
	if s.DrainTimeout < 0 {
		return fmt.Errorf("%w: drain timeout %v is negative", ErrInvalidSpec, s.DrainTimeout)
	}
	if s.Config != nil && s.Config.HostListeners != nil {
		return fmt.Errorf("%w: config carries live host listeners; a spec must be inert", ErrInvalidSpec)
	}
	return nil
}

// validateIdentity checks the name and kind, which together key the registry.
func (s Spec) validateIdentity() error {
	if s.Name != "" {
		if err := instance.ValidName(s.Name); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidSpec, err)
		}
	}
	if !s.Kind.Valid() {
		return fmt.Errorf("%w: unknown instance kind %q", ErrInvalidSpec, s.Kind)
	}
	return nil
}

// validateCartridge enforces that the cartridge fields and the kind agree, in
// both directions: a cartridge boot needs an image and a mountpoint, and a
// non-cartridge boot must not carry either.
func (s Spec) validateCartridge() error {
	if s.Kind != instance.KindCartridge {
		if s.CartridgePath != "" {
			return fmt.Errorf("%w: cartridge path %q set for kind %q", ErrInvalidSpec, s.CartridgePath, s.Kind)
		}
		if s.Mountpoint != "" {
			return fmt.Errorf("%w: mountpoint %q set for kind %q", ErrInvalidSpec, s.Mountpoint, s.Kind)
		}
		return nil
	}
	if s.CartridgePath == "" {
		return fmt.Errorf("%w: kind %q needs a cartridge path", ErrInvalidSpec, instance.KindCartridge)
	}
	if s.Mountpoint == "" {
		return fmt.Errorf("%w: kind %q needs a mountpoint", ErrInvalidSpec, instance.KindCartridge)
	}
	if s.RestoreFrom != "" {
		return fmt.Errorf("%w: a cartridge always cold-boots, so it cannot restore from %q", ErrInvalidSpec, s.RestoreFrom)
	}
	return nil
}

// validateSizing rejects a driven start with unresolved sizing. A driven Spec
// is applied verbatim, so a zero here would silently configure a VM with no
// CPUs; a non-driven start leaves the field alone unless the user changed it,
// where zero simply means "untouched".
func (s Spec) validateSizing() error {
	if !s.Driven {
		return nil
	}
	switch {
	case s.Overrides.CPUs == 0:
		return fmt.Errorf("%w: a driven spec must resolve CPUs", ErrInvalidSpec)
	case s.Overrides.MemoryGiB == 0:
		return fmt.Errorf("%w: a driven spec must resolve memory", ErrInvalidSpec)
	case s.Overrides.DiskSizeGiB == 0:
		return fmt.Errorf("%w: a driven spec must resolve the disk size", ErrInvalidSpec)
	default:
		return nil
	}
}

// validatePorts rejects port preferences that could never be bound.
func (s Spec) validatePorts() error {
	for _, p := range []struct {
		name string
		port int
	}{
		{config.PortNameSSH, s.Ports.SSH},
		{config.PortNameAPI, s.Ports.API},
		{config.PortNameWeb, s.Ports.Web},
		{config.PortNameOIDC, s.Ports.OIDC},
		{config.PortNameNTP, s.Ports.NTP},
	} {
		if p.port < 0 || p.port > maxPort {
			return fmt.Errorf("%w: %s port %d is out of range", ErrInvalidSpec, p.name, p.port)
		}
	}
	return nil
}

// validateImageOverrides rejects contradictory image-selection overrides.
// --hosted-image (or its force env) selects the pre-baked hosted image and
// --debian-image (or its force env) selects the Debian escape hatch, so the two
// cannot be combined with each other or with an explicit --image-url/--image-path
// (which pick a different, user-supplied image). Asking for two at once is a user
// error, not something to resolve silently by precedence.
func (s Spec) validateImageOverrides() error {
	if s.forceHostedImage() && s.forceDebianImage() {
		return fmt.Errorf("--hosted-image conflicts with --debian-image (also check BLADERUNNER_FORCE_HOSTED_IMAGE / BLADERUNNER_FORCE_DEBIAN_IMAGE)")
	}
	if !s.forceHostedImage() && !s.forceDebianImage() {
		return nil
	}
	which := "--hosted-image"
	if s.forceDebianImage() {
		which = "--debian-image"
	}
	if s.Overrides.ImageURL != "" {
		return fmt.Errorf("%s conflicts with --image-url", which)
	}
	if s.Overrides.ImagePath != "" {
		return fmt.Errorf("%s conflicts with --image-path", which)
	}
	return nil
}

// forceHostedImage reports whether this run must use the pre-baked hosted guest
// image, requested either via the --hosted-image flag or the
// BLADERUNNER_FORCE_HOSTED_IMAGE=1 env (the non-interactive equivalent).
func (s Spec) forceHostedImage() bool {
	return s.Overrides.HostedImage || config.ForceHostedImage()
}

// forceDebianImage reports whether this run must use the Debian genericcloud +
// cloud-init escape hatch, requested either via the --debian-image flag or the
// BLADERUNNER_FORCE_DEBIAN_IMAGE=1 env (the non-interactive equivalent).
func (s Spec) forceDebianImage() bool {
	return s.Overrides.DebianImage || config.ForceDebianImage()
}

// changed reports whether the named flag should be applied: every flag on a
// driven (boot/cartridge) start, only the ones the user explicitly set
// otherwise.
func (s Spec) changed(name string) bool {
	return s.Driven || slices.Contains(s.ChangedFlags, name)
}

// applyOverrides applies the `start` CLI flags onto cfg, on top of the
// already-overlaid Settings and disk manifest. When Driven is true (a
// boot/cartridge boot stuffed pre-resolved precedence into the Overrides) every
// flag is applied verbatim; otherwise only flags the user explicitly changed
// (per ChangedFlags) are applied, so the persisted Settings baseline is not
// clobbered by cobra's flag defaults.
func (s Spec) applyOverrides(cfg *config.Config) {
	o := s.Overrides

	if s.changed("cpus") {
		cfg.CPUs = o.CPUs
	}
	if s.changed("memory") {
		cfg.MemoryGiB = o.MemoryGiB
	}
	if s.changed("disk") {
		cfg.DiskSizeGiB = o.DiskSizeGiB
	}
	if s.changed("gui") {
		cfg.GUI = o.GUI
	}
	// The readiness budget is RESOLVED, never assigned: see resolveWaitBudget.
	// Assigning it the way every other flag is assigned is what let a Spec with
	// no timeout in it (a zero survives no JSON round trip, and a driven start
	// applies its overrides verbatim) hand the readiness wait a budget of zero.
	cfg.WaitForIncus = resolveWaitBudget(s.timeoutOverride(), cfg.WaitForIncus)
	if s.changed("no-nested-virt") {
		cfg.NestedVirtDisabled = o.NoNestedVirt
	}
	// Image flags keep their "non-empty means set" guard: a boot/cartridge start
	// clears them (it carries the image via the manifest), and a plain start
	// leaves them empty unless the user passed one.
	if o.ImageURL != "" && s.changed("image-url") {
		cfg.BaseImageURL = o.ImageURL
		// A custom image isn't the pinned Debian default, so the embedded
		// SHA-512 no longer applies; fall back to sidecar verification.
		cfg.BaseImageSHA512 = ""
	}
	if o.ImagePath != "" && s.changed("image-path") {
		cfg.BaseImagePath = o.ImagePath
	}
	// --hosted-image (or BLADERUNNER_FORCE_HOSTED_IMAGE=1) forces the pre-baked
	// hosted guest image. Since the hosted image is now the DEFAULT, this mostly
	// re-selects it over a persisted Settings image choice or makes the intent
	// explicit. It re-resolves BaseImageURL to the guest-image-latest release asset
	// for this arch, clears the pinned Debian SHA-512 and any local path, and arms
	// UseHostedGuestImage — which switches vm asset verification to the fail-closed
	// .sha256 sidecar path. --debian-image is the opposite escape hatch: it forces
	// the Debian genericcloud + cloud-init path. Conflicts (both flags, or either
	// with --image-url/--image-path) are rejected up front by Validate, so at most
	// one force lands here.
	if s.forceHostedImage() {
		if url, err := config.HostedGuestImageURL(cfg.Arch); err == nil {
			cfg.BaseImageURL = url
		}
		cfg.BaseImageSHA512 = ""
		cfg.BaseImageExpectedSHA256 = ""
		cfg.BaseImagePath = ""
		cfg.UseHostedGuestImage = true
	}
	if s.forceDebianImage() {
		// UseDebianImage repoints the URL, restores the pinned SHA-512, disarms
		// UseHostedGuestImage, and clears any local path / manifest pin — so no
		// auto-fallback is needed and the escape hatch boots the verified Debian
		// path directly. Best effort: an unsupported arch leaves the default URL.
		_ = config.UseDebianImage(cfg)
	}
}

// timeoutOverride is the `--timeout` the user actually supplied, or 0 when they
// supplied none.
//
// A driven (boot/cartridge) start carries pre-resolved precedence and so counts
// as having supplied every flag — but only a POSITIVE value is an override. A
// zero on a driven Spec means the field was never filled in (it is `omitempty`,
// so it does not survive the JSON round trip to a holder process), and treating
// that as "the user asked for no time at all" is the bug this exists to prevent.
func (s Spec) timeoutOverride() time.Duration {
	if !s.changed("timeout") {
		return 0
	}
	return s.Overrides.Timeout
}

// resolveWaitBudget picks the budget the Incus readiness wait runs under, and it
// is the only place that decision is made — cfg.WaitForIncus is what the runner
// bounds the wait with, and this function is what writes it.
//
// The precedence is total, and deliberately so:
//
//  1. the user's --timeout, when they supplied one. It wins over everything,
//     including a persisted Settings value and this package's own default. That
//     is the whole point of the flag.
//  2. the baseline already resolved onto the config — the persisted
//     Settings.waitForIncus, else config.Default's DefaultTimeout.
//  3. config.DefaultTimeout, so a config that arrived with no budget at all
//     (a hand-built Spec.Config, a zeroed round trip) still gets one.
//
// It never returns a non-positive duration. A zero budget makes
// context.WithTimeout expire before the first probe, which surfaces far away as
// either an instant "deadline exceeded" or a config-validation error about
// wait-for-incus, neither of which names the flag that caused it.
func resolveWaitBudget(override, base time.Duration) time.Duration {
	switch {
	case override > 0:
		return override
	case base > 0:
		return base
	default:
		return config.DefaultTimeout
	}
}
