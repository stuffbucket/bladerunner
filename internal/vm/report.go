package vm

import "fmt"

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
