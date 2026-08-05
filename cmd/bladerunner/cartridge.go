package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/disk"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/vm"
	"github.com/stuffbucket/bladerunner/internal/vmhost"
)

// This file is the CLI surface over internal/cartridge: `br disk pack` (build a
// cartridge) and the `br boot <cartridge>` entry point. The cartridge semantics
// themselves — the on-image layout, the pack layout writer, and the
// convert/attach/verify open sequence — live in internal/cartridge so a holder
// process can drive them without importing package main.

// --- runner disk pack ----------------------------------------------------

var diskPackFlags struct {
	out  string
	ship bool
	arch string
	size int
}

var diskPackCmd = &cobra.Command{
	Use:   "pack <name>",
	Short: "Build an AirDrop-able cartridge from a disk",
	Long: `Pack a catalog or user disk into a self-contained, AirDrop-able cartridge: a
single APFS sparse image holding the disk manifest, a bootable root.img, EFI +
cloud-init state, and a read-write host<->guest share folder.

Because 'br eject' always powers the guest off cleanly (ACPI), a cartridge is
always in a consistent cold-boot state — AirDrop the file to any Mac running
bladerunner and 'br boot <file>' just works.

  --out <file>   Output path for the runnable cartridge. It must end in
                 ".sparseimage" (a name with no extension gets one).
                 Default: ./<name>.sparseimage
  --ship         Also produce a compressed read-only <name>.dmg (the AirDrop form)
  --arch <arch>  Target architecture for the root image (default: host GOARCH)
  --size <GiB>   Cartridge capacity (default: disk size + headroom)

'disk pack' always writes the RUNNABLE .sparseimage; --ship writes the .dmg
beside it. So '--out demo.dmg' is refused — ask for '--out demo.sparseimage'
and add --ship.

The cartridge is named after its OUTPUT FILE, not after the disk it was packed
from: '--out demo.sparseimage' produces a volume named bladerunner-demo, and
'br boot demo.sparseimage' runs it as instance "demo". Omit --out and the two
coincide. The name must be lowercase letters, digits and dashes.

Requires macOS (hdiutil) and qemu-img.`,
	Args: cobra.ExactArgs(1),
	RunE: runDiskPack,
}

func init() {
	f := diskPackCmd.Flags()
	f.StringVar(&diskPackFlags.out, "out", "", "Output cartridge path; must end in .sparseimage (default: ./<name>.sparseimage)")
	f.BoolVar(&diskPackFlags.ship, "ship", false, "Also produce a compressed read-only <name>.dmg AirDrop artifact")
	f.StringVar(&diskPackFlags.arch, "arch", runtime.GOARCH, "Target architecture for the root image")
	f.IntVar(&diskPackFlags.size, "size", 0, "Cartridge capacity in GiB (default: disk size + headroom)")

	diskCmd.AddCommand(diskPackCmd)
}

// cartridgePackReport is the JSON result for `br disk pack`.
type cartridgePackReport struct {
	Status     string `json:"status"`
	Name       string `json:"name"`
	Disk       string `json:"disk"`
	Cartridge  string `json:"cartridge"`
	DMG        string `json:"dmg,omitempty"`
	SizeGiB    int    `json:"size_gib"`
	RootImg    string `json:"root_img"`
	DiskGiB    int    `json:"disk_gib"`
	ShareTag   string `json:"share_tag"`
	SharePath  string `json:"share_guest_path"`
	Compressed bool   `json:"compressed"`
}

// packSizeGiB resolves the cartridge capacity: an explicit --size wins, else the
// disk size plus cartridge headroom (clamped to the cartridge minimum).
func packSizeGiB(flagSize, diskGiB int) int {
	if flagSize > 0 {
		return flagSize
	}
	return cartridge.SizeGiB(diskGiB)
}

// errPackOutExtension refuses an `--out` path that does not name the runnable
// cartridge form. It is a sentinel so the refusal is recognizable in a test and
// distinguishable from a name that is merely unusable.
var errPackOutExtension = errors.New("cartridge output path must name the runnable form")

