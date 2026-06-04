package vm

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"
)

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
func writeSaveMetadata(savePath string, cpus uint, memGiB uint64, diskGiB int, gui bool, diskPath string) error {
	m, err := diskStamp(diskPath)
	if err != nil {
		return err
	}
	m.CPUs = cpus
	m.MemoryGiB = memGiB
	m.DiskSizeGiB = diskGiB
	m.GUI = &gui
	m.DiskPath = diskPath

	b, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(SaveMetadataPath(savePath), b, 0o600)
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
// os.ErrNotExist when there is no sidecar (a save from before this feature).
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

// guiModeLabel renders a boot mode for operator-facing messages.
func guiModeLabel(gui bool) string {
	if gui {
		return "gui"
	}
	return "headless"
}
