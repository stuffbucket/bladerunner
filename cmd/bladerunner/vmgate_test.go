package main

import (
	"strings"
	"testing"
)

func TestConfirmStartVMFrom(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"empty default yes", "\n", true},
		{"eof default yes", "", true},
		{"y", "y\n", true},
		{"yes", "yes\n", true},
		{"uppercase Y", "Y\n", true},
		{"yes with spaces", "  yes  \n", true},
		{"n", "n\n", false},
		{"no", "no\n", false},
		{"garbage", "maybe\n", false},
		{"explicit n no newline", "n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := confirmStartVMFrom(strings.NewReader(tc.input))
			if got != tc.want {
				t.Fatalf("confirmStartVMFrom(%q)=%v want %v", tc.input, got, tc.want)
			}
		})
	}
}
