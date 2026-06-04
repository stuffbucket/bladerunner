package disk

import (
	"os"
	"path/filepath"
	"testing"
)

// withConfigHome points DefaultDiskDir at <tmp>/bladerunner/disks by setting
// XDG_CONFIG_HOME, and returns that disks dir. It restores the env on cleanup.
func withConfigHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	return filepath.Join(tmp, "bladerunner", "disks")
}

func TestBuiltinsLoad(t *testing.T) {
	withConfigHome(t) // empty user dir

	cat, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	for _, name := range []string{"incus", "debian-trixie-gui"} {
		e, ok := cat.Lookup(name)
		if !ok {
			t.Fatalf("expected builtin %q present", name)
		}
		if e.Origin != OriginBuiltin {
			t.Fatalf("%q origin = %q, want builtin", name, e.Origin)
		}
		if err := e.Manifest.Validate(); err != nil {
			t.Fatalf("%q builtin invalid: %v", name, err)
		}
	}
}

func TestUserOverridesBuiltin(t *testing.T) {
	dir := withConfigHome(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const desc = "user-overridden incus"
	body := `{"name":"incus","description":"` + desc + `","image":{"hosted":true},"boot":{"mode":"headless"}}`
	if err := os.WriteFile(filepath.Join(dir, "incus.disk"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cat, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	e, ok := cat.Lookup("incus")
	if !ok {
		t.Fatal("incus not found")
	}
	if e.Origin != OriginUser {
		t.Fatalf("origin = %q, want user", e.Origin)
	}
	if e.Manifest.Description != desc {
		t.Fatalf("description = %q, want %q", e.Manifest.Description, desc)
	}
}

func TestMalformedUserFileSkipped(t *testing.T) {
	dir := withConfigHome(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.disk"), []byte(`{garbage`), 0o644); err != nil {
		t.Fatal(err)
	}
	ok := `{"name":"ok","image":{"hosted":true},"boot":{"mode":"headless"}}`
	if err := os.WriteFile(filepath.Join(dir, "ok.disk"), []byte(ok), 0o644); err != nil {
		t.Fatal(err)
	}

	cat, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog should not error on malformed file: %v", err)
	}
	if _, found := cat.Lookup("bad"); found {
		t.Fatal("malformed disk should be skipped")
	}
	if _, found := cat.Lookup("ok"); !found {
		t.Fatal("valid disk should be present")
	}
}

func TestMissingUserDirNotError(t *testing.T) {
	withConfigHome(t) // dir never created

	cat, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog with missing user dir: %v", err)
	}
	if _, ok := cat.Lookup("incus"); !ok {
		t.Fatal("expected builtins to still load")
	}
}

func TestLookupNotFound(t *testing.T) {
	withConfigHome(t)
	cat, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cat.Lookup("does-not-exist"); ok {
		t.Fatal("expected not found")
	}
}

func TestList(t *testing.T) {
	withConfigHome(t)
	cat, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	list := cat.List()
	if len(list) < 2 {
		t.Fatalf("expected >= 2 entries, got %d", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].Manifest.Name > list[i].Manifest.Name {
			t.Fatalf("List not sorted: %q > %q", list[i-1].Manifest.Name, list[i].Manifest.Name)
		}
	}
}
