package main

import "testing"

func TestValidEventTypes(t *testing.T) {
	for _, ok := range []string{"lifecycle", "operation", "logging", "network-acl"} {
		if _, found := validEventTypes[ok]; !found {
			t.Errorf("expected %q to be a valid event type", ok)
		}
	}
	for _, bad := range []string{"", "metrics", "unknown"} {
		if _, found := validEventTypes[bad]; found {
			t.Errorf("did not expect %q to be a valid event type", bad)
		}
	}
}
