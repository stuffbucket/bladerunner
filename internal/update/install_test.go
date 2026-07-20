package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIsHomebrewPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/opt/homebrew/bin/br", true},
		{"/opt/homebrew/Cellar/bladerunner/0.4.7/bin/br", true},
		{"/usr/local/Homebrew/bin/br", true},
		{"/home/linuxbrew/.linuxbrew/bin/br", true},
		{"/Applications/Bladerunner.app/Contents/MacOS/br", false},
		{"/Users/x/build/bin/br", false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			if got := isHomebrewPath(tc.path); got != tc.want {
				t.Fatalf("isHomebrewPath(%q)=%v want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestInstallTarget(t *testing.T) {
	t.Run("homebrew refused", func(t *testing.T) {
		_, err := installTarget("/opt/homebrew/bin/br")
		if !errors.Is(err, ErrHomebrewManaged) {
			t.Fatalf("expected ErrHomebrewManaged, got %v", err)
		}
	})

	t.Run("app bundle resolved", func(t *testing.T) {
		got, err := installTarget("/Applications/Bladerunner.app/Contents/MacOS/br")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/Applications/Bladerunner.app" {
			t.Fatalf("bundle root = %q", got)
		}
	})

	t.Run("not a bundle refused", func(t *testing.T) {
		_, err := installTarget("/Users/x/go/bin/br")
		if !errors.Is(err, ErrNotAppBundle) {
			t.Fatalf("expected ErrNotAppBundle, got %v", err)
		}
	})
}

func TestTopAppComponent(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"Bladerunner.app/Contents/MacOS/br", "Bladerunner.app"},
		{"./Bladerunner.app/Contents", "Bladerunner.app"},
		{"Bladerunner.app", "Bladerunner.app"},
		{"notes.txt", ""},
		{"nested/Bladerunner.app", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := topAppComponent(tc.name); got != tc.want {
				t.Fatalf("topAppComponent(%q)=%q want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestExtractAppBundle(t *testing.T) {
	tarball := buildAppTarball(t, map[string]string{
		"Contents/Info.plist":  "<plist/>",
		"Contents/MacOS/br":    "#!/bin/sh\n",
		"Contents/Resources/x": "res",
	})

	dest := t.TempDir()
	appRoot, err := extractAppBundle(tarball, dest)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if appRoot != filepath.Join(dest, "Bladerunner.app") {
		t.Fatalf("appRoot = %q", appRoot)
	}
	got, err := os.ReadFile(filepath.Join(appRoot, "Contents", "MacOS", "br"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != "#!/bin/sh\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestExtractAppBundle_RejectsTraversal(t *testing.T) {
	// A tarball with an entry that escapes the destination must be rejected.
	tarball := buildRawTarball(t, []tarEntry{
		{name: "Bladerunner.app/Contents/MacOS/br", body: "ok"},
		{name: "../evil.sh", body: "pwned"},
	})
	if _, err := extractAppBundle(tarball, t.TempDir()); err == nil {
		t.Fatal("expected extract to reject path traversal")
	}
}

func TestExtractAppBundle_NoBundle(t *testing.T) {
	tarball := buildRawTarball(t, []tarEntry{{name: "notes.txt", body: "hi"}})
	if _, err := extractAppBundle(tarball, t.TempDir()); err == nil {
		t.Fatal("expected error when tarball has no .app")
	}
}

func TestSwapBundle(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "Bladerunner.app")
	if err := os.MkdirAll(filepath.Join(dst, "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "Contents", "marker"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	// New bundle staged on the same filesystem.
	staging, err := os.MkdirTemp(root, ".stage-*")
	if err != nil {
		t.Fatal(err)
	}
	newApp := filepath.Join(staging, "Bladerunner.app")
	if err := os.MkdirAll(filepath.Join(newApp, "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newApp, "Contents", "marker"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := swapBundle(dst, newApp); err != nil {
		t.Fatalf("swap: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "Contents", "marker"))
	if err != nil {
		t.Fatalf("read after swap: %v", err)
	}
	if string(got) != "new" {
		t.Fatalf("marker = %q want new", got)
	}
	// Backup should be cleaned up.
	if _, err := os.Stat(filepath.Join(root, ".Bladerunner.app.old")); !os.IsNotExist(err) {
		t.Fatalf("expected backup removed, stat err=%v", err)
	}
}

func TestSwapBundle_FreshInstall(t *testing.T) {
	// When no bundle exists yet, swap should just place the new one.
	root := t.TempDir()
	dst := filepath.Join(root, "Bladerunner.app")

	staging, err := os.MkdirTemp(root, ".stage-*")
	if err != nil {
		t.Fatal(err)
	}
	newApp := filepath.Join(staging, "Bladerunner.app")
	if err := os.MkdirAll(newApp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := swapBundle(dst, newApp); err != nil {
		t.Fatalf("swap fresh: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("expected bundle at dst: %v", err)
	}
}

func TestRelaunch_NonAppRejected(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("relaunch is macOS-only")
	}
	if err := relaunch(context.Background(), "/tmp/not-a-bundle"); err == nil {
		t.Fatal("expected relaunch to reject a non-.app target")
	}
}
