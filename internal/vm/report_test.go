package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoClientExample(t *testing.T) {
	const (
		certPath = "/tmp/bladerunner/client.crt"
		keyPath  = "/tmp/bladerunner/client.key"
		apiPort  = 18443
	)

	got := goClientExample(certPath, keyPath, apiPort)

	tests := []struct {
		name string
		want string
	}{
		{"package clause", "package main"},
		{"incus import", `incus "github.com/lxc/incus/v6/client"`},
		{"cert path injected", `os.ReadFile("` + certPath + `")`},
		{"key path injected", `os.ReadFile("` + keyPath + `")`},
		{"api port injected", `ConnectIncus("https://127.0.0.1:18443"`},
		{"insecure skip verify", "InsecureSkipVerify: true"},
		{"prints server env", `fmt.Println("Connected to", server.Environment.Server)`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(got, tt.want) {
				t.Errorf("goClientExample() missing %q\n---\n%s", tt.want, got)
			}
		})
	}
}

// TestGoClientExampleEscapesPaths guards against path values with characters
// that would break the generated Go source if injected without %q quoting.
func TestGoClientExampleEscapesPaths(t *testing.T) {
	got := goClientExample(`/tmp/a"b\c`, "/tmp/key", 1)
	const wantCert = `os.ReadFile("/tmp/a\"b\\c")`
	if !strings.Contains(got, wantCert) {
		t.Errorf("goClientExample() did not %%q-escape cert path\nwant substring %q\n---\n%s", wantCert, got)
	}
}

// TestWriteGoClientExampleReportsFailure pins that a failed write is NOT
// silently swallowed: the report must not advertise a path to a file that does
// not exist. This mirrors how Access.SSHConfigPath already degrades when its
// write fails.
func TestWriteGoClientExampleReportsFailure(t *testing.T) {
	// A VM dir that does not exist: the write cannot succeed.
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	if got := writeGoClientExample(missing, "/c.crt", "/c.key", 18443); got != "" {
		t.Errorf("writeGoClientExample() into a missing dir = %q, want \"\"", got)
	}
}

// TestWriteGoClientExampleWritesFile pins the success path: the returned path is
// the file that was actually written, and it holds the rendered program.
func TestWriteGoClientExampleWritesFile(t *testing.T) {
	dir := t.TempDir()
	got := writeGoClientExample(dir, "/c.crt", "/c.key", 18443)
	if got == "" {
		t.Fatal("writeGoClientExample() = \"\", want the written path")
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read %s: %v", got, err)
	}
	if !strings.Contains(string(b), "/c.crt") {
		t.Errorf("written example does not carry the client cert path: %s", b)
	}
}
