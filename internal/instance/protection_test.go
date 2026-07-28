package instance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every state has to render as something a user can act on. The failure this
// guards against is not a crash: it is a front end that prints a constant name,
// or an empty cell, at the one moment a person needs to be told that their
// cartridge can be pulled out from under a running guest.
//
// Run with -v to print the reason table.
func TestProtectionRendersEveryState(t *testing.T) {
	cases := []struct {
		state        Protection
		wantCode     string
		wantArmed    bool
		wantDegraded bool
	}{
		{ProtectionUnrecorded, "unrecorded", false, false},
		{ProtectionArmed, "armed", true, false},
		{ProtectionNoCartridge, "no-cartridge", false, true},
		{ProtectionNoDevNode, "no-device-node", false, true},
		{ProtectionUnreadableDevNode, "unreadable-device-node", false, true},
		{ProtectionNoSession, "diskarbitration-unavailable", false, true},
		{ProtectionWatchFailed, "watch-registration-failed", false, true},
		{ProtectionUnsupported, "unsupported-platform", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.wantCode, func(t *testing.T) {
			if got := tc.state.Code(); got != tc.wantCode {
				t.Errorf("Code() = %q, want %q", got, tc.wantCode)
			}
			if got := tc.state.Armed(); got != tc.wantArmed {
				t.Errorf("Armed() = %v, want %v", got, tc.wantArmed)
			}
			if got := tc.state.Degraded(); got != tc.wantDegraded {
				t.Errorf("Degraded() = %v, want %v", got, tc.wantDegraded)
			}

			reason := tc.state.Reason()
			switch {
			case reason == "":
				t.Error("Reason() is empty; a state with no sentence cannot be reported")
			case strings.Contains(reason, "Protection"):
				t.Errorf("Reason() = %q leaks the constant name; it must be user language", reason)
			case strings.ToUpper(reason[:1]) == reason[:1] && reason[:1] != "m" && reason[:1] != "e":
				// Reasons are clauses that complete a sentence, so they start
				// lowercase — except where the first word is a proper noun.
				if reason[:5] != "macOS" && reason[:5] != "eject" {
					t.Errorf("Reason() = %q starts with a capital; it is a clause, not a sentence", reason)
				}
			}
			t.Logf("%-28s -> %s", tc.state.Code(), reason)
		})
	}

	// An unrecognized value (a record written by a newer bladerunner) must
	// still say something rather than render blank.
	if got := Protection("from-the-future").Reason(); !strings.Contains(got, "from-the-future") {
		t.Errorf("Reason() for an unknown state = %q, want it to name the value", got)
	}
}

// A registry record written before Entry.UnmountProtection existed must still
// load — and must decode to "not recorded", never to "armed". Reporting an old
// record as protected is the same lie as reporting nothing at all: the user
// would leave a cartridge in believing an eject is held when it is not.
func TestEntryWithoutUnmountProtectionLoadsAsUnrecorded(t *testing.T) {
	root := t.TempDir()
	dir := Dir(root)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		t.Fatalf("create registry dir: %v", err)
	}
	old := `{
  "name": "legacy",
  "kind": "cartridge",
  "stateDir": "/Volumes/bladerunner-legacy",
  "mountpoint": "/Volumes/bladerunner-legacy",
  "devNode": "/dev/disk9s1",
  "pid": 4242,
  "ports": {"ssh": 51001}
}`
	if err := os.WriteFile(filepath.Join(dir, "legacy.json"), []byte(old), entryPerm); err != nil {
		t.Fatalf("write legacy entry: %v", err)
	}

	e, err := Read(root, "legacy")
	if err != nil {
		t.Fatalf("Read a record that predates the field: %v", err)
	}
	if e.UnmountProtection != ProtectionUnrecorded {
		t.Errorf("UnmountProtection = %q, want %q", e.UnmountProtection, ProtectionUnrecorded)
	}
	if e.UnmountProtection.Armed() {
		t.Error("a record that predates the field must never report itself protected")
	}
	if e.DevNode != "/dev/disk9s1" || e.Ports.SSH != 51001 {
		t.Errorf("the rest of the legacy record did not survive: %+v", e)
	}
}

// The value has to survive the registry round trip, because the holder writes
// it in one process and the CLI reads it in another. The zero value stays out
// of the file entirely, so an instance with nothing to protect carries no key.
func TestUnmountProtectionRoundTripsThroughTheRegistry(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name    string
		state   Protection
		wantKey bool
	}{
		{"armed", ProtectionArmed, true},
		{"unavailable", ProtectionNoSession, true},
		{"nothing", ProtectionUnrecorded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Write(root, Entry{Name: tc.name, Kind: KindCartridge, UnmountProtection: tc.state}); err != nil {
				t.Fatalf("Write: %v", err)
			}
			back, err := Read(root, tc.name)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if back.UnmountProtection != tc.state {
				t.Errorf("UnmountProtection = %q, want %q", back.UnmountProtection, tc.state)
			}

			raw, err := os.ReadFile(filepath.Join(Dir(root), tc.name+".json"))
			if err != nil {
				t.Fatalf("read the record back: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatalf("decode the record: %v", err)
			}
			if _, ok := fields["unmountProtection"]; ok != tc.wantKey {
				t.Errorf("record has an unmountProtection key = %v, want %v:\n%s", ok, tc.wantKey, raw)
			}
		})
	}
}
