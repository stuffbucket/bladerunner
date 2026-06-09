package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
	"github.com/stuffbucket/bladerunner/internal/util"
)

const (
	// defaultBakeSizeGiB is the working image size passed to build-guest-image.sh.
	defaultBakeSizeGiB = 8
	// defaultBakeTimeoutMin caps how long a bake build may run.
	defaultBakeTimeoutMin = 60
)

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
or verified via sidecar at boot). Use --from <disk> to fork an existing catalog
disk's image and sizing. --gui sets boot mode to "gui"; otherwise "headless".`,
	Args: cobra.ExactArgs(1),
	RunE: runDiskNew,
}

var diskBakeCmd = &cobra.Command{
	Use:   "bake <name>",
	Short: "Build a disk's qcow2 and record its SHA-256",
	Long: `Build a disk's guest qcow2 via scripts/build-guest-image.sh, then record the
resulting SHA-256 and image path back into the user manifest.

This is a host-side developer action: it requires bash, qemu-img, and the build
script's dependencies (libguestfs-tools, likely sudo). Builtin disks are
read-only; fork one first with 'br disk new <name> --from <builtin>'.`,
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
	agentBin   string
	size       int
	release    string
	timeoutMin int
}

func init() {
	diskNewCmd.Flags().StringVar(&diskNewFlags.from, "from", "", "Fork an existing catalog disk's image and sizing")
	diskNewCmd.Flags().BoolVar(&diskNewFlags.gui, "gui", false, "Set boot mode to gui (default: headless)")
	diskNewCmd.Flags().BoolVar(&diskNewFlags.force, "force", false, "Overwrite an existing manifest")
	diskNewCmd.Flags().StringVar(&diskNewFlags.arch, "arch", runtime.GOARCH, "Target architecture for the scaffold")
	diskNewCmd.Flags().IntVar(&diskNewFlags.size, "size", config.DefaultDiskSizeGiB, "Disk size in GiB written into the manifest")

	diskBakeCmd.Flags().StringVar(&diskBakeFlags.output, "output", "", "Output qcow2 path (default: <disks-dir>/<name>-<arch>.qcow2)")
	diskBakeCmd.Flags().StringVar(&diskBakeFlags.arch, "arch", runtime.GOARCH, "Target architecture to build")
	diskBakeCmd.Flags().StringVar(&diskBakeFlags.agentBin, "br-agent-binary", "", "Optional br-agent binary to bake into the image")
	diskBakeCmd.Flags().IntVar(&diskBakeFlags.size, "size", defaultBakeSizeGiB, "Working image size in GiB passed to the build script")
	diskBakeCmd.Flags().StringVar(&diskBakeFlags.release, "debian-release", "trixie", "Debian release to build from")
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

// writeManifest marshals m and writes it to path (0o644), mirroring oidc.Store.Add.
func writeManifest(path string, m *disk.Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal disk: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write disk %s: %w", path, err)
	}
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
	} else {
		armURL, _ := config.DebianTrixieGenericCloudURL("arm64")
		amdURL, _ := config.DebianTrixieGenericCloudURL("amd64")
		m = &disk.Manifest{
			Name:        name,
			Description: "User disk scaffolded from Debian Trixie genericcloud.",
			Version:     time.Now().Format("2006.01.02"),
			Image: disk.ImageSpec{
				Arches: map[string]disk.ArchImage{
					"arm64": {URL: armURL},
					"amd64": {URL: amdURL},
				},
			},
			VM:   disk.VMSpec{DiskSizeGiB: diskNewFlags.size},
			Boot: disk.BootSpec{Mode: mode},
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

// runDiskBake builds the disk's qcow2 and records its SHA-256. The branches are
// sequential preflight + shell-out + manifest rewrite, not nested logic.
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

	// Preflight tools.
	if _, err := exec.LookPath("bash"); err != nil {
		return jsonOrError(fmt.Errorf("bash not found in PATH (required to run the build script): %w", err))
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return jsonOrError(fmt.Errorf("qemu-img not found in PATH (install with: brew install qemu): %w", err))
	}

	scriptPath, err := resolveBuildScript()
	if err != nil {
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
		fmt.Println(subtle("This is a host-side dev build; it needs libguestfs-tools and likely sudo."))
	}

	build := exec.CommandContext(ctx, "bash", scriptPath,
		"--arch", arch,
		"--output", absOut,
		"--size", strconv.Itoa(diskBakeFlags.size),
		"--debian-release", diskBakeFlags.release)
	if diskBakeFlags.agentBin != "" {
		build.Args = append(build.Args, "--br-agent-binary", diskBakeFlags.agentBin)
	}
	build.Stderr = os.Stderr // script logs go to stderr; stdout is the bare digest
	out, err := build.Output()
	if err != nil {
		return jsonOrError(fmt.Errorf("build-guest-image.sh failed: %w", err))
	}

	digest := strings.TrimSpace(string(out))
	if digest == "" {
		// Fallback: parse the sidecar the script also writes.
		if sidecar, rerr := os.ReadFile(absOut + ".sha256"); rerr == nil {
			fields := strings.Fields(string(sidecar))
			if len(fields) > 0 {
				digest = fields[0]
			}
		}
	}
	if !disk.ValidSHA256(digest) {
		return jsonOrError(fmt.Errorf("build script produced an invalid sha256 %q", digest))
	}

	// Record the result back into the manifest. If the disk uses per-arch
	// images, point this arch at the freshly built file + digest. If it is a
	// hosted/path disk, only stamp the digest is not meaningful, so refuse.
	if m.Image.Arches == nil {
		return jsonOrError(fmt.Errorf("disk %q is not a per-arch image disk; bake only supports image.arches disks", name))
	}
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

// resolveBuildScript locates scripts/build-guest-image.sh relative to the
// executable or the current working directory (it is a dev-time host script).
func resolveBuildScript() (string, error) {
	const rel = "scripts/build-guest-image.sh"
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, rel),
			filepath.Join(dir, "..", rel),
			filepath.Join(dir, "..", "..", rel),
		)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, rel))
	}
	candidates = append(candidates, rel)
	for _, c := range candidates {
		if util.FileExists(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("could not find %s near the executable or cwd; run 'br disk bake' from the bladerunner repo (it needs the build script and libguestfs-tools)", rel)
}
