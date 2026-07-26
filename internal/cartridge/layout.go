// Cartridge layout addressing: where each element of a mounted cartridge lives,
// how a cartridge is laid out at pack time, and how attached cartridges are
// discovered under a bladerunner state directory.
//
// Like version.go this is plain path arithmetic and file I/O over an
// already-mounted directory, so it is NOT gated on hostSupported() and stays
// unit-testable in Linux CI. Only ListAttached consults the platform (through
// IsAttached), because "is a disk image mounted here" is a kernel question.

package cartridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/disk"
)

// MountDirName is the directory under a bladerunner state dir where cartridges
// are attached: <stateDir>/mnt/<name>. It is deliberately not under /Volumes so
// a privately mounted cartridge stays invisible in Finder and isolated by name.
const MountDirName = "mnt"

// layoutDirPerm is the mode cartridge directories are created with. The share
// is a host<->guest exchange point, so it is world-readable by design.
const layoutDirPerm = 0o755

// layoutFilePerm is the mode the packed disk.json is written with.
const layoutFilePerm = 0o644

// Layout addresses the elements of a cartridge mounted at Mountpoint. It is a
// value: two Layouts for two different mounts coexist happily, which is what
// lets one process hold more than one cartridge.
type Layout struct {
	// Mountpoint is the root of the mounted cartridge volume.
	Mountpoint string
}

// NewLayout returns the Layout of a cartridge mounted at mountpoint.
func NewLayout(mountpoint string) Layout { return Layout{Mountpoint: mountpoint} }

// ManifestPath is the packed disk manifest (disk.json) at the volume root.
func (l Layout) ManifestPath() string { return filepath.Join(l.Mountpoint, ManifestFile) }

// MetadataPath is the cartridge self-description (cartridge.json).
func (l Layout) MetadataPath() string { return filepath.Join(l.Mountpoint, MetadataFile) }

// RootImagePath is the bootable raw root disk, booted in place (no copy).
func (l Layout) RootImagePath() string { return filepath.Join(l.Mountpoint, RootImageFile) }

// StateDir holds boot-time state: the EFI variable store and cloud-init seed.
func (l Layout) StateDir() string { return filepath.Join(l.Mountpoint, StateDirName) }

// CloudInitDir is the cloud-init seed directory under StateDir.
func (l Layout) CloudInitDir() string { return filepath.Join(l.StateDir(), CloudInitDirName) }

// EFIVarsPath is the EFI variable store under StateDir.
func (l Layout) EFIVarsPath() string { return filepath.Join(l.StateDir(), EFIVarsFile) }

// ShareDir is the read-write host<->guest VirtioFS share.
func (l Layout) ShareDir() string { return filepath.Join(l.Mountpoint, ShareDirName) }

// Dirs returns every directory a laid-out cartridge contains, parents first so
// the slice can be created in order.
func (l Layout) Dirs() []string {
	return []string{l.StateDir(), l.CloudInitDir(), l.ShareDir()}
}

// Create makes the cartridge's directories, which the boot path roots the VM
// under. It is idempotent.
func (l Layout) Create() error {
	for _, d := range l.Dirs() {
		if err := os.MkdirAll(d, layoutDirPerm); err != nil {
			return fmt.Errorf("create cartridge dir %s: %w", d, err)
		}
	}
	return nil
}

// LoadManifest reads and parses the cartridge's packed disk.json.
func (l Layout) LoadManifest() (*disk.Manifest, error) {
	path := l.ManifestPath()
	m, err := disk.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load cartridge manifest %s: %w", path, err)
	}
	return m, nil
}

// WriteManifest marshals m into the cartridge's disk.json.
func (l Layout) WriteManifest(m *disk.Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cartridge manifest: %w", err)
	}
	path := l.ManifestPath()
	if err := os.WriteFile(path, b, layoutFilePerm); err != nil {
		return fmt.Errorf("write cartridge manifest %s: %w", path, err)
	}
	return nil
}

// MountpointFor returns the private mountpoint a cartridge of the given name is
// attached at under stateDir: <stateDir>/mnt/<name>.
func MountpointFor(stateDir, name string) string {
	return filepath.Join(stateDir, MountDirName, name)
}

