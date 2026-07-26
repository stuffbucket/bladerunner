package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/disk"
	"github.com/stuffbucket/bladerunner/internal/vm"
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

  --out <file>   Output path (default: ./<name>.sparseimage)
  --ship         Also produce a compressed read-only <name>.dmg (the AirDrop form)
  --arch <arch>  Target architecture for the root image (default: host GOARCH)
  --size <GiB>   Cartridge capacity (default: disk size + headroom)

Requires macOS (hdiutil) and qemu-img.`,
	Args: cobra.ExactArgs(1),
	RunE: runDiskPack,
}

func init() {
	f := diskPackCmd.Flags()
	f.StringVar(&diskPackFlags.out, "out", "", "Output cartridge path (default: ./<name>.sparseimage)")
	f.BoolVar(&diskPackFlags.ship, "ship", false, "Also produce a compressed read-only <name>.dmg AirDrop artifact")
	f.StringVar(&diskPackFlags.arch, "arch", runtime.GOARCH, "Target architecture for the root image")
	f.IntVar(&diskPackFlags.size, "size", 0, "Cartridge capacity in GiB (default: disk size + headroom)")

	diskCmd.AddCommand(diskPackCmd)
}

// cartridgePackReport is the JSON result for `br disk pack`.
type cartridgePackReport struct {
	Status     string `json:"status"`
	Name       string `json:"name"`
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

// packOutPath resolves the output cartridge path: an explicit --out wins (with a
// .sparseimage extension ensured), else ./<name>.sparseimage in the cwd.
func packOutPath(flagOut, name string) (string, error) {
	if flagOut != "" {
		return flagOut, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	return filepath.Join(cwd, name+cartridge.SparseExt), nil
}

func runDiskPack(cmd *cobra.Command, args []string) error {
	name := args[0]
	if !disk.ValidName(name) {
		return jsonOrError(fmt.Errorf("invalid disk name %q: must be lowercase letters, digits, and dashes (start alphanumeric)", name))
	}

	cat, err := disk.LoadCatalog()
	if err != nil {
		return jsonOrError(fmt.Errorf("load disk catalog: %w", err))
	}
	entry, ok := cat.Lookup(name)
	if !ok {
		return jsonOrError(fmt.Errorf("unknown disk %q; %s", name, availableDisksHint(cat)))
	}
	m := entry.Manifest

	// Resolve sizing: the manifest's disk size (defaulted) drives the cartridge
	// capacity unless --size overrides it.
	diskGiB := pickDiskGiB(0, m.VM.DiskSizeGiB)
	sizeGiB := packSizeGiB(diskPackFlags.size, diskGiB)

	outPath, err := packOutPath(diskPackFlags.out, name)
	if err != nil {
		return jsonOrError(err)
	}

	if !jsonOutput {
		fmt.Printf("%s cartridge %s (%s, %d GiB) -> %s\n", subtle("Packing"), value(name), diskPackFlags.arch, sizeGiB, subtle(outPath))
	}

	// 1) Create the sparse image.
	imgPath, err := cartridge.Create(outPath, name, sizeGiB)
	if err != nil {
		return jsonOrError(fmt.Errorf("create cartridge image: %w", err))
	}

	// 2) Attach to a private mountpoint and lay out the cartridge.
	mountpoint := cartridgeMountpoint(name)
	mount, err := cartridge.Attach(imgPath, mountpoint)
	if err != nil {
		return jsonOrError(fmt.Errorf("attach cartridge: %w", err))
	}
	// Always detach when done (success or failure), so we never strand the image.
	packed := false
	defer func() {
		if derr := cartridge.Detach(mount.Mountpoint); derr != nil && !jsonOutput {
			fmt.Printf("%s detach cartridge: %v\n", warning("⚠"), derr)
		}
		if !packed {
			// A failed pack leaves a partial image; remove it so a retry is clean.
			_ = os.Remove(imgPath)
		}
	}()

	if err := layoutCartridge(cmd, mount.Mountpoint, m, name); err != nil {
		return jsonOrError(err)
	}

	packed = true

	report := cartridgePackReport{
		Status:    "packed",
		Name:      name,
		Cartridge: imgPath,
		SizeGiB:   sizeGiB,
		RootImg:   cartridge.RootImageFile,
		DiskGiB:   diskGiB,
		ShareTag:  cartridge.ShareTag(m),
		SharePath: cartridge.ShareGuestPath(m),
	}

	// 3) Optionally ship: compress to a read-only DMG (the AirDrop artifact). The
	// image must be detached before convert reads it, so run the detach now.
	if diskPackFlags.ship {
		if derr := cartridge.Detach(mount.Mountpoint); derr != nil {
			return jsonOrError(fmt.Errorf("detach before ship: %w", derr))
		}
		dmgStem := cartridge.TrimExt(imgPath)
		dmgPath, derr := cartridge.ConvertToDMG(imgPath, dmgStem)
		if derr != nil {
			return jsonOrError(fmt.Errorf("convert cartridge to dmg: %w", derr))
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
func layoutCartridge(cmd *cobra.Command, mountpoint string, m *disk.Manifest, name string) error {
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

	return cartridge.Pack(mountpoint, m, cartridge.PackOptions{Name: name, PackedBy: version})
}

// --- runner boot <cartridge> --------------------------------------------

// bootCartridge is the handoff from `br boot <cartridge>` into the foreground
// runStart: applyBootCartridge roots cfg inside the mount, and
// detachBootCartridge (the last deferred cleanup in runStart) releases the
// image after the VMM has stopped.
//
// Everything a cartridge boot needs — the mount and its dev node, the packed
// manifest, the .dmg working copy, the layout — now lives in the
// *cartridge.Opened value below rather than in loose package globals, so two
// cartridges can be open in one process. The mountpoint mirror survives only
// because start.go reads it directly to detect a driven start; extracting the
// VM lifecycle replaces this handoff with an explicit parameter.
var bootCartridge struct {
	mountpoint string
	opened     *cartridge.Opened
}

// cartridgeMountpoint returns the private mountpoint for a cartridge name:
// <DefaultStateDir>/mnt/<name>. Not under /Volumes so the cartridge is invisible
// in Finder and isolated per name.
func cartridgeMountpoint(name string) string {
	return cartridge.MountpointFor(config.DefaultStateDir(), name)
}

// runBootCartridge boots a .sparseimage/.dmg cartridge. Opening it converts a
// shipped .dmg to a writable working copy (the read-only ship form stays
// pristine), attaches the image privately, and verifies its layout; the VM is
// then rooted inside the mount and the foreground runStart owns it — detaching
// on exit.
func runBootCartridge(cmd *cobra.Command, args []string, path string) error {
	name := cartridge.NameFromPath(path)
	if !disk.ValidName(name) {
		return jsonOrError(fmt.Errorf("invalid cartridge name %q derived from %s", name, path))
	}

	baseDir := cartridgeMountpoint(name)
	if control.NewClient(baseDir).IsRunning() {
		return jsonOrError(fmt.Errorf("cartridge %q is already booted (use 'br eject' first)", name))
	}

	opened, err := cartridge.Open(path, cartridge.OpenOptions{Mountpoint: baseDir, Name: name})
	if err != nil {
		return jsonOrError(err)
	}

	// Stash for the foreground runStart: applyBootCartridge roots cfg inside the
	// mount, detachBootCartridge releases it after the VMM stops.
	bootCartridge.opened = opened
	bootCartridge.mountpoint = opened.Mountpoint()

	guiMode := opened.GUI()
	switch {
	case bootFlags.gui:
		guiMode = true
	case bootFlags.headless:
		guiMode = false
	}

	// Cartridges cold-boot by design (no host-bound RAM snapshot), so never set
	// restoreFrom. The mount roots state inside the cartridge.
	m := opened.Manifest
	startFlags.stateDir = opened.Mountpoint()
	startFlags.cpus = pickCPUs(bootFlags.cpus, m.VM.CPUs)
	startFlags.memory = pickMemoryGiB(bootFlags.memory, m.VM.MemoryGiB)
	startFlags.disk = pickDiskGiB(bootFlags.disk, m.VM.DiskSizeGiB)
	startFlags.gui = guiMode
	startFlags.timeout = bootFlags.timeout
	startFlags.imageURL = ""
	startFlags.imagePath = ""
	startFlags.noNested = false
	startFlags.restoreFrom = ""

	// bootManifest stays nil: applyBootCartridge sets the cfg paths directly.
	bootManifest = nil

	if !jsonOutput {
		fmt.Printf("%s cartridge %s (%s)\n", subtle("Booting"), value(name), modeLabel(guiMode))
	}
	return runStart(cmd, args)
}

// applyBootCartridge roots cfg inside the mounted cartridge (root.img, state/,
// share/). No-op for a non-cartridge boot.
func applyBootCartridge(cfg *config.Config) {
	bootCartridge.opened.ApplyTo(cfg)
}

// detachBootCartridge releases the cartridge image the foreground boot owned. It
// runs as the LAST deferred cleanup in runStart — after runner.Stop() has torn
// the VMM down and released root.img — so the detach is not blocked by the VMM.
// No-op for a non-cartridge boot.
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

// listAttachedCartridges reports every cartridge attached under the state dir
// along with its boot state (a live control socket => booted, else
// ejected/idle). Nothing attached yields an empty list (no error).
func listAttachedCartridges() []cartridgeStatus {
	attached := cartridge.ListAttached(config.DefaultStateDir())
	if len(attached) == 0 {
		// Stay nil so `br disks --json` keeps reporting null, not [].
		return nil
	}
	out := make([]cartridgeStatus, 0, len(attached))
	for _, a := range attached {
		out = append(out, cartridgeStatus{
			Name:       a.Name,
			Mountpoint: a.Mountpoint,
			Booted:     control.NewClient(a.Mountpoint).IsRunning(),
		})
	}
	return out
}

// ejectTimeoutDuration is the CLI-side default eject wait, mirroring the control
// default but expressed as a Duration for the socket-gone wait.
const ejectTimeoutDuration = control.DefaultEjectTimeoutSeconds * time.Second
