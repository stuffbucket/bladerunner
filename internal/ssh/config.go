package ssh

import (
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

const sshConfigTemplate = `# Bladerunner SSH configuration
# Generated automatically - do not edit manually
#
# Usage:
#   ssh -F {{.ConfigPath}} bladerunner
#
# Or add to ~/.ssh/config:
#   Include {{.ConfigPath}}

Host bladerunner
    HostName 127.0.0.1
    Port {{.Port}}
    User {{.User}}
    IdentityFile {{.IdentityFile}}
    IdentitiesOnly yes
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
    LogLevel ERROR
`

// ConfigParams holds parameters for generating SSH config.
type ConfigParams struct {
	Port         int
	User         string
	IdentityFile string
	ConfigPath   string
}

// WriteSSHConfig writes an SSH config file for Bladerunner to the config directory.
// Returns the path to the generated config file.
func WriteSSHConfig(port int, user string, identityFile string) (string, error) {
	configPath := filepath.Join(ConfigDir(), "ssh", "config")

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return "", fmt.Errorf("create ssh config directory: %w", err)
	}

	params := ConfigParams{
		Port:         port,
		User:         user,
		IdentityFile: identityFile,
		ConfigPath:   configPath,
	}

	tmpl, err := template.New("ssh_config").Parse(sshConfigTemplate)
	if err != nil {
		return "", fmt.Errorf("parse ssh config template: %w", err)
	}

	f, err := os.OpenFile(configPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create ssh config file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := tmpl.Execute(f, params); err != nil {
		return "", fmt.Errorf("write ssh config: %w", err)
	}

	return configPath, nil
}

// ConfigPath returns the path to the SSH config file.
func ConfigPath() string {
	return filepath.Join(Dir(), "config")
}

// Command returns the SSH command to connect to the VM.
func Command(configPath string) string {
	return fmt.Sprintf("ssh -F %s bladerunner", configPath)
}
