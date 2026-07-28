package main

import (
	"os/exec"
	"reflect"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/ssh"
)

func TestSSHArgvFor(t *testing.T) {
	// sshArgvFor resolves the ssh binary; skip if it is not on PATH so the test
	// stays hermetic in minimal environments.
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skipf("ssh not on PATH: %v", err)
	}

	const cfg = "/tmp/ssh-config"
	// The default instance's alias, which is what every verb resolved to
	// before --instance was routed through sshTarget.
	alias := ssh.HostAlias(ssh.DefaultInstanceName)
	tests := []struct {
		name string
		opts []string
		tail []string
		want []string
	}{
		{
			name: "no opts or tail (incus base)",
			want: []string{"ssh", "-F", cfg, alias},
		},
		{
			name: "pty opt (shell)",
			opts: []string{"-t"},
			want: []string{"ssh", "-F", cfg, "-t", alias},
		},
		{
			name: "opts and tail (reconnect)",
			opts: []string{"-o", "BatchMode=yes"},
			tail: []string{"echo", "hi"},
			want: []string{"ssh", "-F", cfg, "-o", "BatchMode=yes", alias, "echo", "hi"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, argv, err := sshArgvFor(alias, cfg, tt.opts, tt.tail...)
			if err != nil {
				t.Fatalf("sshArgvFor returned error: %v", err)
			}
			if path == "" {
				t.Error("sshArgvFor returned empty ssh path")
			}
			if !reflect.DeepEqual(argv, tt.want) {
				t.Errorf("argv = %v, want %v", argv, tt.want)
			}
		})
	}
}
