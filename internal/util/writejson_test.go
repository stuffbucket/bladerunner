package util

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type jsonDoc struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// TestWriteJSONAtomicIndentsTwoSpaces pins the on-disk shape. Every caller of
// this helper writes a file a person is expected to open with `cat`, and each
// one previously spelled the indent out for itself; the whole point of the
// helper is that they cannot drift.
func TestWriteJSONAtomicIndentsTwoSpaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	if err := WriteJSONAtomic(path, jsonDoc{Name: "demo", Count: 3}, 0o644); err != nil {
		t.Fatalf("WriteJSONAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "{\n  \"name\": \"demo\",\n  \"count\": 3\n}"
	if string(got) != want {
		t.Errorf("on-disk JSON =\n%q\nwant\n%q", got, want)
	}
}

// TestWriteJSONAtomicHonoursPerm checks the mode reaches the destination rather
// than the temp file's default, on a rewrite as well as a create — a rewrite is
// where a temp-and-rename implementation regresses to os.CreateTemp's 0600.
func TestWriteJSONAtomicHonoursPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	for i, perm := range []os.FileMode{0o644, 0o600} {
		if err := WriteJSONAtomic(path, jsonDoc{Count: i}, perm); err != nil {
			t.Fatalf("write #%d: %v", i, err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat #%d: %v", i, err)
		}
		if st.Mode().Perm() != perm {
			t.Errorf("mode after write #%d = %o, want %o", i, st.Mode().Perm(), perm)
		}
	}
}

// TestWriteJSONAtomicIsAtomic is the reason this helper exists rather than each
// caller doing MarshalIndent + os.WriteFile: a reader must never see a
// truncated file. This fails against a plain os.WriteFile, which opens O_TRUNC.
func TestWriteJSONAtomicIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	big := jsonDoc{Name: strings.Repeat("x", 200_000)}
	small := jsonDoc{Name: strings.Repeat("y", 200_000)}

	if err := WriteJSONAtomic(path, big, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("seed read: %v", err)
	}

	done := make(chan struct{})
	bad := make(chan int, 1)
	go func() {
		defer close(done)
		for i := 0; i < 400; i++ {
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				continue
			}
			// Every observation must be a complete document: either the
			// previous one or the new one, never a prefix.
			if len(b) != len(full) {
				select {
				case bad <- len(b):
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 100; i++ {
		v := big
		if i%2 == 1 {
			v = small
		}
		if err := WriteJSONAtomic(path, v, 0o644); err != nil {
			t.Fatalf("rewrite %d: %v", i, err)
		}
	}
	<-done
	select {
	case n := <-bad:
		t.Errorf("a reader saw a %d-byte file, want a complete %d-byte document: the write is not atomic", n, len(full))
	default:
	}
}

// TestWriteJSONAtomicLeavesNoTempFile covers both the success and the failure
// path: an unmarshalable value must not leave a staging file behind either.
func TestWriteJSONAtomicLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.json")
	if err := WriteJSONAtomic(path, jsonDoc{Name: "ok"}, 0o644); err != nil {
		t.Fatalf("WriteJSONAtomic: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "doc.json" {
		t.Errorf("directory holds %d entries, want only doc.json", len(entries))
	}

	// A channel cannot be marshaled. The marshal must fail before anything is
	// created, leaving the good file and no turds.
	err = WriteJSONAtomic(filepath.Join(dir, "bad.json"), make(chan int), 0o644)
	if err == nil {
		t.Fatal("marshaling a channel should fail")
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir after failure: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("a failed write left %d entries, want only the original", len(entries))
	}
}

// TestWriteJSONAtomicMatchesHandRolledEncoding guards the migration: the four
// call sites this helper replaced each did json.MarshalIndent(v, "", "  ")
// followed by a write, so the bytes must be identical or the change is not
// behavior-preserving.
func TestWriteJSONAtomicMatchesHandRolledEncoding(t *testing.T) {
	v := jsonDoc{Name: "demo", Count: 7}
	want, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	path := filepath.Join(t.TempDir(), "doc.json")
	if err := WriteJSONAtomic(path, v, 0o644); err != nil {
		t.Fatalf("WriteJSONAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("helper wrote %q, hand-rolled encoding gives %q", got, want)
	}
}