// TrimExt removes a trailing .sparseimage or .dmg extension from p, leaving
// every other path unchanged. It is how a cartridge image path becomes a stem
// for conversion output and a slot name.
func TrimExt(p string) string {
	if stem, ok := strings.CutSuffix(p, SparseExt); ok {
		return stem
	}
	return strings.TrimSuffix(p, DMGExt)
}

// NameFromPath derives a cartridge (and mount slot) name from an image path by
// trimming the cartridge extension from its basename. The result is not
// validated; callers that need a legal slot name check disk.ValidName.
func NameFromPath(p string) string {
	return TrimExt(filepath.Base(p))
}

// HasImageExt reports whether p names a cartridge image by extension: the
// runnable .sparseimage form or the shipped .dmg form. It is a syntactic test
// only — the path need not exist.
func HasImageExt(p string) bool {
	return strings.HasSuffix(p, SparseExt) || strings.HasSuffix(p, DMGExt)
}

// ShareTag returns the effective VirtioFS tag for a manifest's share,
// defaulting to config.DefaultShareTag when the manifest names none.
func ShareTag(m *disk.Manifest) string {
	if m != nil && m.Share != nil && m.Share.Tag != "" {
		return m.Share.Tag
	}
	return config.DefaultShareTag
}

// ShareGuestPath returns the effective in-guest mount path for a manifest's
// share, defaulting to config.DefaultShareGuestPath.
func ShareGuestPath(m *disk.Manifest) string {
	if m != nil && m.Share != nil && m.Share.GuestPath != "" {
		return m.Share.GuestPath
	}
	return config.DefaultShareGuestPath
}

// PackManifest rewrites a disk manifest for embedding in a cartridge: the image
// becomes the local root.img so a boot never re-downloads (and disk.json
// honestly describes a self-contained source), and a default read-write share
// is ensured when the source manifest names none. The source is cloned, never
// mutated.
func PackManifest(m *disk.Manifest, name string) *disk.Manifest {
	cp := m.Clone()
	cp.Name = name
	cp.Image = disk.ImageSpec{Path: RootImageFile}
	if cp.Share == nil {
		cp.Share = &disk.ShareSpec{
			Tag:       config.DefaultShareTag,
			GuestPath: config.DefaultShareGuestPath,
		}
	}
	return cp
}

// PackOptions configures how a cartridge layout is written.
type PackOptions struct {
	// Name is the cartridge name. The packed manifest is renamed to it and it
	// is recorded in the cartridge metadata.
	Name string
	// PackedBy records the bladerunner version doing the packing. It is
	// provenance for bug reports only; compatibility is gated on FormatVersion.
	PackedBy string
}

// Pack lays out a cartridge inside an already-mounted volume: the packed
// disk.json, the state/ (with its cloud-init seed dir) and share/ directories,
// and the cartridge.json format stamp.
//
// It deliberately does NOT materialize root.img: that needs the host's image
// cache and qemu-img, which the caller owns. Pack is the part a holder process
// and the CLI must agree on byte-for-byte.
func Pack(mountpoint string, m *disk.Manifest, opts PackOptions) error {
	l := NewLayout(mountpoint)
	if err := l.WriteManifest(PackManifest(m, opts.Name)); err != nil {
		return err
	}
	if err := l.Create(); err != nil {
		return err
	}
	return WriteMetadata(mountpoint, Metadata{Name: opts.Name, PackedBy: opts.PackedBy})
}

// Attached names a cartridge volume currently mounted under a state dir.
type Attached struct {
	// Name is the mount slot name (the directory under <stateDir>/mnt).
	Name string
	// Mountpoint is the directory the cartridge is mounted at.
	Mountpoint string
}

// ListAttached scans <stateDir>/mnt/* and reports every cartridge currently
// attached there. A missing or unreadable mnt directory yields no entries and
// no error: "nothing attached" is the normal state, not a failure. Off darwin
// the result is always empty, since nothing can be attached.
func ListAttached(stateDir string) []Attached {
	root := filepath.Join(stateDir, MountDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Attached
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mp := filepath.Join(root, e.Name())
		if !IsAttached(mp) {
			continue
		}
		out = append(out, Attached{Name: e.Name(), Mountpoint: mp})
	}
	return out
}
