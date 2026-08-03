package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
	"github.com/stuffbucket/bladerunner/internal/imagebuild"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/util"
	"github.com/stuffbucket/bladerunner/internal/vm"
)

const (
	// defaultBakeSizeGiB is the size the working image is grown to before the
	// recipe is applied.
	defaultBakeSizeGiB = 8
	// defaultBakeTimeoutMin caps how long a bake build may run.
	defaultBakeTimeoutMin = 60
	// manifestFilePerm is the mode a user disk manifest is published with. A
	// manifest names an image and its sizing; it carries no secret.
	manifestFilePerm = 0o644
)

// scaffoldArchList is every architecture the pinned Debian genericcloud image
// is published for, and so what `br disk new` writes when --arch is not given.
var scaffoldArchList = []string{"arm64", "amd64"}

// diskSlotDir returns the per-disk state slot baseDir, isolated from the flat
// default layout: <DefaultStateDir>/disks/<name>. Passing it to config.Default
// roots disk.raw, saved-state.bin, console.log, efivars, cloud-init, oidc, and
// the control socket (via VMDir) inside the slot, so a whole disk+memory slot is
// isolated with zero per-field surgery.
func diskSlotDir(name string) string {
	return filepath.Join(config.DefaultStateDir(), "disks", name)
}

// savedStatePath returns the saved-RAM file path inside a disk's slot.
func savedStatePath(baseDir string) string {
	return filepath.Join(baseDir, "saved-state.bin")
}

var disksCmd = &cobra.Command{
	Use:   "disks",
	Short: "List the disk shelf (available .disk manifests)",
	Long: `List every disk bladerunner knows about: the embedded builtins and any
user disks in ~/.config/bladerunner/disks/*.disk.

Each disk shows its boot mode, origin (builtin/user), and whether its per-disk
state slot holds saved guest RAM ("saved", restorable with 'br boot') or is
fresh.`,
	RunE: runDisks,
}

// diskReport is the JSON shape for one row of `br disks`.
type diskReport struct {
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	Mode          string `json:"mode"`
	Origin        string `json:"origin"`
	HasSavedState bool   `json:"has_saved_state"`
	Slot          string `json:"slot"`
}

func runDisks(_ *cobra.Command, _ []string) error {
	cat, err := disk.LoadCatalog()
	if err != nil {
		return jsonOrError(fmt.Errorf("load disk catalog: %w", err))
	}

	entries := cat.List()
	reports := make([]diskReport, 0, len(entries))
	for _, e := range entries {
		slot := diskSlotDir(e.Manifest.Name)
		reports = append(reports, diskReport{
			Name:          e.Manifest.Name,
			Description:   e.Manifest.Description,
			Mode:          e.Manifest.Boot.Mode,
			Origin:        e.Origin,
			HasSavedState: util.FileExists(savedStatePath(slot)),
			Slot:          slot,
		})
	}

	cartridges := listAttachedCartridges()

	if jsonOutput {
		return emitJSON(map[string]any{
			"disks":      reports,
			"cartridges": cartridges,
		})
	}

	if len(reports) == 0 && len(cartridges) == 0 {
		fmt.Println(subtle("No disks available."))
		fmt.Printf("Create one with %s\n", command("br disk new <name>"))
		return nil
	}

	if len(reports) > 0 {
		fmt.Println(title("Disk Shelf"))
		fmt.Println()
		for _, r := range reports {
			state := subtle("fresh")
			if r.HasSavedState {
				state = success("saved")
			}
			fmt.Printf("  %s  %s\n", value(r.Name), state)
			if r.Description != "" {
				fmt.Printf("    %s %s\n", key("about:"), r.Description)
			}
			fmt.Printf("    %s  %s   %s %s\n", key("mode:"), r.Mode, key("origin:"), r.Origin)
		}
		fmt.Println()
	}

	if len(cartridges) > 0 {
		fmt.Println(title("Attached Cartridges"))
		fmt.Println()
		for _, c := range cartridges {
			state := subtle("ejected")
			if c.Booted {
				state = success("booted")
			}
			fmt.Printf("  %s  %s\n", value(c.Name), state)
			fmt.Printf("    %s %s\n", key("mount:"), subtle(c.Mountpoint))
		}
		fmt.Println()
	}

	fmt.Printf("Boot one with %s\n", command("br boot <name|cartridge>"))
	return nil
}

