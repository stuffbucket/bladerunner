//go:build darwin

package vm

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// Bounds for the concurrent-reader hammer below.
const (
	// metadataPadBytes makes the payload big enough that a truncating write has
	// a window a concurrent reader can land in.
	metadataPadBytes = 1 << 18
	// metadataRewrites is how many times the writer republishes the metadata.
	metadataRewrites = 200
	// metadataReaders is how many goroutines read the metadata concurrently.
	metadataReaders = 4
)

// metadataConfig returns a config whose MetadataPath is inside a fresh temp dir.
func metadataConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{MetadataPath: filepath.Join(t.TempDir(), "runtime-metadata.json")}
}

// A reader must never see runtime-metadata.json missing, empty or short while
// it is being rewritten. os.WriteFile opens O_TRUNC, so the destination is
// briefly zero-length; a reader that lands there loses the VM's MAC address and
// the next boot invents a new one.
func TestSaveMetadataNeverExposesAPartialFile(t *testing.T) {
	cfg := metadataConfig(t)
	md := &runtimeMetadata{MACAddress: strings.Repeat("a", metadataPadBytes)}
	if err := saveMetadata(cfg, md); err != nil {
		t.Fatalf("seed saveMetadata: %v", err)
	}
	full, err := os.ReadFile(cfg.MetadataPath)
	if err != nil {
		t.Fatalf("read seeded metadata: %v", err)
	}
	wantLen := len(full)

	var partials atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range metadataReaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b, readErr := os.ReadFile(cfg.MetadataPath)
				if readErr != nil || len(b) != wantLen {
					partials.Add(1)
				}
			}
		}()
	}
	for range metadataRewrites {
		if err := saveMetadata(cfg, md); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("saveMetadata: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	if n := partials.Load(); n != 0 {
		t.Errorf("a concurrent reader saw partial runtime metadata %d times (want %d bytes every time): the publish is not atomic", n, wantLen)
	}
}

// The metadata keeps its published mode across a rewrite. os.CreateTemp makes
// 0600 files, so an atomic publish that forgets to chmod would narrow it.
func TestSaveMetadataKeepsFileMode(t *testing.T) {
	cfg := metadataConfig(t)
	md := &runtimeMetadata{MACAddress: "aa:bb:cc:dd:ee:ff"}
	for i := range 2 {
		if err := saveMetadata(cfg, md); err != nil {
			t.Fatalf("saveMetadata #%d: %v", i, err)
		}
		st, err := os.Stat(cfg.MetadataPath)
		if err != nil {
			t.Fatalf("stat metadata #%d: %v", i, err)
		}
		if perm := st.Mode().Perm(); perm != metadataFilePerm {
			t.Errorf("metadata mode after write #%d = %o, want %o", i, perm, metadataFilePerm)
		}
	}
}

// A completed publish must leave no staging file in the VM's state directory,
// and the round trip must survive it.
func TestSaveMetadataLeavesNoTempFileAndRoundTrips(t *testing.T) {
	cfg := metadataConfig(t)
	want := &runtimeMetadata{MACAddress: "aa:bb:cc:dd:ee:ff"}
	if err := saveMetadata(cfg, want); err != nil {
		t.Fatalf("saveMetadata: %v", err)
	}
	des, err := os.ReadDir(filepath.Dir(cfg.MetadataPath))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(des) != 1 {
		t.Errorf("state directory has %d entries, want only the metadata file", len(des))
	}
	got, err := loadOrCreateMetadata(cfg)
	if err != nil {
		t.Fatalf("loadOrCreateMetadata: %v", err)
	}
	if got.MACAddress != want.MACAddress {
		t.Errorf("MACAddress = %q, want %q", got.MACAddress, want.MACAddress)
	}
}
