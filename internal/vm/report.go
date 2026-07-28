package vm

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/util"
)

// goClientExample renders a standalone Go program that connects to the local
// Incus API over mutual TLS using the generated client certificate and key.
//
// It lives in this build-tag-free file (rather than the darwin/cgo runner) so
// the template is compiled and tested on every platform. The template injects
// three runtime values, so it is not gofmt-able Go on its own; it is emitted
// verbatim to disk as incus-client-example.go for the user to run.
func goClientExample(clientCertPath, clientKeyPath string, localAPIPort int) string {
	return fmt.Sprintf(`package main

import (
	"fmt"
	"os"

	incus "github.com/lxc/incus/v6/client"
)

func main() {
	cert, err := os.ReadFile(%q)
	if err != nil {
		panic(err)
	}
	key, err := os.ReadFile(%q)
	if err != nil {
		panic(err)
	}

	client, err := incus.ConnectIncus("https://127.0.0.1:%d", &incus.ConnectionArgs{
		TLSClientCert: string(cert),
		TLSClientKey:  string(key),
		InsecureSkipVerify: true,
	})
	if err != nil {
		panic(err)
	}

	server, _, err := client.GetServer()
	if err != nil {
		panic(err)
	}

	fmt.Println("Connected to", server.Environment.Server)
}
`, clientCertPath, clientKeyPath, localAPIPort)
}

// goClientExampleName is the file the rendered example program is written to
// inside the instance's VM directory.
const goClientExampleName = "incus-client-example.go"

// goClientExamplePerm is the mode of the written example. It is a convenience
// artifact for the user to read and run, not a credential.
const goClientExamplePerm os.FileMode = 0o644

// writeGoClientExample renders the example program into vmDir and returns the
// path it was written to, or "" when the write failed.
//
// The empty return is load-bearing: report.Access.GoClientExamplePath is what
// the report tells the user to go and look at, so publishing a path whose file
// was never created sends them to a file that is not there. Access.SSHConfigPath
// already degrades this way for the same reason. The failure is logged rather
// than returned because an unwritten convenience example must never fail a boot
// that otherwise succeeded.
//
// The write goes through util.WriteFileAtomic (the owner of temp-file-and-rename
// in this tree) so a reader that follows the published path never observes a
// half-written program.
func writeGoClientExample(vmDir, clientCertPath, clientKeyPath string, localAPIPort int) string {
	path := filepath.Join(vmDir, goClientExampleName)
	body := []byte(goClientExample(clientCertPath, clientKeyPath, localAPIPort))
	if err := util.WriteFileAtomic(path, body, goClientExamplePerm); err != nil {
		logging.L().Warn("write incus client example", "path", path, "err", err)
		return ""
	}
	return path
}