// --- runner disk (parent) + disk new / disk bake -------------------------

var diskCmd = &cobra.Command{
	Use:   "disk",
	Short: "Author disk manifests (new, bake)",
	Long: `Author bladerunner disk manifests.

A disk is a ".disk" JSON manifest bundling an image identity, VM sizing, and a
boot mode. Use 'br disk new' to scaffold one and 'br disk bake' to build
its qcow2 and record the image's SHA-256. List disks with 'br disks' and
power one on with 'br boot <name>'.

User disks live in ~/.config/bladerunner/disks/*.disk.`,
}

var diskNewCmd = &cobra.Command{
	Use:   "new <name>",
	Short: "Scaffold a new user disk manifest",
	Long: `Scaffold a new ".disk" manifest in ~/.config/bladerunner/disks/.

By default the disk targets the Debian Trixie genericcloud image for both
arm64/amd64 with empty SHA-256 digests (filled in later by 'br disk bake',
or verified via sidecar at boot). Use --arch to scaffold one architecture
alone. Use --from <disk> to fork an existing catalog disk's image and sizing;
--size then overrides the forked size and --arch narrows the forked images.
--gui sets boot mode to "gui"; otherwise "headless".`,
	Args: cobra.ExactArgs(1),
	RunE: runDiskNew,
}

var diskBakeCmd = &cobra.Command{
	Use:   "bake <name>",
	Short: "Build a disk's qcow2 and record its SHA-256",
	Long: `Build a disk's guest qcow2, then record the resulting SHA-256 and image
path back into the user manifest.

This is a host-side developer action. It needs qemu-img, and the mechanic it
selects decides the rest: the native path wants Linux, root and an nbd device;
the libguestfs path wants neither root nor a device but does want a working
appliance. Builtin disks are read-only; fork one first with
'br disk new <name> --from <builtin>'.`,
	Args: cobra.ExactArgs(1),
	RunE: runDiskBake,
}

var diskNewFlags struct {
	from  string
	gui   bool
	force bool
	arch  string
	size  int
}

var diskBakeFlags struct {
	output     string
	arch       string
	size       int
	release    string
	method     string
	timeoutMin int
}

func init() {
	diskNewCmd.Flags().StringVar(&diskNewFlags.from, "from", "", "Fork an existing catalog disk's image and sizing")
	diskNewCmd.Flags().BoolVar(&diskNewFlags.gui, "gui", false, "Set boot mode to gui (default: headless)")
	diskNewCmd.Flags().BoolVar(&diskNewFlags.force, "force", false, "Overwrite an existing manifest")
	diskNewCmd.Flags().StringVar(&diskNewFlags.arch, "arch", "", "Scaffold this architecture alone (default: every architecture the image publishes)")
	diskNewCmd.Flags().IntVar(&diskNewFlags.size, "size", 0, fmt.Sprintf("Disk size in GiB written into the manifest (default: the --from disk's size, else %d)", config.DefaultDiskSizeGiB))

	diskBakeCmd.Flags().StringVar(&diskBakeFlags.output, "output", "", "Output qcow2 path (default: <disks-dir>/<name>-<arch>.qcow2)")
	diskBakeCmd.Flags().StringVar(&diskBakeFlags.arch, "arch", runtime.GOARCH, "Target architecture to build")
	diskBakeCmd.Flags().IntVar(&diskBakeFlags.size, "size", defaultBakeSizeGiB, "Working image size in GiB passed to the build script")
	diskBakeCmd.Flags().StringVar(&diskBakeFlags.release, "debian-release", "trixie", "Debian release to build from")
	diskBakeCmd.Flags().StringVar(&diskBakeFlags.method, "method", string(imagebuild.MethodAuto),
		fmt.Sprintf("Customize method: %s|%s|%s|%s (%s prefers the fastest mechanic that will actually work here)",
			imagebuild.MethodAuto, imagebuild.MethodNative, imagebuild.MethodAppliance, imagebuild.MethodVM, imagebuild.MethodAuto))
	diskBakeCmd.Flags().IntVar(&diskBakeFlags.timeoutMin, "timeout", defaultBakeTimeoutMin, "Build timeout in minutes")

	diskCmd.AddCommand(diskNewCmd, diskBakeCmd)
}

