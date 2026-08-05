//go:build linux

package imagebuild

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"math"
	"testing"
)

// syntheticGPT builds an in-memory disk with a GPT describing the given
// partitions, so partition discovery can be tested without root or a real image.
func syntheticGPT(t *testing.T, parts []partition) []byte {
	t.Helper()

	const (
		entriesLBA = 2
		entrySize  = 128
		entryCount = 128
	)
	disk := make([]byte, sectorSize*(entriesLBA+(entrySize*entryCount)/sectorSize+1))

	header := disk[sectorSize : sectorSize*2]
	copy(header, gptSignature)
	binary.LittleEndian.PutUint32(header[headerSizeOffset:], minHeaderSize)
	binary.LittleEndian.PutUint64(header[headerEntryLBAOffset:], entriesLBA)
	binary.LittleEndian.PutUint32(header[headerEntryCountOffset:], entryCount)
	binary.LittleEndian.PutUint32(header[headerEntrySizeOffset:], entrySize)
	sealGPTHeader(header)

	for i, p := range parts {
		e := disk[sectorSize*entriesLBA+i*entrySize:]
		// A non-zero type GUID marks the entry as used.
		e[0] = 0x01
		binary.LittleEndian.PutUint64(e[entryStartLBAOffset:], p.StartLBA)
		binary.LittleEndian.PutUint64(e[entryEndLBAOffset:], p.EndLBA)
	}
	return disk
}

// sealGPTHeader writes the CRC a real GPT header carries, over HeaderSize bytes
// with the CRC field itself zeroed. Tests that mutate a header must re-seal it,
// or they assert the CRC check rather than what they meant to.
func sealGPTHeader(header []byte) {
	size := binary.LittleEndian.Uint32(header[headerSizeOffset:])
	binary.LittleEndian.PutUint32(header[headerCRCOffset:], 0)
	binary.LittleEndian.PutUint32(header[headerCRCOffset:], crc32.ChecksumIEEE(header[:size]))
}

// The layout here is the real one from debian-13-genericcloud-arm64: a 2.9G root
// at sector 262144 and a 127M EFI system partition at 2048. Note the ESP comes
// FIRST on disk while the root is partition entry 1 — picking entry 1, or the
// lowest start sector, would both be wrong on some layout, so selection is by
// size.
func TestFindRootPartitionPicksTheLargest(t *testing.T) {
	disk := syntheticGPT(t, []partition{
		{StartLBA: 262144, EndLBA: 6289407},
		{StartLBA: 2048, EndLBA: 262143},
	})

	got, err := findRootPartition(bytes.NewReader(disk))
	if err != nil {
		t.Fatalf("findRootPartition() error = %v", err)
	}
	if got.StartLBA != 262144 {
		t.Errorf("StartLBA = %d, want 262144", got.StartLBA)
	}
	if want := uint64(262144 * sectorSize); got.byteOffset() != want {
		t.Errorf("byteOffset() = %d, want %d", got.byteOffset(), want)
	}
}

// The ESP ordering must not matter.
func TestFindRootPartitionIgnoresEntryOrder(t *testing.T) {
	disk := syntheticGPT(t, []partition{
		{StartLBA: 2048, EndLBA: 262143},
		{StartLBA: 262144, EndLBA: 6289407},
	})

	got, err := findRootPartition(bytes.NewReader(disk))
	if err != nil {
		t.Fatalf("findRootPartition() error = %v", err)
	}
	if got.StartLBA != 262144 {
		t.Errorf("StartLBA = %d, want the larger partition at 262144", got.StartLBA)
	}
}

func TestFindRootPartitionRejectsANonGPTImage(t *testing.T) {
	disk := make([]byte, sectorSize*4) // all zeroes: no signature
	if _, err := findRootPartition(bytes.NewReader(disk)); err == nil {
		t.Fatal("findRootPartition() error = nil, want an error for a missing GPT signature")
	}
}

func TestFindRootPartitionRejectsAnEmptyTable(t *testing.T) {
	disk := syntheticGPT(t, nil)
	if _, err := findRootPartition(bytes.NewReader(disk)); err == nil {
		t.Fatal("findRootPartition() error = nil, want an error when no partitions are defined")
	}
}

