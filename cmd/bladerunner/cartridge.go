package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/disk"
	"github.com/stuffbucket/bladerunner/internal/vm"
)

// Cartridge on-image layout. A mounted cartridge exposes a complete, self-
// contained VM: the disk manifest, the bootable root disk, EFI + cloud-init
// state, and the RW host<->guest share folder.
const (
	cartridgeManifestFile = "disk.json"
	cartridgeRootImg      = "root.img"
	cartridgeStateDir     = "state"
	cartridgeShareDir     = "share"
	cartridgeEFIVarsFile  = "efi-vars.bin"
	cartridgeCloudInitDir = "cloud-init"
)

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

	rootImg := filepath.Join(mount.Mountpoint, cartridgeRootImg)
	shareTag := manifestShareTag(m)
	shareGuestPath := manifestShareGuestPath(m)

	if err := layoutCartridge(cmd, mount.Mountpoint, m, name, rootImg); err != nil {
		return jsonOrError(err)
	}

	packed = true

	report := cartridgePackReport{
		Status:    "packed",
		Name:      name,
		Cartridge: imgPath,
		SizeGiB:   sizeGiB,
		RootImg:   cartridgeRootImg,
		DiskGiB:   diskGiB,
		ShareTag:  shareTag,
		SharePath: shareGuestPath,
	}

	// 3) Optionally ship: compress to a read-only DMG (the AirDrop artifact). The
	// image must be detached before convert reads it, so run the detach now.
	if diskPackFlags.ship {
		if derr := cartridge.Detach(mount.Mountpoint); derr != nil {
			return jsonOrError(fmt.Errorf("detach before ship: %w", derr))
		}
		dmgStem := trimCartridgeExt(imgPath)
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
	fmt.Printf("  %s %s mounted at %s in the guest\n", key("share:"), shareTag, shareGuestPath)
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

// layoutCartridge writes the disk.json manifest, materializes root.img from the
// disk's image source, and creates the state/ and share/ directories inside a
// mounted cartridge.
func layoutCartridge(cmd *cobra.Command, mountpoint string, m *disk.Manifest, name, rootImg string) error {
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
	if err := vm.MaterializeRawDisk(srcRaw, rootImg, diskGiB); err != nil {
		return fmt.Errorf("materialize root.img: %w", err)
	}

	// The cartridge's disk.json points its image at the LOCAL root.img so boot
	// never re-downloads; sizing and share carry through.
	packed := cartridgeManifest(m, name)
	if err := writeManifest(filepath.Join(mountpoint, cartridgeManifestFile), packed); err != nil {
		return err
	}

	// State + share directories the boot path roots the VM under.
	for _, d := range []string{
		filepath.Join(mountpoint, cartridgeStateDir),
		filepath.Join(mountpoint, cartridgeStateDir, cartridgeCloudInitDir),
		filepath.Join(mountpoint, cartridgeShareDir),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create cartridge dir %s: %w", d, err)
		}
	}
	return nil
}

// cartridgeManifest rewrites a disk manifest for embedding in a cartridge: the
// image becomes the local root.img (Path, relative to the mountpoint at boot
// time we set BaseImagePath directly, but record root.img so the manifest is
// self-describing) and a default RW share is ensured when absent.
func cartridgeManifest(m *disk.Manifest, name string) *disk.Manifest {
	cp := m.Clone()
	cp.Name = name
	// The cartridge carries its own bootable root.img; record it as a path image
	// so disk.json honestly describes a local, self-contained source.
	cp.Image = disk.ImageSpec{Path: cartridgeRootImg}
	if cp.Share == nil {
		cp.Share = &disk.ShareSpec{
			Tag:       config.DefaultShareTag,
			GuestPath: config.DefaultShareGuestPath,
		}
	}
	return cp
}

// manifestShareTag returns the effective VirtioFS tag for a manifest's share,
// defaulting cartridges to config.DefaultShareTag.
func manifestShareTag(m *disk.Manifest) string {
	if m.Share != nil && m.Share.Tag != "" {
		return m.Share.Tag
	}
	return config.DefaultShareTag
}

// manifestShareGuestPath returns the effective in-guest mount path for a
// manifest's share, defaulting to config.DefaultShareGuestPath.
func manifestShareGuestPath(m *disk.Manifest) string {
	if m.Share != nil && m.Share.GuestPath != "" {
		return m.Share.GuestPath
	}
	return config.DefaultShareGuestPath
}

