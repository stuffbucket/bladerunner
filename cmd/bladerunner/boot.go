package main

import (
	"fmt"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
	"github.com/stuffbucket/bladerunner/internal/util"
)

var bootFlags struct {
	cpus         uint
	memory       uint64
	disk         int
	gui          bool
	headless     bool
	noRestore    bool
	persist      bool
	privateMount bool
	timeout      time.Duration
}

// bootManifest stashes the resolved disk manifest so runStart can fold it into
// the vmhost.Spec, where it is applied onto the config after the persisted
// Settings and before the flag overrides. It is set by runBoot just before
// delegating to runStart and is nil for plain `br start`.
var bootManifest *disk.Manifest

var bootCmd = &cobra.Command{
	Use:   "boot <name|url|path>",
	Short: "Slide a disk in and power it on",
	Long: `Resolve a disk, ensure its image is in the shared content-addressed cache,
apply the disk's recommended sizing, and boot it per its boot.mode (headless or
GUI) in an isolated per-disk state slot. If the slot holds saved guest RAM (from
a prior 'br eject'), the guest is restored where it left off instead of
cold-booting (use --no-restore to force a cold boot).

The argument resolves in this order:
  - a URL (contains "://")            — booted as a one-off disk named after its basename
  - an existing .sparseimage or .dmg  — booted as a cartridge
  - an existing path ending in ".disk" — loaded as a manifest file
  - otherwise a catalog disk name     — looked up in the shelf ('br disks')

A cartridge is one file holding a whole VM, built by 'br disk pack': the
runnable .sparseimage, or the compressed .dmg that --ship produces for AirDrop.
Booting a .dmg converts a fresh writable working copy beside it, so the shipped
file stays pristine. The cartridge carries its own disk, EFI state and
cloud-init, so it always cold-boots and the slot flags below do not apply to it;
put it away with 'br eject'.

Cartridge-only flags:

  --persist        Write the guest's changes back over the .dmg you booted.
                   Without it a .dmg boot is a throwaway run: every byte the
                   guest wrote is discarded when it powers off. That is the
                   default and it does not change. With it, once the guest has
                   powered off and the volume has been detached, the working
                   copy is compacted, compressed to a new .dmg beside the
                   original, verified, and only then renamed over it. The
                   original is never written into: a failed or interrupted
                   write-back leaves it byte-for-byte as it was, and the guest's
                   changes are kept in a '<name>-rescue-<timestamp>.sparseimage'
                   you can boot directly. Booting a .sparseimage already writes
                   into that file, so --persist is a no-op there.
  --private-mount  Attach the cartridge -nobrowse at <state>/mnt/<name> instead
                   of letting macOS place it under /Volumes. Deterministic, and
                   what scripts want — but invisible in Finder, so the cartridge
                   cannot be ejected by hand. The browsable default stays the
                   default. Do not use it while 'br disk pack' is writing a
                   cartridge of the same name: both want that mountpoint.

Sizing and boot-mode flags override the disk's recommendations. The disk carries
its own image, so there is no --image-url/--image-path here.

Like 'br start', the VM is owned by a holder process: boot prints the progress,
returns once the guest is up, and leaves the VM running. --gui is the exception
and stays in the foreground, where its window belongs.`,
	Args: cobra.ExactArgs(1),
	RunE: runBoot,
}

func init() {
	f := bootCmd.Flags()
	f.UintVar(&bootFlags.cpus, "cpus", 0, "Override the disk's CPU count")
	f.Uint64Var(&bootFlags.memory, "memory", 0, "Override the disk's memory (GiB)")
	f.IntVar(&bootFlags.disk, "disk", 0, "Override the disk's size (GiB)")
	f.BoolVar(&bootFlags.gui, "gui", false, "Force a GUI window (override boot.mode)")
	f.BoolVar(&bootFlags.headless, "headless", false, "Force headless boot (override boot.mode)")
	f.BoolVar(&bootFlags.noRestore, "no-restore", false, "Cold-boot even if the slot holds saved guest RAM")
	f.BoolVar(&bootFlags.persist, flagPersist, false,
		"Cartridge only: write the guest's changes back over the .dmg on eject (default: discard them)")
	f.BoolVar(&bootFlags.privateMount, flagPrivateMount, false,
		"Cartridge only: attach -nobrowse at <state>/mnt/<name> instead of the Finder-ejectable /Volumes default")
	f.DurationVar(&bootFlags.timeout, "timeout", config.DefaultTimeout, "How long to wait for the guest's Incus API to come up and authorize this client")
}

// The cartridge-only boot flags, named once so the registration, the refusal
// and the help all use the same spelling.
const (
	flagPersist      = "persist"
	flagPrivateMount = "private-mount"
)

