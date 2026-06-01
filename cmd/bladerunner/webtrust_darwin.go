//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// incusCertCommonName is the subject CN of the Incus server cert for the default
// guest hostname ("bladerunner"): Incus issues it as root@<hostname>. Used to
// locate the cert for removal.
const (
	incusCertCommonName = "root@bladerunner"
	securityCmd         = "security"
)

func loginKeychain() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "Keychains", "login.keychain-db")
}

// installTrustedCert adds the PEM at pemPath to the macOS keychain as an SSL-
// trusted root. trustRoot is correct because the Incus cert is self-signed (its
// own issuer). macOS prompts the user to authorize the trust change.
func installTrustedCert(pemPath string, system bool) error {
	name := securityCmd
	args := []string{"add-trusted-cert", "-r", "trustRoot", "-p", "ssl", "-k", loginKeychain(), pemPath}
	if system {
		name = "sudo"
		args = []string{securityCmd, "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "ssl", "-k", "/Library/Keychains/System.keychain", pemPath}
	}
	fmt.Println(subtle("macOS will prompt you to authorize the keychain change."))
	c := exec.CommandContext(context.Background(), name, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("add trusted cert: %w", err)
	}
	return nil
}

func removeTrustedCert(system bool) error {
	name := securityCmd
	args := []string{"delete-certificate", "-c", incusCertCommonName, loginKeychain()}
	if system {
		name = "sudo"
		args = []string{securityCmd, "delete-certificate", "-c", incusCertCommonName, "/Library/Keychains/System.keychain"}
	}
	c := exec.CommandContext(context.Background(), name, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("delete certificate %q: %w", incusCertCommonName, err)
	}
	return nil
}