// --- runner boot <cartridge> --------------------------------------------

// bootCartridge stashes the attached cartridge state for the foreground runStart:
// applyBootCartridge roots cfg inside the mount, and detachBootCartridge (a
// deferred cleanup in runStart) releases the image after the VMM has stopped.
var bootCartridge struct {
	mountpoint string
	manifest   *disk.Manifest
	name       string
	// workingCopy is a temporary .sparseimage materialized from a shipped .dmg;
	// removed after detach so a pristine .dmg is never mutated.
	workingCopy string
}

// cartridgeMountpoint returns the private mountpoint for a cartridge name:
// <DefaultStateDir>/mnt/<name>. Not under /Volumes so the cartridge is invisible
// in Finder and isolated per name.
func cartridgeMountpoint(name string) string {
	return filepath.Join(config.DefaultStateDir(), "mnt", name)
}

// cartridgeNameFromPath derives a slot/mount name from a cartridge file path by
// trimming the cartridge extension from its basename.
func cartridgeNameFromPath(p string) string {
	return trimCartridgeExt(filepath.Base(p))
}

// trimCartridgeExt removes a trailing .sparseimage or .dmg extension from p.
func trimCartridgeExt(p string) string {
	if stem, ok := strings.CutSuffix(p, cartridge.SparseExt); ok {
		return stem
	}
	return strings.TrimSuffix(p, cartridge.DMGExt)
}

// runBootCartridge boots a .sparseimage/.dmg cartridge. A .dmg is first converted
// to a writable working .sparseimage (the read-only ship form stays pristine);
// the image is attached privately, the VM is rooted inside it, and the foreground
// runStart owns the mount — detaching it on exit.
func runBootCartridge(cmd *cobra.Command, args []string, path string) error {
	name := cartridgeNameFromPath(path)
	if !disk.ValidName(name) {
		return jsonOrError(fmt.Errorf("invalid cartridge name %q derived from %s", name, path))
	}

	baseDir := cartridgeMountpoint(name)
	if control.NewClient(baseDir).IsRunning() {
		return jsonOrError(fmt.Errorf("cartridge %q is already booted (use 'br eject' first)", name))
	}

	bootImg := path
	var workingCopy string
	if filepath.Ext(path) == cartridge.DMGExt {
		// Materialize a writable working copy next to the DMG so the shipped,
		// read-only artifact is never mutated. Clear any stale copy first (a prior
		// boot that crashed before detach could have left one; hdiutil convert
		// refuses to overwrite), so re-booting a .dmg always works.
		work := trimCartridgeExt(path)
		_ = os.Remove(work + cartridge.SparseExt)
		converted, err := cartridge.ConvertToSparse(path, work)
		if err != nil {
			return jsonOrError(fmt.Errorf("convert cartridge dmg to working copy: %w", err))
		}
		bootImg = converted
		workingCopy = converted
	}

	mount, err := cartridge.Attach(bootImg, baseDir)
	if err != nil {
		if workingCopy != "" {
			_ = os.Remove(workingCopy)
		}
		return jsonOrError(fmt.Errorf("attach cartridge: %w", err))
	}

	manifestPath := filepath.Join(mount.Mountpoint, cartridgeManifestFile)
	m, err := disk.Load(manifestPath)
	if err != nil {
		_ = cartridge.Detach(mount.Mountpoint)
		if workingCopy != "" {
			_ = os.Remove(workingCopy)
		}
		return jsonOrError(fmt.Errorf("load cartridge manifest %s: %w", manifestPath, err))
	}

	// Stash for the foreground runStart: applyBootCartridge roots cfg inside the
	// mount, detachBootCartridge releases it after the VMM stops.
	bootCartridge.mountpoint = mount.Mountpoint
	bootCartridge.manifest = m
	bootCartridge.name = name
	bootCartridge.workingCopy = workingCopy

	guiMode := m.Boot.Mode == disk.BootModeGUI
	switch {
	case bootFlags.gui:
		guiMode = true
	case bootFlags.headless:
		guiMode = false
	}

	// Cartridges cold-boot by design (no host-bound RAM snapshot), so never set
	// restoreFrom. The mount roots state inside the cartridge.
	startFlags.stateDir = mount.Mountpoint
	startFlags.cpus = pickCPUs(bootFlags.cpus, m.VM.CPUs)
	startFlags.memory = pickMemoryGiB(bootFlags.memory, m.VM.MemoryGiB)
	startFlags.disk = pickDiskGiB(bootFlags.disk, m.VM.DiskSizeGiB)
	startFlags.gui = guiMode
	startFlags.timeout = bootFlags.timeout
	startFlags.imageURL = ""
	startFlags.imagePath = ""
	startFlags.useAgent = false
	startFlags.noAgent = false
	startFlags.noNested = false
	startFlags.restoreFrom = ""

	// bootManifest stays nil: applyBootCartridge sets the cfg paths directly.
	bootManifest = nil

	if !jsonOutput {
		fmt.Printf("%s cartridge %s (%s)\n", subtle("Booting"), value(name), modeLabel(guiMode))
	}
	return runStart(cmd, args)
}

