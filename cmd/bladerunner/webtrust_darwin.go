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
	// systemKeychain is the machine-wide trust store `--system` targets.
	systemKeychain = "/Library/Keychains/System.keychain"
)

func loginKeychain() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "Keychains", "login.keychain-db")
}

// trustCertCommand builds the `security add-trusted-cert` invocation that
// installs pemPath as an SSL-trusted root, and the keychain it lands in.
// Separated from the exec so untrustCertCommand can be held against it (see
// TestTrustAndUntrustNameTheSameKeychain) without touching a real keychain.
func trustCertCommand(pemPath string, system bool) (name string, args []string) {
	if system {
		return sudoCmd, []string{securityCmd, "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "ssl", "-k", systemKeychain, pemPath}
	}
	return securityCmd, []string{"add-trusted-cert", "-r", "trustRoot", "-p", "ssl", "-k", loginKeychain(), pemPath}
}

// untrustCertCommand builds the `security delete-certificate` invocation that
// removes what trustCertCommand added, from the same keychain.
func untrustCertCommand(system bool) (name string, args []string) {
	if system {
		return sudoCmd, []string{securityCmd, "delete-certificate", "-c", incusCertCommonName, systemKeychain}
	}
	return securityCmd, []string{"delete-certificate", "-c", incusCertCommonName, loginKeychain()}
}

// installTrustedCert adds the PEM at pemPath to the macOS keychain as an SSL-
// trusted root. trustRoot is correct because the Incus cert is self-signed (its
// own issuer). macOS prompts the user to authorize the trust change.
func installTrustedCert(pemPath string, system bool) error {
	name, args := trustCertCommand(pemPath, system)
	fmt.Println(subtle("macOS will prompt you to authorize the keychain change."))
	c := exec.CommandContext(context.Background(), name, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("add trusted cert: %w", err)
	}
	return nil
}

// removeTrustedCert deletes the certificate installed by installTrustedCert,
// from the keychain the same --system value named.
func removeTrustedCert(system bool) error {
	name, args := untrustCertCommand(system)
	c := exec.CommandContext(context.Background(), name, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("delete certificate %q: %w", incusCertCommonName, err)
	}
	return nil
}