// packOutPath resolves the output cartridge path: an explicit --out wins, else
// ./<name>.sparseimage in the cwd. Either way the result carries the
// .sparseimage extension, because hdiutil appends it regardless — and a
// requested path that differs from the file that actually appears would make
// the cartridge's name (packCartridgeName, derived from this path) disagree
// with the name `br boot` later derives from that file.
//
// A bare `--out demo` is therefore accepted and becomes demo.sparseimage, while
// `--out demo.dmg` is REFUSED here. It used to become demo.dmg.sparseimage,
// whose name "demo.dmg" then failed instance.ValidName three calls later and
// put a regex in front of a user who had only picked the wrong extension —
// after --ship had advertised a .dmg to them. `disk pack` writes the runnable
// form; --ship produces the .dmg.
func packOutPath(flagOut, name string) (string, error) {
	if flagOut != "" {
		if ext := filepath.Ext(flagOut); ext != "" && ext != cartridge.SparseExt {
			return "", fmt.Errorf("%w: %s ends in %q; write the runnable cartridge with '--out %s%s', and add --ship to also produce the compressed %s AirDrop artifact",
				errPackOutExtension, flagOut, ext, strings.TrimSuffix(flagOut, ext), cartridge.SparseExt, cartridge.DMGExt)
		}
		return ensureSparseExt(flagOut), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	return filepath.Join(cwd, name+cartridge.SparseExt), nil
}

// ensureSparseExt appends the runnable cartridge extension unless p already
// carries it.
func ensureSparseExt(p string) string {
	if strings.HasSuffix(p, cartridge.SparseExt) {
		return p
	}
	return p + cartridge.SparseExt
}

// packCartridgeName is the identity of the cartridge being written: the base
// name of the output file with its cartridge extension trimmed.
//
// It is deliberately NOT the source disk's name. `br boot <file>` derives the
// cartridge name the same way (cartridge.NameFromPath), and everything
// downstream — the instance name, the registry key, the ssh alias — follows
// from that, while detection reads the name back out of the APFS VOLUME name
// (cartridge.NameFromVolume). Seeding both ends from the output path is what
// keeps them from disagreeing for every cartridge whose file name differs from
// the disk it was packed from, and what stops two such cartridges from
// colliding on one /Volumes path.
//
// With --out omitted the output is ./<disk>.sparseimage, so the cartridge name
// is still the disk name — the previous behavior, now reached by derivation
// rather than by assumption.
//
// The name must survive as far as the instance registry, so it is checked with
// instance.ValidName (the strictest of the name rules, and a superset of
// disk.ValidName) rather than with a rule invented here.
func packCartridgeName(outPath string) (string, error) {
	name := cartridge.NameFromPath(outPath)
	if err := instance.ValidName(name); err != nil {
		return "", fmt.Errorf("cartridge name %q derived from output path %s is unusable: %w", name, outPath, err)
	}
	return name, nil
}

// runDiskPack renders whatever packCartridge reported. The body returns a plain
// error so its deferred cleanup can JOIN a cleanup failure onto it; the one
// combined error is emitted here, which is also what puts the cleanup diagnosis
// in front of a --json consumer. It used to be printed only in text mode, so a
// scripted pack was told nothing at all when the image could not be released.
func runDiskPack(cmd *cobra.Command, args []string) error {
	if err := packCartridge(cmd, args); err != nil {
		return jsonOrError(err)
	}
	return nil
}

// cleanUpPack releases the cartridge mount and, ONLY once that release is
// confirmed, removes the partial image a failed pack left behind.
//
// The order is the whole point. Unlinking an image whose volume the kernel is
// still serving is the data loss AGENTS.md section 8 names, and a detach whose
// result was ignored says nothing about whether the volume is gone. So a failed
// or unconfirmed release KEEPS the image and reports the path and the mountpoint
// the user has to act on; only a confirmed release authorizes the removal.
//
// release is injected so the rule is testable without hdiutil; the caller passes
// cartridge.DetachMount, which is the owner package's identity-addressed detach.
func cleanUpPack(release func() error, imgPath, mountpoint string, packed bool) error {
	if err := release(); err != nil {
		if packed {
			return fmt.Errorf("detach cartridge %s mounted at %s: %w", imgPath, mountpoint, err)
		}
		return fmt.Errorf("detach cartridge %s mounted at %s: %w; the partial image was KEPT because it may still be attached — release it and delete it by hand before retrying", imgPath, mountpoint, err)
	}
	if packed {
		return nil
	}
	// A failed pack leaves a partial image; remove it so a retry is clean.
	if err := os.Remove(imgPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the partial cartridge image %s: %w", imgPath, err)
	}
	return nil
}

func packCartridge(cmd *cobra.Command, args []string) (err error) {
	name := args[0]
	if !disk.ValidName(name) {
		return fmt.Errorf("invalid disk name %q: must be lowercase letters, digits, and dashes (start alphanumeric)", name)
	}

	cat, err := disk.LoadCatalog()
	if err != nil {
		return fmt.Errorf("load disk catalog: %w", err)
	}
	entry, ok := cat.Lookup(name)
	if !ok {
		return fmt.Errorf("unknown disk %q; %s", name, availableDisksHint(cat))
	}
	m := entry.Manifest

	// Resolve sizing: the manifest's disk size (defaulted) drives the cartridge
	// capacity unless --size overrides it.
	diskGiB := pickDiskGiB(0, m.VM.DiskSizeGiB)
	sizeGiB := packSizeGiB(diskPackFlags.size, diskGiB)

	outPath, err := packOutPath(diskPackFlags.out, name)
	if err != nil {
		return err
	}
	// From here on the CARTRIDGE's own name is the identity: the volume name,
	// the on-image metadata, the private mount slot and the report all take it,
	// so a boot of this file and a detection of its volume name agree.
	cartName, err := packCartridgeName(outPath)
	if err != nil {
		return err
	}

	if !jsonOutput {
		fmt.Printf("%s cartridge %s from disk %s (%s, %d GiB) -> %s\n",
			subtle("Packing"), value(cartName), value(name), diskPackFlags.arch, sizeGiB, subtle(outPath))
	}

	// 1) Create the sparse image.
	imgPath, err := cartridge.Create(outPath, sizeGiB)
	if err != nil {
		return fmt.Errorf("create cartridge image: %w", err)
	}

	// 2) Attach to a private mountpoint and lay out the cartridge.
	mountpoint := cartridgeMountpoint(cartName)
	mount, err := cartridge.Attach(imgPath, mountpoint)
	if err != nil {
		return fmt.Errorf("attach cartridge: %w", err)
	}
	// Always release the image when done (success or failure), so we never
	// strand it — and never unlink it while it may still be attached.
	packed := false
	mountedAt := mount.Mountpoint
	defer func() {
		err = errors.Join(err, cleanUpPack(
			func() error { return cartridge.DetachMount(*mount) },
			imgPath, mountedAt, packed))
	}()

	if err := layoutCartridge(cmd, mount.Mountpoint, m, cartName); err != nil {
		return err
	}

	packed = true

	report := cartridgePackReport{
		Status:    "packed",
		Name:      cartName,
		Disk:      name,
		Cartridge: imgPath,
		SizeGiB:   sizeGiB,
		RootImg:   cartridge.RootImageFile,
		DiskGiB:   diskGiB,
		ShareTag:  cartridge.ShareTag(m),
		SharePath: cartridge.ShareGuestPath(m),
	}

	// 3) Optionally ship: compress to a read-only DMG (the AirDrop artifact). The
	// image must be detached before convert reads it, so run the release now and
	// clear the Mount, which is what makes the deferred cleanup a no-op.
	if diskPackFlags.ship {
		if derr := cartridge.DetachMount(*mount); derr != nil {
			return fmt.Errorf("detach before ship: %w", derr)
		}
		*mount = cartridge.Mount{}
		dmgStem := cartridge.TrimExt(imgPath)
		dmgPath, derr := cartridge.ConvertToDMG(imgPath, dmgStem)
		if derr != nil {
			return fmt.Errorf("convert cartridge to dmg: %w", derr)
		}
		report.DMG = dmgPath
		report.Compressed = true
	}

	if jsonOutput {
		return emitJSON(report)
	}
	fmt.Printf("%s Packed cartridge %s\n", success("✓"), value(imgPath))
	if report.DMG != "" {
		fmt.Printf("  %s %s\n", key("AirDrop (dmg):"), value(report.DMG))
	}
	fmt.Printf("  %s %s mounted at %s in the guest\n", key("share:"), report.ShareTag, report.SharePath)
	fmt.Printf("Boot it with %s\n", command("br boot "+report.cartridgeArg()))
	return nil
}

// cartridgeArg returns the path a user would pass to `br boot`: the DMG when
// shipped (the AirDrop form), else the sparse image.
func (r cartridgePackReport) cartridgeArg() string {
	if r.DMG != "" {
		return r.DMG
	}
	return r.Cartridge
}

// layoutCartridge materializes the bootable root.img into a mounted cartridge
// and then writes the cartridge layout around it (packed disk.json, state/ +
// share/, the format stamp).
//
// The root.img half stays here because it needs the host's image cache and
// qemu-img; everything that defines the cartridge *shape* is cartridge.Pack, so
// the CLI and a holder process cannot drift apart.
//
// m is the SOURCE disk manifest (it names the image to materialize), while
// cartName is the CARTRIDGE's own name: the packed manifest and the on-image
// metadata are stamped with the latter, which is what cartridge.Detected
// resolveName reports and therefore what `br watch` offers the user.
func layoutCartridge(cmd *cobra.Command, mountpoint string, m *disk.Manifest, cartName string) error {
	// Resolve + materialize the bootable root image into the cartridge. We reuse
	// the exact image cache/convert path boot uses, so packed bytes == booted bytes.
	tmpCfg, err := config.Default("")
	if err != nil {
		return fmt.Errorf("prepare image config: %w", err)
	}
	tmpCfg.Arch = diskPackFlags.arch
	if err := m.ApplyTo(tmpCfg); err != nil {
		return fmt.Errorf("apply disk manifest: %w", err)
	}

	srcRaw, err := vm.EnsureBaseImage(cmd.Context(), tmpCfg)
	if err != nil {
		return fmt.Errorf("resolve disk image: %w", err)
	}
	diskGiB := pickDiskGiB(0, m.VM.DiskSizeGiB)
	if err := vm.MaterializeRawDisk(srcRaw, cartridge.NewLayout(mountpoint).RootImagePath(), diskGiB); err != nil {
		return fmt.Errorf("materialize root.img: %w", err)
	}

	return cartridge.Pack(mountpoint, m, cartridge.PackOptions{Name: cartName, PackedBy: version})
}

// --- runner boot <cartridge> --------------------------------------------

// bootCartridge is the handoff from `br boot <cartridge>` into runStart, which
// folds it into the vmhost.Spec and then hands the open cartridge itself to the
// Host (see takeBootCartridge). The Host owns it from that moment: it applies
// the rooting and detaches the image as the last step of its reverse-order
// teardown, once the VMM has released root.img.
//
// Everything a cartridge boot needs — the mount and its dev node, the packed
// manifest, the .dmg working copy, the layout — lives in the *cartridge.Opened
// value below rather than in loose package globals, so two cartridges can be
// open in one process. What is left here is only the argument-passing hop
// between two cobra RunE functions.
var bootCartridge struct {
	mountpoint string
	opened     *cartridge.Opened
}

// cartridgeMountpoint returns the private mountpoint for a cartridge name:
// <DefaultStateDir>/mnt/<name>. It is the mount target for the BUILD-side flows
// (`br disk pack`) and for a boot that asked for --private-mount: both need a
// deterministic location.
//
// The two therefore share one path, and that is the collision the browsable
// default was chosen to avoid — packing a cartridge while a --private-mount
// boot of the SAME NAME is live wants the same directory. It fails loudly
// (`hdiutil attach -mountpoint` refuses an occupied mountpoint) rather than
// quietly, and the browsable default keeps it off the ordinary path, so the
// flag documents it instead of the package preventing it.
func cartridgeMountpoint(name string) string {
	return cartridge.MountpointFor(config.DefaultStateDir(), name)
}

// cartridgeOpenOptions builds the cartridge.Open options for `br boot
// <cartridge>` from the two cartridge-only flags.
//
// Under the browsable default the mountpoint is left EMPTY on purpose: macOS
// picks the location (under /Volumes, where the user can eject it) and Open
// reports back where it landed. Predicting it here is exactly the mistake this
// file used to make. Only --private-mount dictates one, because MountPrivate
// requires it.
func cartridgeOpenOptions(name string, privateMount, persist bool) cartridge.OpenOptions {
	opts := cartridge.OpenOptions{
		Name:    name,
		Policy:  cartridge.MountPolicyFor(privateMount),
		Persist: persist,
	}
	if opts.Policy.Private() {
		opts.Mountpoint = cartridgeMountpoint(name)
	}
	return opts
}

// cartridgeImageKey reduces a cartridge image path to the identity two boots of
// the same cartridge share: the canonical path of the WORKING COPY they would
// both attach. It is what makes "demo.dmg" and "demo.sparseimage" — and the
// same file reached through a symlinked directory — one cartridge.
func cartridgeImageKey(path string) string {
	if path == "" {
		return ""
	}
	return cartridge.CanonicalImagePath(cartridge.WorkingCopyPath(path))
}

// bootedCartridgeInstance returns the live instance already booted from the same
// cartridge image as path, if any.
//
// It matches on the IMAGE, never on a mountpoint. A booted cartridge is mounted
// wherever macOS chose to put it, so "is <state>/mnt/<name> live?" — which is
// what this used to ask — is always false now, and every already-booted check
// built on it silently passed.
func bootedCartridgeInstance(path string) (instance.Entry, bool) {
	want := cartridgeImageKey(path)
	if want == "" {
		return instance.Entry{}, false
	}
	entries, err := instance.List(config.DefaultStateDir())
	if err != nil {
		logging.L().Debug("list instance registry for boot", "err", err)
	}
	for i := range entries {
		e := &entries[i]
		if !instance.Alive(*e) {
			continue
		}
		if cartridgeImageKey(e.SourcePath) == want || cartridgeImageKey(e.WorkingCopy) == want {
			return *e, true
		}
	}
	return instance.Entry{}, false
}

// errCartridgeAlreadyBooted is the sentinel behind every "that cartridge is
// already running" refusal, so callers can recognize one.
var errCartridgeAlreadyBooted = errors.New("cartridge is already booted")

// ensureCartridgeBootable refuses a boot of a cartridge that is already running,
// with a message naming the instance to eject.
//
// Two checks, in order of friendliness. The registry knows the instance NAME,
// which is what the user has to type next; the on-image claim knows about a
// holder that has not published an entry yet (or one started by a build that
// does not publish at all). Neither is the protection — cartridge.Open takes an
// exclusive claim, and that is what actually makes the race safe — they exist
// so the common case reads as a sentence instead of a lock error.
func ensureCartridgeBootable(path, name string) error {
	if e, ok := bootedCartridgeInstance(path); ok {
		return fmt.Errorf("%w: %q is running as instance %q (pid %d); eject it with 'br eject %s'",
			errCartridgeAlreadyBooted, name, e.Name, e.PID, e.Name)
	}
	if holder, busy := cartridge.Busy(path); busy {
		return fmt.Errorf("%w: %q is held by %s; eject it first",
			errCartridgeAlreadyBooted, name, holder)
	}
	return nil
}

// runBootCartridge boots a .sparseimage/.dmg cartridge.
//
// Headless — the ordinary case — the CLI does NOT open the cartridge. The
// holder does, because the holder is what will own the mount for as long as the
// VM runs: a mount opened here would be claimed by a process the user is about
// to close, and releasing that claim while the VM ran is precisely the
// stranding this design removes. So everything the boot needs travels in the
// Spec — the image, --persist, --private-mount and the sizing flags the user
// set — and the holder attaches, verifies and reads the packed manifest itself.
//
// With --gui the CLI opens it, because a GUI VM runs in this process (see
// runStart) and the Host it hands the cartridge to is right here.
func runBootCartridge(cmd *cobra.Command, args []string, path string) error {
	name := cartridge.NameFromPath(path)
	if !disk.ValidName(name) {
		return jsonOrError(fmt.Errorf("invalid cartridge name %q derived from %s", name, path))
	}
	if err := ensureCartridgeBootable(path, name); err != nil {
		return jsonOrError(err)
	}
	if bootFlags.gui {
		return runBootCartridgeForeground(cmd, args, path, name)
	}

	reportPlannedPersistence(path)
	spec := cartridgeBootSpec(cmd, path, name)
	if !jsonOutput {
		fmt.Printf("%s cartridge %s\n", subtle("Booting"), value(name))
	}
	return runStartUnderHolder(spec)
}

// cartridgeBootSpec describes a holder-run cartridge boot.
//
// Its sizing is NOT pre-resolved, which is the one structural difference from
// every other `br boot`: the recommended CPU/memory/disk numbers live in the
// manifest packed inside the image, and nothing has attached the image yet. So
// the spec is not Driven — it carries only the flags the user actually set, and
// the Host layers the cartridge's own manifest underneath them once it has the
// mount (see vmhost.Host.startConfig).
func cartridgeBootSpec(cmd *cobra.Command, path, name string) vmhost.Spec {
	spec := vmhost.Spec{
		Kind:          instance.KindCartridge,
		Name:          name,
		StateDir:      config.DefaultStateDir(),
		CartridgePath: path,
		Persist:       bootFlags.persist,
		MountPolicy:   cartridge.MountPolicyFor(bootFlags.privateMount),
		BinaryVersion: version,
	}
	if spec.MountPolicy.Private() {
		spec.Mountpoint = cartridgeMountpoint(name)
	}
	spec.Overrides, spec.ChangedFlags = bootOverrides(cmd)
	return spec
}

// bootOverrides maps the `br boot` flags the user explicitly set onto the
// `br start` override vocabulary a Spec speaks.
//
// Only flags that were CHANGED are reported, so the cartridge's own manifest
// and the persisted Settings keep their say over everything the user left
// alone. --headless is spelled as the "gui" override turned off, because that
// is the field it contradicts.
func bootOverrides(cmd *cobra.Command) (vmhost.Overrides, []string) {
	changed := func(name string) bool { return cmd != nil && cmd.Flags().Changed(name) }
	o := vmhost.Overrides{
		CPUs:        bootFlags.cpus,
		MemoryGiB:   bootFlags.memory,
		DiskSizeGiB: bootFlags.disk,
		Timeout:     bootFlags.timeout,
	}
	var names []string
	for flag, name := range map[string]string{
		"cpus": "cpus", "memory": "memory", "disk": "disk", "timeout": "timeout",
	} {
		if changed(flag) {
			names = append(names, name)
		}
	}
	if changed("headless") {
		o.GUI = false
		names = append(names, "gui")
	}
	sort.Strings(names)
	return o, names
}

// runBootCartridgeForeground is the pre-holder path, kept for --gui: the CLI
// opens the cartridge and hands it to a Host running in this very process,
// whose main thread the console window then takes.
func runBootCartridgeForeground(cmd *cobra.Command, args []string, path, name string) error {
	// No mountpoint is passed under the browsable default: macOS picks the
	// location (under /Volumes, visible and ejectable in Finder) and Open
	// reports back where it landed. --private-mount dictates one instead.
	opened, err := cartridge.Open(path, cartridgeOpenOptions(name, bootFlags.privateMount, bootFlags.persist))
	if err != nil {
		return jsonOrError(err)
	}
	reportPersistence(opened)

	// Stash for the foreground runStart, which hands the open cartridge to the
	// vmhost.Host: the Host roots cfg inside the mount as the last overlay of
	// its config step, and detachBootCartridge is the safety net that releases
	// the mount if the handoff never happens.
	bootCartridge.opened = opened
	bootCartridge.mountpoint = opened.Mountpoint()

	// Cartridges cold-boot by design (no host-bound RAM snapshot), so never set
	// restoreFrom. The mount roots state inside the cartridge.
	m := opened.Manifest
	startFlags.stateDir = opened.Mountpoint()
	startFlags.cpus = pickCPUs(bootFlags.cpus, m.VM.CPUs)
	startFlags.memory = pickMemoryGiB(bootFlags.memory, m.VM.MemoryGiB)
	startFlags.disk = pickDiskGiB(bootFlags.disk, m.VM.DiskSizeGiB)
	startFlags.gui = true
	startFlags.timeout = bootFlags.timeout
	startFlags.imageURL = ""
	startFlags.imagePath = ""
	startFlags.noNested = false
	startFlags.restoreFrom = ""

	// bootManifest stays nil: a cartridge boot carries its manifest inside
	// cartridge.Opened, and the vmhost.Host applies it (paths included) when it
	// overlays the cartridge onto the config.
	bootManifest = nil

	if !jsonOutput {
		fmt.Printf("%s cartridge %s (%s)\n", subtle("Booting"), value(name), disk.BootModeGUI)
	}
	return runStart(cmd, args)
}

// reportPlannedPersistence is reportPersistence for a boot that has not opened
// the cartridge — the holder will. It answers the same question at the same
// moment (before the guest starts, while canceling is free) from the only thing
// available here: the image's own extension, which is what decides whether
// there is a working copy to write back at all.
func reportPlannedPersistence(path string) {
	if !bootFlags.persist || jsonOutput {
		return
	}
	if cartridge.WritesBack(path, true) {
		fmt.Printf("  %s changes will be written back over %s once the guest powers off\n",
			key("persist:"), value(path))
		return
	}
	fmt.Printf("  %s %s is a runnable cartridge, so the guest writes into it directly; --persist changes nothing\n",
		key("persist:"), value(path))
}

// reportPersistence tells the user, before the guest even starts, what will
// happen to its changes when it powers off. --persist rewrites a file the user
// may have been handed by someone else, so the announcement belongs at the
// START of the run, while canceling is still free — not in the teardown
// output, where it would be news.
//
// --persist on a runnable .sparseimage is reported as the no-op it is: that
// file IS the disk the guest writes to, so there is nothing to write back.
func reportPersistence(opened *cartridge.Opened) {
	if !bootFlags.persist || jsonOutput || opened == nil {
		return
	}
	if opened.WritesBack() {
		fmt.Printf("  %s changes will be written back over %s once the guest powers off\n",
			key("persist:"), value(opened.SourcePath))
		return
	}
	fmt.Printf("  %s %s is a runnable cartridge, so the guest writes into it directly; --persist changes nothing\n",
		key("persist:"), value(opened.SourcePath))
}

// takeBootCartridge hands the open cartridge over to whoever will own it from
// here on (the vmhost.Host that runs the VM), clearing the stash so the
// deferred detachBootCartridge safety net in runStart becomes a no-op. Returns
// nil for a non-cartridge boot.
func takeBootCartridge() *cartridge.Opened {
	opened := bootCartridge.opened
	bootCartridge.opened = nil
	bootCartridge.mountpoint = ""
	return opened
}

// detachBootCartridge releases a cartridge image that was opened but never
// handed to a vmhost.Host — a spec rejected before the handoff, say. On the
// normal path takeBootCartridge has already cleared the stash and the Host owns
// the detach, so this is a no-op. It is also a no-op for a non-cartridge boot.
func detachBootCartridge() {
	opened := bootCartridge.opened
	if opened == nil {
		return
	}
	if err := opened.Close(); err != nil && !jsonOutput {
		fmt.Printf("%s detach cartridge %s: %v\n", warning("⚠"), opened.Name, err)
	}
	bootCartridge.opened = nil
	bootCartridge.mountpoint = ""
}

// --- cartridge listing for `br disks` --------------------------------

// cartridgeStatus describes an attached cartridge for `br disks`.
type cartridgeStatus struct {
	Name       string `json:"name"`
	Mountpoint string `json:"mountpoint"`
	Booted     bool   `json:"booted"`
}

// listAttachedCartridges reports every cartridge currently attached to this
// host along with its boot state (a live control socket => booted, else
// ejected/idle). Nothing attached yields an empty list (no error).
//
// Two sources, unioned and deduplicated by mountpoint:
//
//  1. the instance registry, which is the only thing that knows about a booted
//     cartridge now that one is mounted under /Volumes rather than at a path
//     bladerunner picked;
//  2. the legacy <state>/mnt scan, which still finds a privately mounted
//     cartridge (a scripted boot, or one attached by an older build).
//
// The registry comes first so a booted cartridge is reported under the name the
// user typed.
func listAttachedCartridges() []cartridgeStatus {
	root := config.DefaultStateDir()
	var out []cartridgeStatus
	seen := make(map[string]bool)

	add := func(name, mountpoint string) {
		if name == "" || mountpoint == "" {
			return
		}
		key := filepath.Clean(mountpoint)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, cartridgeStatus{
			Name:       name,
			Mountpoint: mountpoint,
			// Held, not pinged: a wedged holder replies to nothing and still owns
			// this cartridge, and reporting it as free invites a second boot of
			// the same image.
			Booted: instanceHeld(mountpoint),
		})
	}

	entries, err := instance.List(root)
	if err != nil {
		logging.L().Debug("list instance registry for disks", "err", err)
	}
	for i := range entries {
		e := &entries[i]
		if e.Kind != instance.KindCartridge || !instance.Alive(*e) {
			continue
		}
		mountpoint := e.Mountpoint
		if mountpoint == "" {
			// A cartridge instance is rooted AT its mountpoint.
			mountpoint = e.StateDir
		}
		add(e.Name, mountpoint)
	}
	for _, a := range cartridge.ListAttached(root) {
		add(a.Name, a.Mountpoint)
	}

	// Stay nil when there is nothing, so `br disks --json` keeps reporting null
	// rather than [].
	return out
}

// ejectTimeoutDuration is the CLI-side default eject wait, mirroring the control
// default but expressed as a Duration for the socket-gone wait.
const ejectTimeoutDuration = control.DefaultEjectTimeoutSeconds * time.Second