// applyBootCartridge roots cfg inside the mounted cartridge: the bootable
// root.img, EFI + cloud-init state under state/, and the RW share under share/.
// No-op for a non-cartridge boot (bootCartridge.mountpoint == "").
func applyBootCartridge(cfg *config.Config) {
	mp := bootCartridge.mountpoint
	if mp == "" {
		return
	}
	m := bootCartridge.manifest

	cfg.BaseImagePath = filepath.Join(mp, cartridgeRootImg)
	cfg.BaseImageURL = ""
	cfg.BaseImageSHA512 = ""
	cfg.BaseImageExpectedSHA256 = ""
	// The materialized root.img is already the resized disk; DiskPath IS root.img
	// so the VM boots the cartridge's disk in place (no copy/resize on boot).
	cfg.DiskPath = filepath.Join(mp, cartridgeRootImg)

	state := filepath.Join(mp, cartridgeStateDir)
	cfg.EFIVarsPath = filepath.Join(state, cartridgeEFIVarsFile)
	cfg.CloudInitDir = filepath.Join(state, cartridgeCloudInitDir)

	// The RW host<->guest share lives inside the cartridge.
	cfg.ShareDir = filepath.Join(mp, cartridgeShareDir)
	cfg.ShareTag = manifestShareTag(m)
	cfg.ShareGuestPath = manifestShareGuestPath(m)
}

// detachBootCartridge releases the cartridge image the foreground boot owned. It
// runs as the LAST deferred cleanup in runStart — after runner.Stop() has torn
// the VMM down and released root.img — so the detach is not blocked by the VMM.
// No-op for a non-cartridge boot.
func detachBootCartridge() {
	mp := bootCartridge.mountpoint
	if mp == "" {
		return
	}
	if err := cartridge.Detach(mp); err != nil && !jsonOutput {
		fmt.Printf("%s detach cartridge %s: %v\n", warning("⚠"), bootCartridge.name, err)
	}
	if bootCartridge.workingCopy != "" {
		_ = os.Remove(bootCartridge.workingCopy)
	}
}

// --- cartridge listing for `br disks` --------------------------------

// cartridgeStatus describes an attached cartridge for `br disks`.
type cartridgeStatus struct {
	Name       string `json:"name"`
	Mountpoint string `json:"mountpoint"`
	Booted     bool   `json:"booted"`
}

// listAttachedCartridges scans <DefaultStateDir>/mnt/* for attached cartridges
// and reports each one's boot state (a live control socket => booted, else
// ejected/idle). A missing mnt dir yields an empty list (no error).
func listAttachedCartridges() []cartridgeStatus {
	mntRoot := filepath.Join(config.DefaultStateDir(), "mnt")
	entries, err := os.ReadDir(mntRoot)
	if err != nil {
		return nil
	}
	var out []cartridgeStatus
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mp := filepath.Join(mntRoot, e.Name())
		if !cartridge.IsAttached(mp) {
			continue
		}
		out = append(out, cartridgeStatus{
			Name:       e.Name(),
			Mountpoint: mp,
			Booted:     control.NewClient(mp).IsRunning(),
		})
	}
	return out
}

// ejectTimeoutDuration is the CLI-side default eject wait, mirroring the control
// default but expressed as a Duration for the socket-gone wait.
const ejectTimeoutDuration = control.DefaultEjectTimeoutSeconds * time.Second
