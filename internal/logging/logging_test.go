package logging

import (
	"testing"

	charmlog "github.com/charmbracelet/log"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want charmlog.Level
	}{
		{"debug", charmlog.DebugLevel},
		{"DEBUG", charmlog.DebugLevel},
		{"Debug", charmlog.DebugLevel},
		{"info", charmlog.InfoLevel},
		{"INFO", charmlog.InfoLevel},
		{"warn", charmlog.WarnLevel},
		{"WARN", charmlog.WarnLevel},
		{"warning", charmlog.WarnLevel},
		{"error", charmlog.ErrorLevel},
		{"ERROR", charmlog.ErrorLevel},
		{"  debug  ", charmlog.DebugLevel},
		{"", charmlog.InfoLevel},
		{"trace", charmlog.InfoLevel},
		{"verbose", charmlog.InfoLevel},
		{"nonsense", charmlog.InfoLevel},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := parseLevel(c.in)
			if got != c.want {
				t.Fatalf("parseLevel(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestLevelFromEnv(t *testing.T) {
	t.Setenv(LogLevelEnvVar, "")
	if got := levelFromEnv(); got != charmlog.InfoLevel {
		t.Fatalf("unset env: got %v, want InfoLevel", got)
	}

	t.Setenv(LogLevelEnvVar, "debug")
	if got := levelFromEnv(); got != charmlog.DebugLevel {
		t.Fatalf("debug env: got %v, want DebugLevel", got)
	}

	t.Setenv(LogLevelEnvVar, "WARN")
	if got := levelFromEnv(); got != charmlog.WarnLevel {
		t.Fatalf("WARN env: got %v, want WarnLevel", got)
	}

	t.Setenv(LogLevelEnvVar, "bogus")
	if got := levelFromEnv(); got != charmlog.InfoLevel {
		t.Fatalf("bogus env: got %v, want InfoLevel", got)
	}
}
