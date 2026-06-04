//go:build darwin

package cartridge

import (
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
	imgPath, err := Create(stem, "it", MinSizeGiB)
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