// cartridgeOnlyFlags names the `br boot` flags that only mean anything for a
// cartridge image. Passing one on a disk or URL boot is refused rather than
// ignored: --persist ignored in silence tells a user their guest's changes are
// being kept when they are not, which is the one mistake this whole feature
// exists to avoid.
var cartridgeOnlyFlags = []string{flagPersist, flagPrivateMount}

// cartridgeOnlyFlagError refuses a boot that set a cartridge-only flag on a
// target that is not a cartridge. changed reports whether the user explicitly
// set a flag (cobra's Flags().Changed), so a default never trips it.
func cartridgeOnlyFlagError(changed func(string) bool) error {
	for _, name := range cartridgeOnlyFlags {
		if changed(name) {
			return fmt.Errorf("--%s applies only to a cartridge image (%s or %s); this argument resolves to a disk, so there is nothing to %s",
				name, cartridge.SparseExt, cartridge.DMGExt, name)
		}
	}
	return nil
}

// bootTargetKind classifies how a boot argument is to be resolved.
type bootTargetKind int

const (
	bootTargetURL bootTargetKind = iota
	bootTargetFile
	bootTargetName
	// bootTargetCartridge is a self-contained .sparseimage/.dmg cartridge image.
	bootTargetCartridge
)

// bootTarget is the classified boot argument plus the slot name derived for the
// URL/file cases (catalog names get their slot name from the manifest itself).
type bootTarget struct {
	kind bootTargetKind
	arg  string
}

// classifyBootArg decides whether arg is a URL, a .disk file path, or a catalog
// name. The .disk-file branch additionally requires the file to exist so a
// mistyped catalog name ending in ".disk" still falls through to the catalog
// lookup (and a clearer "not found" error). Kept pure for unit testing.
func classifyBootArg(arg string, fileExists func(string) bool) bootTarget {
	switch {
	case strings.Contains(arg, "://"):
		return bootTarget{kind: bootTargetURL, arg: arg}
	case isCartridgeArg(arg) && fileExists(arg):
		return bootTarget{kind: bootTargetCartridge, arg: arg}
	case strings.HasSuffix(arg, disk.ManifestExt) && fileExists(arg):
		return bootTarget{kind: bootTargetFile, arg: arg}
	default:
		return bootTarget{kind: bootTargetName, arg: arg}
	}
}

// isCartridgeArg reports whether arg names a cartridge image by extension
// (.sparseimage runnable form or .dmg ship form). A non-existent path with such
// an extension still falls through to the catalog lookup in classifyBootArg.
func isCartridgeArg(arg string) bool {
	return cartridge.HasImageExt(arg)
}

// slotNameFromURL derives a sanitized disk/slot name from a URL's basename by
// trimming a known image extension and replacing disallowed characters with
// dashes. Returns "" if nothing valid survives (the caller then errors).
func slotNameFromURL(rawurl string) string {
	base := strings.ToLower(path.Base(rawurl))
	// Drop a trailing image extension so "...-arm64.qcow2" -> "...-arm64".
	for _, ext := range []string{".qcow2", ".img", ".raw", ".disk"} {
		base = strings.TrimSuffix(base, ext)
	}
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	// Collapse runs of dashes and trim leading/trailing dashes so the result
	// satisfies the disk name regex (^[a-z0-9][a-z0-9-]*$).
	name := strings.Trim(collapseDashes(b.String()), "-")
	return name
}

func collapseDashes(s string) string {
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return s
}

// resolveBootManifest turns a classified boot argument into a concrete manifest.
func resolveBootManifest(t bootTarget, cat *disk.Catalog) (*disk.Manifest, error) {
	switch t.kind {
	case bootTargetURL:
		name := slotNameFromURL(t.arg)
		if name == "" {
			return nil, fmt.Errorf("cannot derive a disk name from URL %q; create a disk first with 'br disk new <name>'", t.arg)
		}
		m := &disk.Manifest{
			Name: name,
			Image: disk.ImageSpec{
				Arches: map[string]disk.ArchImage{
					runtime.GOARCH: {URL: t.arg},
				},
			},
			Boot: disk.BootSpec{Mode: disk.BootModeHeadless},
		}
		if err := m.Validate(); err != nil {
			return nil, fmt.Errorf("synthesized disk for %q is invalid: %w", t.arg, err)
		}
		return m, nil

	case bootTargetFile:
		return disk.Load(t.arg)

	default: // bootTargetName
		// A path that names a cartridge image reached this branch because no
		// such FILE exists (classifyBootArg requires it). Answering that with
		// the disk catalog told a user who had passed a path to go and consult
		// a shelf of names; the extension says what was meant, so say that.
		if cartridge.HasImageExt(t.arg) {
			return nil, fmt.Errorf("no cartridge at %q: the path names a cartridge image (%s or %s) but no such file exists; check the path, or pack one with 'br disk pack <disk> --out <name>%s'",
				t.arg, cartridge.SparseExt, cartridge.DMGExt, cartridge.SparseExt)
		}
		e, ok := cat.Lookup(t.arg)
		if !ok {
			return nil, fmt.Errorf("unknown disk %q; %s", t.arg, availableDisksHint(cat))
		}
		return e.Manifest, nil
	}
}

