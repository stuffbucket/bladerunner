package vm

import (
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
