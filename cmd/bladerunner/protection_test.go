package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/instance"
)

// `br status --json` has to carry both halves: a code a script branches on and
// a sentence a person reads. A consumer that only got the sentence would have
// to pattern-match English to decide whether to stop the VM.
func TestStatusReportCarriesEjectProtection(t *testing.T) {
	get := func(string) string { return "" }

	report := runningStatusReport("running",
		protectionReportFor(instance.KindCartridge, instance.ProtectionWatchFailed), get)
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	prot, ok := decoded["unmount_protection"].(map[string]any)
	if !ok {
		t.Fatalf("unmount_protection = %v, want an object:\n%s", decoded["unmount_protection"], raw)
	}
	if prot["protected"] != false {
		t.Errorf("protected = %v, want false", prot["protected"])
	}
	if prot["code"] != instance.ProtectionWatchFailed.Code() {
		t.Errorf("code = %v, want %q", prot["code"], instance.ProtectionWatchFailed.Code())
	}
	if prot["reason"] != instance.ProtectionWatchFailed.Reason() {
		t.Errorf("reason = %v, want the human sentence", prot["reason"])
	}

	// A flat instance has no cartridge, so the key is absent rather than
	// present-and-meaningless.
	flat, err := json.Marshal(runningStatusReport("running",
		protectionReportFor(instance.KindFlat, instance.ProtectionUnrecorded), get))
	if err != nil {
		t.Fatalf("marshal flat: %v", err)
	}
	if strings.Contains(string(flat), "unmount_protection") {
		t.Errorf("a flat instance reports an eject state:\n%s", flat)
	}
}

// The boot summary is the one moment the user is looking at this instance, and
// until now a lost veto appeared only in the holder's log. It must speak for a
// real loss of protection — and stay quiet for a host that never had
// DiskArbitration to lose, which would otherwise warn on every single boot.
func TestBootWarnsOnlyWhenProtectionWasActuallyLost(t *testing.T) {
	for _, tc := range []struct {
		name  string
		p     *unmountProtectionReport
		warns bool
	}{
		{"no cartridge at all", nil, false},
		{"armed", protectionReportFor(instance.KindCartridge, instance.ProtectionArmed), false},
		{"platform has no DiskArbitration", protectionReportFor(instance.KindCartridge, instance.ProtectionUnsupported), false},
		{"session could not be opened", protectionReportFor(instance.KindCartridge, instance.ProtectionNoSession), true},
		{"watcher would not register", protectionReportFor(instance.KindCartridge, instance.ProtectionWatchFailed), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeUnprotectedCartridge(&buf, tc.p); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := buf.Len() > 0; got != tc.warns {
				t.Fatalf("warned = %v, want %v:\n%s", got, tc.warns, buf.String())
			}
			if !tc.warns {
				return
			}
			for _, want := range []string{"eject protection is off", tc.p.Reason, unprotectedConsequence} {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("warning is missing %q:\n%s", want, buf.String())
				}
			}
		})
	}
}

// The marker in front of each note is the reader's triage: only a host that
// COULD have been protected and was not is a warning. Marking a platform
// without DiskArbitration, or a record that predates the field, the same way
// buries the one line worth acting on among lines that are not.
func TestProtectionNotesMarkOnlyRealLossesAsWarnings(t *testing.T) {
	listing := func(name string, p instance.Protection) instanceListing {
		return instanceListing{
			Name:              name,
			Kind:              string(instance.KindCartridge),
			UnmountProtection: protectionReportFor(instance.KindCartridge, p),
		}
	}
	var buf bytes.Buffer
	err := writeProtectionNotes(&buf, []instanceListing{
		listing("lost", instance.ProtectionNoSession),
		listing("platform", instance.ProtectionUnsupported),
		listing("old", instance.ProtectionUnrecorded),
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, line := range strings.Split(buf.String(), "\n") {
		switch {
		case strings.Contains(line, "lost:"):
			if !strings.Contains(line, "!") {
				t.Errorf("a real loss of protection is not marked as a warning: %q", line)
			}
		case strings.Contains(line, "platform:"), strings.Contains(line, "old:"):
			if strings.Contains(line, "!") {
				t.Errorf("a remark is marked as a warning: %q", line)
			}
		}
	}
}
