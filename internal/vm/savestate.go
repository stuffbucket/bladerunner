package vm

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"

	"github.com/stuffbucket/bladerunner/internal/util"
)

// saveGenerationMode is the mode of the files in a saved-state generation: the
// sidecar, and any staged copy of the state file a transfer has to make. Both
// carry the private state of a guest — its host paths, and its RAM — so both
// stay owner-only.
const saveGenerationMode = 0o600

// SaveMetadata is the sidecar written next to a VZ saved-state file. It records
// the hardware configuration the snapshot requires — so a restore rebuilds a
// matching VZ configuration without the operator re-specifying it — plus a fast
// identity stamp of the disk image, so restoring against a disk that changed
// since the snapshot is refused rather than silently corrupting the guest.
type SaveMetadata struct {
	CPUs        uint   `json:"cpus"`
	MemoryGiB   uint64 `json:"memory_gib"`
	DiskSizeGiB int    `json:"disk_size_gib"`
	DiskPath    string `json:"disk_path"`

	// GUI records whether the snapshot was taken with a graphics device attached.
	// Graphics devices are fixed at VZ-config-build time, so restoring with a
	// different mode yields a mismatched device topology and a confusing VZ
	// failure; the restore path refuses it with an actionable error instead. A
	// pointer so a sidecar written before this field (nil) skips the check.
	GUI *bool `json:"gui,omitempty"`

	// ShareTag records the VirtioFS directory-sharing device tag the snapshot was
	// taken with ("" when no share device was attached). Like graphics, the
	// directory-sharing topology is fixed at VZ-config-build time, so a mismatched
	// restore (share present vs absent, or a different tag) yields a confusing VZ
	// failure; the restore path refuses it with an actionable error. Omitted from
	// JSON when empty so a sidecar from before this field decodes to "" and the
	// check is a no-op for snapshots with no share.
	ShareTag string `json:"share_tag,omitempty"`

	// Disk identity stamp. A full hash of a multi-GB image would be far too
	// slow; the disk only changes while the VM runs, so size+mtime+inode is an
	// instant, reliable "has it changed since the (paused) save?" check.
	DiskSizeBytes     int64  `json:"disk_size_bytes"`
	DiskMtimeUnixNano int64  `json:"disk_mtime_unix_nano"`
	DiskInode         uint64 `json:"disk_inode"`
}

// SaveMetadataPath returns the sidecar path for a saved-state file.
func SaveMetadataPath(savePath string) string { return savePath + ".json" }

// writeSaveMetadata captures the hardware config and current disk stamp and
// writes the sidecar next to savePath. Call it while the guest is paused, so
// the disk is frozen and the stamp is consistent with the saved RAM.
func writeSaveMetadata(savePath string, cpus uint, memGiB uint64, diskGiB int, gui bool, diskPath, shareTag string) error {
	m, err := diskStamp(diskPath)
	if err != nil {
		return err
	}
	m.CPUs = cpus
	m.MemoryGiB = memGiB
	m.DiskSizeGiB = diskGiB
	m.GUI = &gui
	m.ShareTag = shareTag
	m.DiskPath = diskPath

	b, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		return err
	}
	// Through internal/util, the owner of atomic file writes: a reader of the
	// sidecar sees either the whole previous generation's file or the whole new
	// one, never the half-written file a plain os.WriteFile can leave when the
	// host dies or the disk fills mid-write. A half-written sidecar parses as
	// nothing and would take a restore down the "no usable metadata" path.
	return util.WriteFileAtomic(SaveMetadataPath(savePath), b, saveGenerationMode)
}

// publishSaveGeneration writes one saved-state generation — the VZ
// machine-state file and the sidecar that describes it — as a single unit.
// writeState writes the state file at statePath; writeSidecar writes the
// sidecar beside it.
//
// A state file paired with a sidecar that does not describe it is worse than no
// snapshot at all: the sidecar carries the disk-identity stamp that stops a
// restore from pushing saved RAM into a disk image that has moved on since. So
// the previous generation is removed FIRST — a new state can never inherit the
// previous save's sidecar — and a failure at any step removes both files again,
// leaving nothing that looks restorable and nothing to roll forward from.
//
// The state is written BEFORE the sidecar, not after. The stamp in the sidecar
// must describe the disk as it stands once the RAM freeze is complete; stamping
// first would assume nothing touches the image while VZ writes the state, which
// is a claim about another component that no test here can hold (AGENTS.md
// section 5.7), and a stamp taken too early makes every later restore refuse.
// The window this ordering opens — a state file whose sidecar was never written
// because the host died between the two — is closed at the other end instead: a
// restore refuses a state file that has no sidecar rather than skipping the
// disk check (see prepareRestore).
func publishSaveGeneration(statePath string, writeState, writeSidecar func() error) error {
	if err := removeSaveGeneration(statePath); err != nil {
		return err
	}
	if err := writeState(); err != nil {
		// VZ may have left a partial state file; it must not survive as an
		// apparently restorable snapshot.
		_ = removeSaveGeneration(statePath)
		return err
	}
	if err := writeSidecar(); err != nil {
		_ = removeSaveGeneration(statePath)
		return fmt.Errorf("write saved-state metadata: %w", err)
	}
	return nil
}

// removeSaveGeneration removes a saved-state file and its sidecar together. A
// file that is not there is not an error: the point is that neither half of the
// generation remains, not that both were present.
func removeSaveGeneration(statePath string) error {
	for _, p := range []string{statePath, SaveMetadataPath(statePath)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove saved state %s: %w", p, err)
		}
	}
	return nil
}

// diskStamp returns a SaveMetadata populated with the disk's identity fields
// (size, mtime, inode) only — a stat-only, instant fingerprint.
func diskStamp(diskPath string) (SaveMetadata, error) {
	fi, err := os.Stat(diskPath)
	if err != nil {
		return SaveMetadata{}, fmt.Errorf("stat disk %s: %w", diskPath, err)
	}
	m := SaveMetadata{
		DiskSizeBytes:     fi.Size(),
		DiskMtimeUnixNano: fi.ModTime().UnixNano(),
	}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		m.DiskInode = sys.Ino
	}
	return m, nil
}

// LoadSaveMetadata reads the sidecar next to savePath. The returned error wraps
// os.ErrNotExist when there is no sidecar, which callers must treat as "this
// saved state cannot be verified" rather than as "no checks needed": the
// sidecar and the state file are published and moved as one generation, so a
// state file without one is a generation that came apart.
func LoadSaveMetadata(savePath string) (*SaveMetadata, error) {
	b, err := os.ReadFile(SaveMetadataPath(savePath))
	if err != nil {
		return nil, err
	}
	var m SaveMetadata
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse saved-state metadata: %w", err)
	}
	return &m, nil
}

// VerifyDisk reports an error when the disk image no longer matches the stamp
// recorded at save time — i.e. it changed since the snapshot, so restoring the
// snapshot's RAM would be inconsistent with the on-disk filesystem.
func (m *SaveMetadata) VerifyDisk() error {
	cur, err := diskStamp(m.DiskPath)
	if err != nil {
		return err
	}
	if cur.DiskSizeBytes != m.DiskSizeBytes ||
		cur.DiskMtimeUnixNano != m.DiskMtimeUnixNano ||
		(m.DiskInode != 0 && cur.DiskInode != m.DiskInode) {
		return fmt.Errorf("disk %s changed since the snapshot was taken (size/mtime/inode mismatch); restoring would corrupt the guest", m.DiskPath)
	}
	return nil
}