// A crafted entry size must not become an allocation.
//
// SizeOfPartitionEntry is a uint32 read straight off the disk and used as the
// length of a make([]byte, ...). Only a lower bound was checked, so a header
// carrying 0xFFFFFFFF asked for 4 GiB before anything about the table had been
// established. Reported as point 5 of #239.
func TestFindRootPartitionRejectsAHugeEntrySize(t *testing.T) {
	for _, tc := range []struct {
		name string
		size uint32
	}{
		{"four gigabytes", math.MaxUint32},
		{"just over the cap", maxEntrySize + entrySizeAlignment},
		{"not a multiple of the alignment", minEntrySize + 1},
		{"zero", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disk := syntheticGPT(t, []partition{{StartLBA: 2048, EndLBA: 4096}})
			header := disk[sectorSize : sectorSize*2]
			binary.LittleEndian.PutUint32(header[headerEntrySizeOffset:], tc.size)
			sealGPTHeader(header)

			if _, err := findRootPartition(bytes.NewReader(disk)); err == nil {
				t.Fatalf("accepted an entry size of %d", tc.size)
			}
		})
	}
}

// A crafted entry-array LBA must not overflow the offset arithmetic.
//
// The offset was computed as int64(entryLBA)*sectorSize. A uint64 near the top
// of its range wraps to a negative int64 there. ReadAt rejects a negative
// offset, so this was latent rather than live — which is not a property worth
// depending on.
func TestFindRootPartitionRejectsAnOverflowingEntryLBA(t *testing.T) {
	for _, tc := range []struct {
		name string
		lba  uint64
	}{
		{"wraps int64 when scaled", math.MaxUint64 / 256},
		{"maximum uint64", math.MaxUint64},
		{"beyond any real disk", maxEntryArrayLBA + 1},
		{"zero", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disk := syntheticGPT(t, []partition{{StartLBA: 2048, EndLBA: 4096}})
			header := disk[sectorSize : sectorSize*2]
			binary.LittleEndian.PutUint64(header[headerEntryLBAOffset:], tc.lba)
			sealGPTHeader(header)

			if _, err := findRootPartition(bytes.NewReader(disk)); err == nil {
				t.Fatalf("accepted an entry array at LBA %d", tc.lba)
			}
		})
	}
}

// A header whose CRC does not match its contents must be refused.
//
// Every field the parse depends on lives in this header, so a table that fails
// its own integrity check cannot be treated as authoritative. Nothing checked
// it at all before.
func TestFindRootPartitionRejectsABadHeaderCRC(t *testing.T) {
	disk := syntheticGPT(t, []partition{{StartLBA: 2048, EndLBA: 999999}})
	header := disk[sectorSize : sectorSize*2]

	// Move the entry array without re-sealing: exactly what a tampered table
	// looks like, and what a corrupt one looks like too.
	binary.LittleEndian.PutUint64(header[headerEntryLBAOffset:], 3)

	if _, err := findRootPartition(bytes.NewReader(disk)); err == nil {
		t.Fatal("accepted a header whose CRC does not match its contents")
	}
}

// A header claiming a size it cannot have must be refused before the CRC is
// computed over it, or the length itself decides how much is read.
func TestFindRootPartitionRejectsAnImplausibleHeaderSize(t *testing.T) {
	for _, size := range []uint32{0, minHeaderSize - 1, sectorSize + 1, math.MaxUint32} {
		disk := syntheticGPT(t, []partition{{StartLBA: 2048, EndLBA: 4096}})
		header := disk[sectorSize : sectorSize*2]
		binary.LittleEndian.PutUint32(header[headerSizeOffset:], size)

		if _, err := findRootPartition(bytes.NewReader(disk)); err == nil {
			t.Errorf("accepted a header claiming size %d", size)
		}
	}
}

// The valid case must still pass, or every test above is satisfied by a parser
// that rejects everything.
func TestFindRootPartitionStillAcceptsAValidTable(t *testing.T) {
	disk := syntheticGPT(t, []partition{
		{StartLBA: 2048, EndLBA: 264191},
		{StartLBA: 262144, EndLBA: 6291456},
	})

	got, err := findRootPartition(bytes.NewReader(disk))
	if err != nil {
		t.Fatalf("rejected a well-formed table: %v", err)
	}
	if got.StartLBA != 262144 {
		t.Errorf("StartLBA = %d, want 262144", got.StartLBA)
	}
}
