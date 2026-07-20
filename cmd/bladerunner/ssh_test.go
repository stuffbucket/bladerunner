package main

import (
	"os/exec"
	"reflect"
	"testing"
)

func TestSSHArgv(t *testing.T) {
	// sshArgv resolves the ssh binary; skip if it is not on PATH so the test
	// stays hermetic in minimal environments.
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skipf("ssh not on PATH: %v", err)
	}

	const cfg = "/tmp/ssh-config"
	tests := []struct {
		name string
		opts []string
		tail []string
		want []string
	}{
		{
			name: "no opts or tail (incus base)",
			want: []string{"ssh", "-F", cfg, sshHostAlias},
		},
		{
			name: "pty opt (shell)",
			opts: []string{"-t"},
			want: []string{"ssh", "-F", cfg, "-t", sshHostAlias},
		},
		{
			name: "opts and tail (reconnect)",
			opts: []string{"-o", "BatchMode=yes"},
			tail: []string{"echo", "hi"},
			want: []string{"ssh", "-F", cfg, "-o", "BatchMode=yes", sshHostAlias, "echo", "hi"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, argv, err := sshArgv(cfg, tt.opts, tt.tail...)
			if err != nil {
				t.Fatalf("sshArgv returned error: %v", err)
			}
			if path == "" {
				t.Error("sshArgv returned empty ssh path")
			}
			if !reflect.DeepEqual(argv, tt.want) {
				t.Errorf("argv = %v, want %v", argv, tt.want)
			}
		})
	}
}
