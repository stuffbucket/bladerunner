//go:build linux

package imagebuild

import (
	"bytes"
	"encoding/binary"
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
	binary.LittleEndian.PutUint64(header[headerEntryLBAOffset:], entriesLBA)
	binary.LittleEndian.PutUint32(header[headerEntryCountOffset:], entryCount)
	binary.LittleEndian.PutUint32(header[headerEntrySizeOffset:], entrySize)

	for i, p := range parts {
		e := disk[sectorSize*entriesLBA+i*entrySize:]
		// A non-zero type GUID marks the entry as used.
		e[0] = 0x01
		binary.LittleEndian.PutUint64(e[entryStartLBAOffset:], p.StartLBA)
		binary.LittleEndian.PutUint64(e[entryEndLBAOffset:], p.EndLBA)
	}
	return disk
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