// availableDisksHint lists known disk names for a not-found error.
func availableDisksHint(cat *disk.Catalog) string {
	entries := cat.List()
	if len(entries) == 0 {
		return "no disks available (create one with 'br disk new <name>')"
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Manifest.Name)
	}
	return "available disks: " + strings.Join(names, ", ")
}

func runBoot(cmd *cobra.Command, args []string) error {
	if bootFlags.gui && bootFlags.headless {
		return jsonOrError(fmt.Errorf("--gui and --headless are mutually exclusive"))
	}

	target := classifyBootArg(args[0], util.FileExists)
	if target.kind == bootTargetCartridge {
		return runBootCartridge(cmd, args, target.arg)
	}
	if err := cartridgeOnlyFlagError(cmd.Flags().Changed); err != nil {
		return jsonOrError(err)
	}

	cat, err := disk.LoadCatalog()
	if err != nil {
		return jsonOrError(fmt.Errorf("load disk catalog: %w", err))
	}

	m, err := resolveBootManifest(target, cat)
	if err != nil {
		return jsonOrError(err)
	}

	baseDir := diskSlotDir(m.Name)

	// Refuse to boot a disk that is already held in its slot (the gate is
	// per-socket, and each slot has its own control socket via baseDir). The
	// gate is the liveness ladder, not a ping: a wedged holder answers nothing
	// and still owns the slot's disk image.
	if instanceHeld(baseDir) {
		return jsonOrError(fmt.Errorf("disk %q is already booted (use 'br eject' first)", m.Name))
	}

	// Resolve the effective boot mode: explicit flags win over the manifest.
	guiMode := m.Boot.Mode == disk.BootModeGUI
	switch {
	case bootFlags.gui:
		guiMode = true
	case bootFlags.headless:
		guiMode = false
	}

	// Drive the existing start path through its package-level flag struct. The
	// slot's baseDir roots every per-VM path; manifest sizing/image is applied
	// inside runStart via bootManifest, and these explicit overrides land after
	// it so they win.
	// Sizing precedence: an explicit boot flag wins, else the disk manifest's
	// recommendation, else the global default. (start.go later writes these onto
	// cfg unconditionally, so the manifest value must be folded in here — applying
	// it to cfg alone would be clobbered.)
	startFlags.stateDir = baseDir
	startFlags.cpus = pickCPUs(bootFlags.cpus, m.VM.CPUs)
	startFlags.memory = pickMemoryGiB(bootFlags.memory, m.VM.MemoryGiB)
	startFlags.disk = pickDiskGiB(bootFlags.disk, m.VM.DiskSizeGiB)
	startFlags.gui = guiMode
	startFlags.timeout = bootFlags.timeout
	startFlags.imageURL = ""
	startFlags.imagePath = ""
	startFlags.noNested = false

	// Restore-with-memory: if the slot holds saved RAM and the user didn't ask
	// for a cold boot, restore it. boot derives GUI from the manifest so the
	// restored device topology matches the save (enforced in prepareRestore).
	save := savedStatePath(baseDir)
	if !bootFlags.noRestore && util.FileExists(save) {
		startFlags.restoreFrom = save
	} else {
		startFlags.restoreFrom = ""
	}

	if !jsonOutput {
		action := "Booting"
		if startFlags.restoreFrom != "" {
			action = "Restoring"
		}
		fmt.Printf("%s disk %s (%s)\n", subtle(action), value(m.Name), modeLabel(guiMode))
	}

	// Hand the manifest to runStart, which applies it onto cfg right after
	// config.Default and before the flag overrides.
	bootManifest = m
	return runStart(cmd, args)
}

func modeLabel(gui bool) string {
	if gui {
		return disk.BootModeGUI
	}
	return disk.BootModeHeadless
}

// pickCPUs resolves sizing precedence: the explicit boot flag wins when set
// (non-zero), else the disk manifest's recommendation when set, else the global
// default. boot flags and manifest fields both use 0 to mean "unset".
func pickCPUs(flag, manifest uint) uint {
	switch {
	case flag > 0:
		return flag
	case manifest > 0:
		return manifest
	default:
		return config.DefaultCPUs
	}
}

func pickMemoryGiB(flag, manifest uint64) uint64 {
	switch {
	case flag > 0:
		return flag
	case manifest > 0:
		return manifest
	default:
		return config.DefaultMemoryGiB
	}
}

func pickDiskGiB(flag, manifest int) int {
	switch {
	case flag > 0:
		return flag
	case manifest > 0:
		return manifest
	default:
		return config.DefaultDiskSizeGiB
	}
}
