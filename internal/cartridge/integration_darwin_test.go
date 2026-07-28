//go:build darwin

package cartridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCartridgeRoundTrip_Integration drives the real hdiutil through a full
// create -> attach -> write -> detach -> reattach -> read -> compact ->
// convert(DMG) -> convert(sparse) cycle, asserting persistence and cleaning up
// every artifact. Gated behind BLADERUNNER_CARTRIDGE_IT=1 so it never runs in
// the default suite or on machines without hdiutil.
func TestCartridgeRoundTrip_Integration(t *testing.T) {
	if os.Getenv("BLADERUNNER_CARTRIDGE_IT") != "1" {
		t.Skip("set BLADERUNNER_CARTRIDGE_IT=1 to run the hdiutil integration test")
	}
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("hdiutil not found in PATH")
	}

	dir := t.TempDir()
	stem := filepath.Join(dir, "it")
	mp := filepath.Join(dir, "mnt")

	// Create a tiny sparse cartridge (MinSizeGiB is small and sparse-backed).
	imgPath, err := Create(stem, MinSizeGiB)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(imgPath); err != nil {
		t.Fatalf("created image missing at %q: %v", imgPath, err)
	}
	t.Cleanup(func() { _ = os.Remove(imgPath) })

	// Attach and write a marker file.
	m, err := Attach(imgPath, mp)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !IsAttached(m.Mountpoint) {
		t.Fatalf("IsAttached(%q) = false after Attach", m.Mountpoint)
	}
	const want = "hello-cartridge"
	marker := filepath.Join(m.Mountpoint, "marker.txt")
	if err := os.WriteFile(marker, []byte(want), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// Detach, then confirm the mountpoint is no longer a mounted volume.
	if err := Detach(m.Mountpoint); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if IsAttached(m.Mountpoint) {
		t.Fatalf("IsAttached true after Detach")
	}

	// Re-attach and assert the marker survived (persistence round-trip).
	m2, err := Attach(imgPath, mp)
	if err != nil {
		t.Fatalf("re-Attach: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(m2.Mountpoint, "marker.txt"))
	if err != nil {
		t.Fatalf("read marker after reattach: %v", err)
	}
	if string(got) != want {
		t.Fatalf("marker = %q, want %q", string(got), want)
	}
	if err := Detach(m2.Mountpoint); err != nil {
		t.Fatalf("final Detach: %v", err)
	}

	// Compact the detached image (must succeed without error).
	if err := Compact(imgPath); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Convert to a shippable DMG, then back to a runnable sparse working copy.
	dmgPath, err := ConvertToDMG(imgPath, filepath.Join(dir, "ship"))
	if err != nil {
		t.Fatalf("ConvertToDMG: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(dmgPath) })
	if _, err := os.Stat(dmgPath); err != nil {
		t.Fatalf("dmg missing at %q: %v", dmgPath, err)
	}

	sparsePath, err := ConvertToSparse(dmgPath, filepath.Join(dir, "work"))
	if err != nil {
		t.Fatalf("ConvertToSparse: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(sparsePath) })

	// The recovered sparse copy must still hold the marker.
	mp3 := filepath.Join(dir, "mnt3")
	m3, err := Attach(sparsePath, mp3)
	if err != nil {
		t.Fatalf("Attach recovered sparse: %v", err)
	}
	got3, err := os.ReadFile(filepath.Join(m3.Mountpoint, "marker.txt"))
	if err != nil {
		t.Fatalf("read marker from recovered sparse: %v", err)
	}
	if string(got3) != want {
		t.Fatalf("recovered marker = %q, want %q", string(got3), want)
	}
	if err := Detach(m3.Mountpoint); err != nil {
		t.Fatalf("Detach recovered sparse: %v", err)
	}
}

// TestWriteBackRoundTrip_Integration is the data-safety proof against the real
// hdiutil: pack a cartridge, ship it as a .dmg, boot-open it with Persist,
// change the volume, close it, and assert the shipped file now carries the
// change — while a write-back that CANNOT succeed leaves that same file
// byte-for-byte identical.
//
// Gated behind BLADERUNNER_CARTRIDGE_IT=1 like the round-trip above. No VM is
// started: Open/Close is the whole cartridge half of a boot, and what a running
// guest adds is writes to root.img, which a marker file stands in for.
func TestWriteBackRoundTrip_Integration(t *testing.T) {
	if os.Getenv("BLADERUNNER_CARTRIDGE_IT") != "1" {
		t.Skip("set BLADERUNNER_CARTRIDGE_IT=1 to run the hdiutil integration test")
	}
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("hdiutil not found in PATH")
	}

	dir := t.TempDir()
	dmg := packShippedCartridge(t, dir)
	shipped := fileSHA256(t, dmg)
	t.Logf("shipped cartridge sha256 = %s", shipped)

	// A write-back that cannot finish must not touch the original. The
	// cartridge's own directory is made read-only, so the staging file has
	// nowhere to go — the case a full disk reaches by another route.
	refuseWriteBackReadOnly(t, dir, dmg, shipped)

	// Now the real thing: open with Persist, write into the volume, close.
	mp := filepath.Join(dir, "mnt")
	o, err := open(context.Background(), defaultRunner, dmg, OpenOptions{
		Mountpoint: mp, Policy: MountPrivate, Persist: true,
	})
	if err != nil {
		t.Fatalf("Open with Persist: %v", err)
	}
	const guestWrote = "what-the-guest-wrote"
	if err := os.WriteFile(filepath.Join(o.Mountpoint(), "guest.txt"), []byte(guestWrote), 0o644); err != nil {
		t.Fatalf("write into the cartridge: %v", err)
	}
	if !o.WritesBack() {
		t.Fatal("WritesBack() is false for a .dmg opened with Persist")
	}
	if err := o.Close(); err != nil {
		t.Fatalf("Close (write-back): %v", err)
	}

	committed := fileSHA256(t, dmg)
	t.Logf("committed cartridge sha256 = %s", committed)
	if committed == shipped {
		t.Fatalf("the shipped .dmg was not rewritten: sha256 still %s", committed)
	}
	if _, err := os.Stat(TrimExt(dmg) + SparseExt); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a committed close left the working copy behind: %v", err)
	}

	// The committed file must still be a cartridge, and must carry the change.
	mp2 := filepath.Join(dir, "mnt2")
	back, err := open(context.Background(), defaultRunner, dmg, OpenOptions{Mountpoint: mp2, Policy: MountPrivate})
	if err != nil {
		t.Fatalf("re-open the committed cartridge: %v", err)
	}
	defer func() { _ = back.Close() }()
	if back.Manifest == nil || back.Manifest.Name == "" {
		t.Fatalf("committed cartridge has no manifest: %+v", back.Manifest)
	}
	if back.Metadata.FormatVersion != FormatVersion {
		t.Errorf("committed cartridge format = %d, want %d", back.Metadata.FormatVersion, FormatVersion)
	}
	got, err := os.ReadFile(filepath.Join(back.Mountpoint(), "guest.txt"))
	if err != nil || string(got) != guestWrote {
		t.Fatalf("committed cartridge holds %q, %v; want the guest's change", got, err)
	}
}

// packShippedCartridge builds a real cartridge with hdiutil and returns the
// shipped .dmg. The build image is left behind in dir; t.TempDir removes it.
func packShippedCartridge(t *testing.T, dir string) string {
	t.Helper()
	img, err := Create(filepath.Join(dir, "build"), MinSizeGiB)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mp := filepath.Join(dir, "build-mnt")
	m, err := Attach(img, mp)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := Pack(m.Mountpoint, validSourceManifest(), PackOptions{Name: "persist", PackedBy: "br-test"}); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if err := os.WriteFile(filepath.Join(m.Mountpoint, RootImageFile), []byte("not-really-a-disk"), 0o644); err != nil {
		t.Fatalf("write root.img: %v", err)
	}
	if err := Detach(m.Mountpoint); err != nil {
		t.Fatalf("Detach after pack: %v", err)
	}
	dmg, err := ConvertToDMG(img, filepath.Join(dir, "persist"))
	if err != nil {
		t.Fatalf("ConvertToDMG: %v", err)
	}
	return dmg
}

// refuseWriteBackReadOnly opens the cartridge with Persist somewhere the new
// artifact cannot be published, closes it, and asserts the shipped file is
// unchanged and the guest's changes were rescued rather than dropped.
func refuseWriteBackReadOnly(t *testing.T, dir, dmg, shipped string) {
	t.Helper()
	mp := filepath.Join(dir, "mnt-ro")
	o, err := open(context.Background(), defaultRunner, dmg, OpenOptions{
		Mountpoint: mp, Policy: MountPrivate, Persist: true,
	})
	if err != nil {
		t.Fatalf("Open for the refusal case: %v", err)
	}
	if err := os.WriteFile(filepath.Join(o.Mountpoint(), "doomed.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write into the cartridge: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	closeErr := o.Close()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("restore dir mode: %v", err)
	}
	if !errors.Is(closeErr, ErrWriteBackReadOnly) {
		t.Fatalf("Close = %v, want ErrWriteBackReadOnly", closeErr)
	}
	after := fileSHA256(t, dmg)
	t.Logf("after a FAILED write-back sha256 = %s (was %s)", after, shipped)
	if after != shipped {
		t.Fatalf("a refused write-back changed the shipped cartridge: %s -> %s", shipped, after)
	}
	// The working copy could not be renamed either (the directory was still
	// read-only at that point), so it is still there; clear it so the real
	// write-back below starts from a clean directory.
	if err := os.Remove(TrimExt(dmg) + SparseExt); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clear the preserved working copy: %v", err)
	}
	for _, rescued := range rescuedFiles(t, dir) {
		if err := os.Remove(rescued); err != nil {
			t.Fatalf("clear rescue image: %v", err)
		}
	}
}

// fileSHA256 fingerprints a file, so "the original is unchanged" is asserted on
// its bytes rather than on its mtime or its size.
func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
