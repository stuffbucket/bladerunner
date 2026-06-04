package disk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Shared test literals (kept as constants to satisfy goconst).
const (
	tArchARM64        = "arm64"
	errBadName        = "invalid disk name"
	errMultiImageSrc  = "multiple image sources"
	errBadSHA256Field = "sha256 must be"
)

func TestManifestParseValidate(t *testing.T) {
	tests := []struct {
		name            string
		jsonInput       string
		wantErr         bool
		wantErrContains string
	}{
		{
			name:      "valid hosted",
			jsonInput: `{"name":"incus","image":{"hosted":true},"boot":{"mode":"headless"}}`,
		},
		{
			name: "valid arches both",
			jsonInput: `{"name":"deb","image":{"arches":{
				"arm64":{"url":"https://x/a.qcow2"},
				"amd64":{"url":"https://x/b.qcow2"}}},"boot":{"mode":"gui"}}`,
		},
		{
			name:      "valid path",
			jsonInput: `{"name":"local","image":{"path":"/tmp/x.qcow2"},"boot":{"mode":"headless"}}`,
		},
		{
			name:      "valid gui",
			jsonInput: `{"name":"g","image":{"hosted":true},"boot":{"mode":"gui","autologin":true}}`,
		},
		{
			name:      "valid sha256",
			jsonInput: `{"name":"s","image":{"arches":{"arm64":{"url":"https://x/a","sha256":"` + strings.Repeat("a", 64) + `"}}},"boot":{"mode":"headless"}}`,
		},
		{
			name:            "empty name",
			jsonInput:       `{"name":"","image":{"hosted":true},"boot":{"mode":"headless"}}`,
			wantErr:         true,
			wantErrContains: "name is required",
		},
		{
			name:            "name with slash",
			jsonInput:       `{"name":"a/b","image":{"hosted":true},"boot":{"mode":"headless"}}`,
			wantErr:         true,
			wantErrContains: errBadName,
		},
		{
			name:            "name with dotdot",
			jsonInput:       `{"name":"..","image":{"hosted":true},"boot":{"mode":"headless"}}`,
			wantErr:         true,
			wantErrContains: errBadName,
		},
		{
			name:            "name uppercase",
			jsonInput:       `{"name":"Incus","image":{"hosted":true},"boot":{"mode":"headless"}}`,
			wantErr:         true,
			wantErrContains: errBadName,
		},
		{
			name:            "name with space",
			jsonInput:       `{"name":"a b","image":{"hosted":true},"boot":{"mode":"headless"}}`,
			wantErr:         true,
			wantErrContains: errBadName,
		},
		{
			name:            "empty boot mode",
			jsonInput:       `{"name":"x","image":{"hosted":true},"boot":{"mode":""}}`,
			wantErr:         true,
			wantErrContains: "invalid boot mode",
		},
		{
			name:            "bad boot mode",
			jsonInput:       `{"name":"x","image":{"hosted":true},"boot":{"mode":"headed"}}`,
			wantErr:         true,
			wantErrContains: "invalid boot mode",
		},
		{
			name:            "zero image sources",
			jsonInput:       `{"name":"x","image":{},"boot":{"mode":"headless"}}`,
			wantErr:         true,
			wantErrContains: "no image source",
		},
		{
			name:            "hosted plus arches",
			jsonInput:       `{"name":"x","image":{"hosted":true,"arches":{"arm64":{"url":"https://x"}}},"boot":{"mode":"headless"}}`,
			wantErr:         true,
			wantErrContains: errMultiImageSrc,
		},
		{
			name:            "hosted plus path",
			jsonInput:       `{"name":"x","image":{"hosted":true,"path":"/tmp/x"},"boot":{"mode":"headless"}}`,
			wantErr:         true,
			wantErrContains: errMultiImageSrc,
		},
		{
			name:            "arches plus path",
			jsonInput:       `{"name":"x","image":{"path":"/tmp/x","arches":{"arm64":{"url":"https://x"}}},"boot":{"mode":"headless"}}`,
			wantErr:         true,
			wantErrContains: errMultiImageSrc,
		},
		{
			name:            "arches empty url",
			jsonInput:       `{"name":"x","image":{"arches":{"arm64":{"url":""}}},"boot":{"mode":"headless"}}`,
			wantErr:         true,
			wantErrContains: "url is required",
		},
		{
			name:            "sha256 too short",
			jsonInput:       `{"name":"x","image":{"arches":{"arm64":{"url":"https://x","sha256":"abcd"}}},"boot":{"mode":"headless"}}`,
			wantErr:         true,
			wantErrContains: errBadSHA256Field,
		},
		{
			name:            "sha256 uppercase",
			jsonInput:       `{"name":"x","image":{"arches":{"arm64":{"url":"https://x","sha256":"` + strings.Repeat("A", 64) + `"}}},"boot":{"mode":"headless"}}`,
			wantErr:         true,
			wantErrContains: errBadSHA256Field,
		},
		{
			name:            "sha256 non-hex",
			jsonInput:       `{"name":"x","image":{"arches":{"arm64":{"url":"https://x","sha256":"` + strings.Repeat("g", 64) + `"}}},"boot":{"mode":"headless"}}`,
			wantErr:         true,
			wantErrContains: errBadSHA256Field,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.jsonInput))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.wantErrContains != "" && !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestManifestRoundTrip(t *testing.T) {
	m := &Manifest{
		Name:  "rt",
		Image: ImageSpec{Hosted: true},
		Boot:  BootSpec{Mode: BootModeHeadless},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// omitempty fields must drop when zero.
	s := string(b)
	for _, drop := range []string{"description", "version", "cpus", "memory_gib", "disk_size_gib", "autologin", "path", "arches"} {
		if strings.Contains(s, `"`+drop+`"`) {
			t.Fatalf("expected %q to be omitted, got %s", drop, s)
		}
	}

	var got Manifest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != m.Name || got.Image.Hosted != true || got.Boot.Mode != BootModeHeadless {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()

	valid := filepath.Join(dir, "ok.disk")
	if err := os.WriteFile(valid, []byte(`{"name":"ok","image":{"hosted":true},"boot":{"mode":"headless"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(valid); err != nil {
		t.Fatalf("Load valid: %v", err)
	}

	malformed := filepath.Join(dir, "bad.disk")
	if err := os.WriteFile(malformed, []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(malformed); err == nil {
		t.Fatalf("Load malformed: expected error")
	} else if !strings.Contains(err.Error(), "load disk") {
		t.Fatalf("expected wrapped error, got %v", err)
	}

	if _, err := Load(filepath.Join(dir, "nope.disk")); err == nil {
		t.Fatalf("Load nonexistent: expected error")
	}
}

func TestValidName(t *testing.T) {
	cases := map[string]bool{
		"incus":             true,
		"debian-trixie-gui": true,
		"a1":                true,
		"":                  false,
		"-leading":          false,
		"Upper":             false,
		"has space":         false,
		"has/slash":         false,
		"..":                false,
		"dot.name":          false,
	}
	for in, want := range cases {
		if got := ValidName(in); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestValidSHA256(t *testing.T) {
	good := "ab" + strings.Repeat("0", 62)
	cases := map[string]bool{
		good:                           true,
		"":                             false,
		strings.Repeat("a", 63):        false, // too short
		strings.Repeat("a", 65):        false, // too long
		"AB" + strings.Repeat("0", 62): false, // uppercase
		"zz" + strings.Repeat("0", 62): false, // non-hex
	}
	for in, want := range cases {
		if got := ValidSHA256(in); got != want {
			t.Errorf("ValidSHA256(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestClone(t *testing.T) {
	orig := &Manifest{
		Name:  "src",
		Image: ImageSpec{Arches: map[string]ArchImage{tArchARM64: {URL: "https://x/a.qcow2"}}},
		Boot:  BootSpec{Mode: BootModeHeadless},
	}
	cp := orig.Clone()
	cp.Name = "dst"
	cp.Image.Arches[tArchARM64] = ArchImage{URL: "https://y/b.qcow2", SHA256: strings.Repeat("a", 64)}

	if orig.Name != "src" {
		t.Errorf("Clone aliased Name: orig.Name = %q", orig.Name)
	}
	if orig.Image.Arches[tArchARM64].URL != "https://x/a.qcow2" {
		t.Errorf("Clone aliased Arches map: %q", orig.Image.Arches[tArchARM64].URL)
	}
}

func TestCloneIsolatesShare(t *testing.T) {
	orig := &Manifest{
		Name:  "src",
		Image: ImageSpec{Path: "/x/root.img"},
		Boot:  BootSpec{Mode: BootModeHeadless},
		Share: &ShareSpec{Tag: "bladerunner-share", GuestPath: "/mnt/share"},
	}
	cp := orig.Clone()
	cp.Share.Tag = "other"
	if orig.Share.Tag != "bladerunner-share" {
		t.Errorf("Clone aliased Share pointer: orig.Share.Tag = %q", orig.Share.Tag)
	}
}

func TestManifestShareValidation(t *testing.T) {
	base := func() *Manifest {
		return &Manifest{
			Name:  "demo",
			Image: ImageSpec{Path: "/x/root.img"},
			Boot:  BootSpec{Mode: BootModeHeadless},
		}
	}

	tests := []struct {
		name    string
		share   *ShareSpec
		wantErr bool
	}{
		{"no share is valid", nil, false},
		{"default RW share is valid", &ShareSpec{Tag: "bladerunner-share", GuestPath: "/mnt/share"}, false},
		{"empty share defaults later, still valid", &ShareSpec{}, false},
		{"relative guest path rejected", &ShareSpec{GuestPath: "mnt/share"}, true},
		{"tag with slash rejected", &ShareSpec{Tag: "bad/tag"}, true},
		{"tag with space rejected", &ShareSpec{Tag: "bad tag"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			m.Share = tc.share
			err := m.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected validation error for %+v", tc.share)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}
