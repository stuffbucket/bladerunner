//go:build linux

package imagebuild

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// GPT on-disk layout constants. Sector size is fixed at 512 because that is what
// the Debian cloud images this pipeline consumes use; a 4Kn image would need the
// logical sector size read from the device rather than assumed, so it is
// rejected rather than silently misparsed.
const (
	sectorSize = 512
	// headerLBA is the logical block holding the primary GPT header.
	headerLBA = 1

	headerEntryLBAOffset   = 72
	headerEntryCountOffset = 80
	headerEntrySizeOffset  = 84

	entryStartLBAOffset = 32
	entryEndLBAOffset   = 40

	// maxPartitionEntries bounds how many entries are read, so a corrupt header
	// cannot make the build allocate unboundedly.
	maxPartitionEntries = 512
	// minEntrySize is the smallest GPT entry the spec allows.
	minEntrySize = 128
)

// gptSignature marks a GPT header.
var gptSignature = []byte("EFI PART")

// errNoGPT reports an image without a readable GPT.
var errNoGPT = errors.New("no GPT partition table found")

// partition is one GPT entry, in logical blocks.
type partition struct {
	// StartLBA is the first logical block of the partition.
	StartLBA uint64
	// EndLBA is the last logical block, inclusive.
	EndLBA uint64
}

// sectors returns the partition length in logical blocks.
func (p partition) sectors() uint64 {
	if p.EndLBA < p.StartLBA {
		return 0
	}
	return p.EndLBA - p.StartLBA + 1
}

// byteOffset returns the partition's start as a byte offset, which is what
// mounting by offset needs.
func (p partition) byteOffset() uint64 {
	return p.StartLBA * sectorSize
}

// findRootPartition returns the guest's root partition.
//
// It selects the LARGEST partition rather than a fixed index. Debian's cloud
// images carry a small EFI system partition alongside the ext4 root, their entry
// order does not match their on-disk order, and the root is not reliably entry
// one — so neither "first entry" nor "lowest start sector" is safe. Size is the
// one property that distinguishes a multi-gigabyte root from a 127M ESP.
func findRootPartition(r io.ReaderAt) (partition, error) {
	header := make([]byte, sectorSize)
	if _, err := r.ReadAt(header, headerLBA*sectorSize); err != nil {
		return partition{}, fmt.Errorf("read GPT header: %w", err)
	}
	if !hasPrefix(header, gptSignature) {
		return partition{}, errNoGPT
	}

	entryLBA := binary.LittleEndian.Uint64(header[headerEntryLBAOffset:])
	count := binary.LittleEndian.Uint32(header[headerEntryCountOffset:])
	size := binary.LittleEndian.Uint32(header[headerEntrySizeOffset:])

	if size < minEntrySize {
		return partition{}, fmt.Errorf("%w: entry size %d is below the %d-byte minimum", errNoGPT, size, minEntrySize)
	}
	if count > maxPartitionEntries {
		count = maxPartitionEntries
	}

	var best partition
	var found bool
	entry := make([]byte, size)
	for i := range count {
		offset := int64(entryLBA)*sectorSize + int64(i)*int64(size)
		if _, err := r.ReadAt(entry, offset); err != nil {
			break // A short table is not fatal; use whatever was readable.
		}
		if isZero(entry[:minEntrySize]) {
			continue // Unused entry.
		}
		p := partition{
			StartLBA: binary.LittleEndian.Uint64(entry[entryStartLBAOffset:]),
			EndLBA:   binary.LittleEndian.Uint64(entry[entryEndLBAOffset:]),
		}
		// Guard on `found` rather than comparing against a zero partition:
		// partition{0,0} spans one sector by the inclusive-end arithmetic, so a
		// zero value is not distinguishable from a real entry by size alone.
		if !found || p.sectors() > best.sectors() {
			best = p
			found = true
		}
	}

	if !found {
		return partition{}, fmt.Errorf("%w: the table defines no usable partition", errNoGPT)
	}
	if best.StartLBA == 0 || best.sectors() == 0 {
		return partition{}, fmt.Errorf("%w: the largest partition is empty or starts at LBA 0", errNoGPT)
	}
	return best, nil
}

// hasPrefix reports whether b starts with prefix.
func hasPrefix(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i, c := range prefix {
		if b[i] != c {
			return false
		}
	}
	return true
}

// isZero reports whether every byte is zero, which marks an unused GPT entry.
func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