// diskActionReport is the JSON result for disk new / disk bake.
type diskActionReport struct {
	Status string `json:"status"`
	Name   string `json:"name"`
	Path   string `json:"path,omitempty"`
	Arch   string `json:"arch,omitempty"`
	Output string `json:"output,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// writeManifest marshals m and publishes it to path.
//
// The write goes through internal/util, the owner of atomic file writes: a
// plain os.WriteFile opens O_TRUNC, so a `br disk ls` racing a rewrite would
// read a truncated manifest and drop the disk from the catalog.
func writeManifest(path string, m *disk.Manifest) error {
	if err := util.WriteJSONAtomic(path, m, manifestFilePerm); err != nil {
		return fmt.Errorf("write disk %s: %w", path, err)
	}
	return nil
}

// scaffoldArches returns the image.arches map a new disk is scaffolded with:
// the one architecture --arch named, or every architecture the pinned Debian
// genericcloud image is published for.
//
// A scaffold used to write both entries unconditionally, which made --arch a
// flag with no read site at all. Defaulting to "every arch" keeps the portable
// manifest that behavior produced, while an explicit --arch now narrows it —
// and an architecture with no published image is refused here rather than
// written as an empty URL.
func scaffoldArches(arch string) (map[string]disk.ArchImage, error) {
	wanted := scaffoldArchList
	if arch != "" {
		wanted = []string{arch}
	}
	arches := make(map[string]disk.ArchImage, len(wanted))
	for _, a := range wanted {
		url, err := config.DebianTrixieGenericCloudURL(a)
		if err != nil {
			return nil, fmt.Errorf("--arch %s: %w", a, err)
		}
		arches[a] = disk.ArchImage{URL: url}
	}
	return arches, nil
}

// narrowToArch restricts a FORKED manifest's per-arch images to arch, so
// --arch means the same thing on both `br disk new` paths. A disk that carries
// no per-arch images (a hosted or path image) cannot honor it, so it says so
// rather than dropping the flag.
func narrowToArch(m *disk.Manifest, arch, from string) error {
	if arch == "" {
		return nil
	}
	if m.Image.Arches == nil {
		return fmt.Errorf("--arch %s cannot be applied to %q: it does not carry per-architecture images", arch, from)
	}
	img, ok := m.Image.Arches[arch]
	if !ok {
		return fmt.Errorf("--arch %s is not published by %q (it has %s)", arch, from, strings.Join(sortedArches(m), ", "))
	}
	m.Image.Arches = map[string]disk.ArchImage{arch: img}
	return nil
}

func runDiskNew(_ *cobra.Command, args []string) error {
	name := args[0]
	if !disk.ValidName(name) {
		return jsonOrError(fmt.Errorf("invalid disk name %q: must be lowercase letters, digits, and dashes (start alphanumeric)", name))
	}

	cat, err := disk.LoadCatalog()
	if err != nil {
		return jsonOrError(fmt.Errorf("load disk catalog: %w", err))
	}

	mode := disk.BootModeHeadless
	if diskNewFlags.gui {
		mode = disk.BootModeGUI
	}

	var m *disk.Manifest
	if diskNewFlags.from != "" {
		src, ok := cat.Lookup(diskNewFlags.from)
		if !ok {
			return jsonOrError(fmt.Errorf("--from disk %q not found (see 'br disks')", diskNewFlags.from))
		}
		m = src.Manifest.Clone()
		m.Name = name
		m.Boot.Mode = mode
		if err := narrowToArch(m, diskNewFlags.arch, diskNewFlags.from); err != nil {
			return jsonOrError(err)
		}
		// A fork keeps the sizing it was forked from unless --size says
		// otherwise; the flag used to be read on the scaffold path only, so it
		// was silently dropped here.
		m.VM.DiskSizeGiB = pickDiskGiB(diskNewFlags.size, m.VM.DiskSizeGiB)
	} else {
		arches, err := scaffoldArches(diskNewFlags.arch)
		if err != nil {
			return jsonOrError(err)
		}
		m = &disk.Manifest{
			Name:        name,
			Description: "User disk scaffolded from Debian Trixie genericcloud.",
			Version:     time.Now().Format("2006.01.02"),
			Image:       disk.ImageSpec{Arches: arches},
			VM:          disk.VMSpec{DiskSizeGiB: pickDiskGiB(diskNewFlags.size, 0)},
			Boot:        disk.BootSpec{Mode: mode},
		}
	}

	if err := m.Validate(); err != nil {
		return jsonOrError(fmt.Errorf("invalid disk: %w", err))
	}

	dir := disk.DefaultDiskDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return jsonOrError(fmt.Errorf("create disks dir: %w", err))
	}
	path := filepath.Join(dir, name+disk.ManifestExt)
	if !diskNewFlags.force {
		if _, statErr := os.Stat(path); statErr == nil {
			return jsonOrError(fmt.Errorf("disk %q already exists at %s (use --force to overwrite)", name, path))
		}
	}
	if err := writeManifest(path, m); err != nil {
		return jsonOrError(err)
	}

	if jsonOutput {
		return emitJSON(diskActionReport{Status: "created", Name: name, Path: path})
	}
	fmt.Printf("%s Created disk %s at %s\n", success("✓"), value(name), subtle(path))
	fmt.Printf("Build it with %s\n", command("br disk bake "+name))
	return nil
}

// runDiskBake builds the disk's qcow2 and records its SHA-256.
func runDiskBake(cmd *cobra.Command, args []string) error {
	name := args[0]
	if !disk.ValidName(name) {
		return jsonOrError(fmt.Errorf("invalid disk name %q", name))
	}
	arch := diskBakeFlags.arch

	// Bake mutates user files only; builtins are read-only.
	manifestPath := filepath.Join(disk.DefaultDiskDir(), name+disk.ManifestExt)
	if !util.FileExists(manifestPath) {
		if cat, err := disk.LoadCatalog(); err == nil {
			if e, ok := cat.Lookup(name); ok && e.Origin == disk.OriginBuiltin {
				return jsonOrError(fmt.Errorf("builtin disks are read-only; fork it first with 'br disk new %s --from %s'", name, name))
			}
		}
		return jsonOrError(fmt.Errorf("no user disk %q at %s (create it with 'br disk new %s')", name, manifestPath, name))
	}
	m, err := disk.Load(manifestPath)
	if err != nil {
		return jsonOrError(err)
	}

	// Refuse a disk bake cannot record into BEFORE anything is built. The
	// manifest shape is known the moment it is loaded, and this check used to
	// sit after the build had already downloaded, customized, compressed and
	// renamed a qcow2 into --output — so the user paid a full build for a
	// guaranteed refusal, and any file already at that path was replaced by an
	// image the command then declined to reference.
	if err := bakePreflight(m, name, arch); err != nil {
		return jsonOrError(err)
	}

	// Decide HOW to build before checking any one mechanic's tools. The probe
	// establishes what this host can actually do — root, a loop device, a
	// matching architecture, a libguestfs that really launches — so an
	// unusable fast path is reported up front with the specific blocking
	// condition, instead of failing halfway through a build.
	want := imagebuild.Method(diskBakeFlags.method)
	caps := imagebuild.Probe(cmd.Context(), want, arch)
	sel, err := imagebuild.Select(want, arch, caps)
	if err != nil {
		return jsonOrError(err)
	}
	for _, w := range sel.Warnings {
		logging.L().Warn("guest image build: falling back", "reason", w)
	}
	// Preflight the host tool the bake shells out to. Everything else it needs
	// is inside the mechanic, which the probe above has already vetted.
	if err := vm.RequireQemuImg(); err != nil {
		return jsonOrError(err)
	}

	outPath := diskBakeFlags.output
	if outPath == "" {
		outPath = filepath.Join(disk.DefaultDiskDir(), fmt.Sprintf("%s-%s.qcow2", name, arch))
	}
	absOut, err := filepath.Abs(outPath)
	if err != nil {
		return jsonOrError(fmt.Errorf("resolve output path: %w", err))
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), time.Duration(diskBakeFlags.timeoutMin)*time.Minute)
	defer cancel()

	if !jsonOutput {
		fmt.Printf("Baking %s (%s) -> %s\n", value(name), arch, subtle(absOut))
	}

	work, err := os.MkdirTemp("", "bladerunner-bake-")
	if err != nil {
		return jsonOrError(fmt.Errorf("create a work directory for the bake: %w", err))
	}
	defer func() { _ = os.RemoveAll(work) }()

	release, err := imagebuild.BaseRelease(arch)
	if err != nil {
		return jsonOrError(err)
	}
	recipe := imagebuild.DefaultRecipe(imagebuild.BuildVersion(time.Now()))
	plan, err := imagebuild.NewBakePlan(release, recipe, work, absOut, diskBakeFlags.size)
	if err != nil {
		return jsonOrError(err)
	}

	deps := imagebuild.LinuxBakeDeps(work, func(line string) {
		if !jsonOutput {
			fmt.Println(subtle("  " + line))
		}
	})
	if err := imagebuild.Bake(ctx, plan, deps); err != nil {
		return jsonOrError(fmt.Errorf("bake %s: %w", name, err))
	}

	digest, err := util.FileSHA256(absOut)
	if err != nil {
		return jsonOrError(err)
	}

	// Record the result back into the manifest. bakePreflight has already
	// established that this disk has a slot for this architecture, so the only
	// failures left here are ones the build itself produced.
	m.Image.Arches[arch] = disk.ArchImage{URL: "file://" + absOut, SHA256: digest}
	m.Version = time.Now().Format("2006.01.02")
	if err := m.Validate(); err != nil {
		return jsonOrError(fmt.Errorf("baked manifest invalid: %w", err))
	}
	if err := writeManifest(manifestPath, m); err != nil {
		return jsonOrError(err)
	}

	if jsonOutput {
		return emitJSON(diskActionReport{Status: "baked", Name: name, Arch: arch, Output: absOut, SHA256: digest})
	}
	fmt.Printf("%s Baked %s (%s): %s  %s%s\n",
		success("✓"), value(name), arch, subtle(absOut), key("sha256="), digest)
	return nil
}

// bakePreflight reports whether a bake could record its result into this disk,
// using only what the manifest already says. The caller runs it before the
// build; see the call site for why that ordering matters.
func bakePreflight(m *disk.Manifest, name, arch string) error {
	if m.Image.Arches == nil {
		return fmt.Errorf("disk %q is not a per-arch image disk; bake only supports image.arches disks", name)
	}
	if _, ok := m.Image.Arches[arch]; !ok {
		return fmt.Errorf("disk %q has no image.arches entry for %s (it has %s)",
			name, arch, strings.Join(sortedArches(m), ", "))
	}
	return nil
}

// sortedArches lists the architectures a manifest carries an image for, in a
// stable order so an error naming them reads the same way twice.
func sortedArches(m *disk.Manifest) []string {
	out := make([]string, 0, len(m.Image.Arches))
	for a := range m.Image.Arches {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}
