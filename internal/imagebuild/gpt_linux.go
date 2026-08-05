//go:build linux

package imagebuild

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
)

// GPT on-disk layout constants. Sector size is fixed at 512 because that is what
// the Debian cloud images this pipeline consumes use; a 4Kn image would need the
// logical sector size read from the device rather than assumed, so it is
// rejected rather than silently misparsed.
const (
	sectorSize = 512
	// headerLBA is the logical block holding the primary GPT header.
	headerLBA = 1

	headerSizeOffset       = 12
	headerCRCOffset        = 16
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
	// maxEntrySize bounds the other end. The spec fixes no ceiling, and the
	// field is a uint32 read straight off the disk, so without one a header
	// carrying 0xFFFFFFFF asks for a 4 GiB allocation before anything about it
	// has been checked. Real tables use 128.
	maxEntrySize = 4096
	// entrySizeAlignment is the multiple the spec requires entry size to be.
	entrySizeAlignment = 8

	// minHeaderSize is the smallest GPT header the spec defines, and the least
	// that can hold every field read here.
	minHeaderSize = 92

	// maxEntryArrayLBA bounds where the entry array may start. A uint64 LBA
	// multiplied by the sector size overflows int64 long before this, and no
	// real table puts its entries 2^40 blocks in.
	maxEntryArrayLBA = 1 << 40
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
	// Check the header's own CRC BEFORE trusting any field in it. Everything
	// below — where the entry array lives, how many entries, how large each one
	// is — is read from this header and used to size allocations and compute
	// file offsets. Validating first means the bounds checks that follow are a
	// second line rather than the only one.
	if err := verifyHeaderCRC(header); err != nil {
		return partition{}, err
	}

	entryLBA := binary.LittleEndian.Uint64(header[headerEntryLBAOffset:])
	count := binary.LittleEndian.Uint32(header[headerEntryCountOffset:])
	size := binary.LittleEndian.Uint32(header[headerEntrySizeOffset:])

	if size < minEntrySize || size > maxEntrySize || size%entrySizeAlignment != 0 {
		return partition{}, fmt.Errorf("%w: entry size %d is not a multiple of %d between %d and %d",
			errNoGPT, size, entrySizeAlignment, minEntrySize, maxEntrySize)
	}
	if entryLBA == 0 || entryLBA > maxEntryArrayLBA {
		return partition{}, fmt.Errorf("%w: entry array starts at implausible LBA %d", errNoGPT, entryLBA)
	}
	if count > maxPartitionEntries {
		count = maxPartitionEntries
	}

	var best partition
	var found bool
	entry := make([]byte, size)
	arrayStart := entryLBA * sectorSize
	for i := range count {
		// Computed in uint64 and range-checked before narrowing. The operands
		// come off the disk, and a signed multiply that wraps produces a
		// negative offset — which ReadAt happens to reject today, making this a
		// latent bug rather than a live one. Not a distinction worth relying on.
		offset := arrayStart + uint64(i)*uint64(size)
		if offset > math.MaxInt64 {
			break
		}
		if _, err := r.ReadAt(entry, int64(offset)); err != nil {
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

// verifyHeaderCRC checks the GPT header against the CRC32 stored inside it.
//
// The CRC covers HeaderSize bytes with its own four bytes taken as zero, which
// is why this works on a copy. A header that fails here is corrupt or forged,
// and every field the caller goes on to use comes out of it — so this is the
// check that makes the rest of the parse meaningful rather than hopeful.
func verifyHeaderCRC(header []byte) error {
	headerSize := binary.LittleEndian.Uint32(header[headerSizeOffset:])
	if headerSize < minHeaderSize || headerSize > uint32(len(header)) {
		return fmt.Errorf("%w: header size %d is outside %d..%d",
			errNoGPT, headerSize, minHeaderSize, len(header))
	}

	want := binary.LittleEndian.Uint32(header[headerCRCOffset:])

	scratch := make([]byte, headerSize)
	copy(scratch, header[:headerSize])
	// The stored CRC is excluded from its own calculation.
	binary.LittleEndian.PutUint32(scratch[headerCRCOffset:], 0)

	if got := crc32.ChecksumIEEE(scratch); got != want {
		return fmt.Errorf("%w: header CRC is %#08x but the header claims %#08x", errNoGPT, got, want)
	}
	return nil
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
