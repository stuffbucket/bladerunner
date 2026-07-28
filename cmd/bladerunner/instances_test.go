package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/instance"
)

// testScanner builds a scanner rooted at root whose liveness probe answers true
// only for the listed state dirs, so the resolution policy can be exercised
// without a control socket.
func testScanner(root string, running ...string) instanceScanner {
	live := make(map[string]bool, len(running))
	for _, dir := range running {
		live[filepath.Clean(dir)] = true
	}
	return instanceScanner{
		root:    root,
		running: func(dir string) bool { return live[filepath.Clean(dir)] },
		ports:   func(string) instance.Ports { return instance.Ports{SSH: 6022, API: 18443} },
	}
}

// register writes a registry entry under root.
func register(t *testing.T, root string, e instance.Entry) {
	t.Helper()
	if err := instance.Write(root, e); err != nil {
		t.Fatalf("write instance %q: %v", e.Name, err)
	}
}

// makeDiskSlot creates the legacy <root>/disks/<name> directory and returns it.
func makeDiskSlot(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, disksDirName, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create disk slot %q: %v", name, err)
	}
	return dir
}

func TestScannerResolvePolicy(t *testing.T) {
	tests := []struct {
		name string
		// setup registers entries / creates legacy dirs and returns the state
		// dirs that should answer as running.
		setup    func(t *testing.T, root string) []string
		selector string

		wantErr      bool
		wantErrParts []string
		wantName     string
		wantStateDir func(root string) string
		wantRunning  bool
		wantFallback bool
	}{
		{
			name: "nothing running resolves to the flat default",
			setup: func(_ *testing.T, _ string) []string {
				return nil
			},
			wantName:     config.DefaultInstanceName,
			wantStateDir: func(root string) string { return root },
			wantFallback: true,
		},
		{
			name: "a single running instance resolves implicitly",
			setup: func(t *testing.T, root string) []string {
				dir := filepath.Join(root, "demo")
				register(t, root, instance.Entry{Name: "demo", Kind: instance.KindCartridge, StateDir: dir})
				return []string{dir}
			},
			wantName:     "demo",
			wantStateDir: func(root string) string { return filepath.Join(root, "demo") },
			wantRunning:  true,
		},
		{
			name: "the single running instance may be the flat default",
			setup: func(_ *testing.T, root string) []string {
				return []string{root}
			},
			wantName:     config.DefaultInstanceName,
			wantStateDir: func(root string) string { return root },
			wantRunning:  true,
		},
		{
			name: "an explicit name wins over the single running instance",
			setup: func(t *testing.T, root string) []string {
				running := filepath.Join(root, "running")
				register(t, root, instance.Entry{Name: "running", Kind: instance.KindDisk, StateDir: running})
				register(t, root, instance.Entry{Name: "picked", Kind: instance.KindDisk, StateDir: filepath.Join(root, "picked")})
				return []string{running}
			},
			selector:     "picked",
			wantName:     "picked",
			wantStateDir: func(root string) string { return filepath.Join(root, "picked") },
			wantRunning:  false,
		},
		{
			name: "an explicit name resolves a stopped instance",
			setup: func(t *testing.T, root string) []string {
				register(t, root, instance.Entry{Name: "cold", Kind: instance.KindDisk, StateDir: filepath.Join(root, "cold")})
				return nil
			},
			selector:     "cold",
			wantName:     "cold",
			wantStateDir: func(root string) string { return filepath.Join(root, "cold") },
		},
		{
			name: `"default" selects the flat layout`,
			setup: func(_ *testing.T, root string) []string {
				return []string{root}
			},
			selector:     defaultSlotAlias,
			wantName:     config.DefaultInstanceName,
			wantStateDir: func(root string) string { return root },
			wantRunning:  true,
		},
		{
			name: "several running instances is an ambiguity error",
			setup: func(t *testing.T, root string) []string {
				a := filepath.Join(root, "alpha")
				b := filepath.Join(root, "beta")
				register(t, root, instance.Entry{Name: "alpha", Kind: instance.KindDisk, StateDir: a})
				register(t, root, instance.Entry{Name: "beta", Kind: instance.KindCartridge, StateDir: b})
				return []string{a, b}
			},
			wantErr:      true,
			wantErrParts: []string{"alpha", "beta", "--instance"},
		},
		{
			name: "an unknown name is an error listing what is running",
			setup: func(t *testing.T, root string) []string {
				dir := filepath.Join(root, "demo")
				register(t, root, instance.Entry{Name: "demo", Kind: instance.KindDisk, StateDir: dir})
				return []string{dir}
			},
			selector:     "nope",
			wantErr:      true,
			wantErrParts: []string{"nope", "demo"},
		},
		{
			name:         "a malformed name is rejected before any lookup",
			setup:        func(_ *testing.T, _ string) []string { return nil },
			selector:     "../escape",
			wantErr:      true,
			wantErrParts: []string{"escape", "invalid instance name"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			running := tt.setup(t, root)
			got, err := testScanner(root, running...).resolve(tt.selector)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolve(%q) = %+v, want error", tt.selector, got)
				}
				for _, part := range tt.wantErrParts {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("error %q does not mention %q", err, part)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve(%q): %v", tt.selector, err)
			}
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if want := tt.wantStateDir(root); got.StateDir != want {
				t.Errorf("StateDir = %q, want %q", got.StateDir, want)
			}
			if got.Running != tt.wantRunning {
				t.Errorf("Running = %v, want %v", got.Running, tt.wantRunning)
			}
			if got.Fallback != tt.wantFallback {
				t.Errorf("Fallback = %v, want %v", got.Fallback, tt.wantFallback)
			}
		})
	}
}

func TestAmbiguityErrorNamesEveryCandidate(t *testing.T) {
	root := t.TempDir()
	flat := root
	disk := makeDiskSlot(t, root, "builder")
	cart := filepath.Join(root, "cartridge-demo")
	register(t, root, instance.Entry{
		Name: "demo", Kind: instance.KindCartridge, StateDir: cart,
		Ports: instance.Ports{SSH: 51001, API: 51002},
	})

	_, err := testScanner(root, flat, disk, cart).resolve("")
	if err == nil {
		t.Fatal("resolve(\"\") with three running instances returned no error")
	}
	msg := err.Error()

	// Every candidate, its kind, and its ports have to be in the message: the
	// point of the error is that the user does not need a second command.
	for _, want := range []string{
		config.DefaultInstanceName, string(instance.KindFlat),
		"builder", string(instance.KindDisk),
		"demo", string(instance.KindCartridge),
		"ssh 51001", "api 51002", // from the registry entry
		"ssh 6022", "api 18443", // filled in for the legacy slots
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("ambiguity error is missing %q:\n%s", want, msg)
		}
	}
}

func TestResolveFallsBackToLegacyLayoutWithEmptyRegistry(t *testing.T) {
	// A VM started by an older binary has no registry entry. instance.List
	// reads the registry only, so the resolver has to scan the legacy layout
	// itself or a running VM becomes unaddressable after an upgrade.
	root := t.TempDir()
	slot := makeDiskSlot(t, root, "legacy")

	entries, err := instance.List(root)
	if err != nil {
		t.Fatalf("list registry: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("registry should be empty, got %d entries", len(entries))
	}

	scanner := testScanner(root, slot)

	implicit, err := scanner.resolve("")
	if err != nil {
		t.Fatalf("implicit resolve: %v", err)
	}
	if implicit.Name != "legacy" || implicit.StateDir != slot {
		t.Errorf("implicit resolve = %q at %q, want legacy at %q", implicit.Name, implicit.StateDir, slot)
	}
	if implicit.Kind != instance.KindDisk {
		t.Errorf("Kind = %q, want %q", implicit.Kind, instance.KindDisk)
	}

	named, err := scanner.resolve("legacy")
	if err != nil {
		t.Fatalf("named resolve: %v", err)
	}
	if named.StateDir != slot {
		t.Errorf("named resolve StateDir = %q, want %q", named.StateDir, slot)
	}
	if !named.Running {
		t.Error("named resolve Running = false, want true")
	}
}

func TestRunningInstancesDeduplicatesRegistryAndLegacy(t *testing.T) {
	// The flat default registers itself under config.DefaultInstanceName while
	// the legacy scan finds the very same directory; it must be listed once.
	root := t.TempDir()
	register(t, root, instance.Entry{
		Name: config.DefaultInstanceName, Kind: instance.KindFlat, StateDir: root, PID: os.Getpid(),
	})

	got := testScanner(root, root).runningInstances()
	if len(got) != 1 {
		t.Fatalf("runningInstances() = %d entries, want 1: %+v", len(got), got)
	}
	if got[0].PID != os.Getpid() {
		t.Errorf("PID = %d, want the registry value %d", got[0].PID, os.Getpid())
	}
}

func TestSelectedInstanceName(t *testing.T) {
	tests := []struct {
		name  string
		flag  string
		env   string
		alias string
		want  string
	}{
		{name: "unset"},
		{name: "flag", flag: "demo", want: "demo"},
		{name: "flag is trimmed", flag: "  demo\n", want: "demo"},
		{name: "environment", env: "fromenv", want: "fromenv"},
		{name: "short environment alias", alias: "fromalias", want: "fromalias"},
		{name: "flag beats the environment", flag: "demo", env: "fromenv", want: "demo"},
		{name: "long environment beats the alias", env: "fromenv", alias: "fromalias", want: "fromenv"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(instanceEnvVar, tt.env)
			t.Setenv(instanceEnvVarAlias, tt.alias)
			saved := instanceFlag
			instanceFlag = tt.flag
			t.Cleanup(func() { instanceFlag = saved })

			if got := selectedInstanceName(); got != tt.want {
				t.Errorf("selectedInstanceName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInstanceListingsJSONShape(t *testing.T) {
	root := t.TempDir()
	started := time.Now().Add(-90 * time.Second)
	cart := filepath.Join(root, "mnt", "demo")
	register(t, root, instance.Entry{
		Name:          "demo",
		Kind:          instance.KindCartridge,
		StateDir:      cart,
		SourcePath:    "/Volumes/ship/demo.dmg",
		Mountpoint:    cart,
		PID:           4242,
		Ports:         instance.Ports{SSH: 51001, API: 51002, Web: 51003, OIDC: 51004},
		BinaryVersion: "v1.2.3",
		StartedAt:     started,
	})

	scanner := testScanner(root, cart)
	listings := scanner.listings(scanner.runningInstances())
	if len(listings) != 1 {
		t.Fatalf("listings = %d, want 1", len(listings))
	}

	raw, err := json.Marshal(listings)
	if err != nil {
		t.Fatalf("marshal listings: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal listings: %v", err)
	}
	row := decoded[0]

	for k, want := range map[string]any{
		"name":           "demo",
		"kind":           string(instance.KindCartridge),
		"state_dir":      cart,
		"running":        true,
		"pid":            float64(4242),
		"source_path":    "/Volumes/ship/demo.dmg",
		"mountpoint":     cart,
		"binary_version": "v1.2.3",
	} {
		if row[k] != want {
			t.Errorf("json[%q] = %v, want %v", k, row[k], want)
		}
	}

	ports, ok := row["ports"].(map[string]any)
	if !ok {
		t.Fatalf("json[\"ports\"] = %T, want an object", row["ports"])
	}
	if ports["ssh"] != float64(51001) || ports["api"] != float64(51002) {
		t.Errorf("ports = %v, want the registry values", ports)
	}
	if row["uptime"] == "" || row["uptime"] == nil {
		t.Error("uptime is missing")
	}
	if row["started_at"] != started.Format(time.RFC3339) {
		t.Errorf("started_at = %v, want %v", row["started_at"], started.Format(time.RFC3339))
	}

	// An empty listing must marshal as [] rather than null so consumers can
	// range over it unconditionally.
	empty, err := json.Marshal(scanner.listings(nil))
	if err != nil {
		t.Fatalf("marshal empty listings: %v", err)
	}
	if string(empty) != "[]" {
		t.Errorf("empty listings marshaled as %s, want []", empty)
	}
}

func TestRenderInstanceListings(t *testing.T) {
	var buf bytes.Buffer
	if err := renderInstanceListings(&buf, nil); err != nil {
		t.Fatalf("render empty: %v", err)
	}
	// The empty listing must name the verbs that actually bring an instance
	// back, and how to discover what is bootable. Advising 'br start' here was
	// the same defect notRunningError fixed in vmgate.go: for a disk or a
	// cartridge 'br start' creates an ADDITIONAL flat VM instead of booting the
	// instance the user meant.
	empty := buf.String()
	for _, want := range []string{
		"No VM instances are running",
		"br up",
		"br boot <name>",
		"br disks",
	} {
		if !strings.Contains(empty, want) {
			t.Errorf("empty listing output = %q, want it to mention %q", empty, want)
		}
	}
	if strings.Contains(empty, "'br start'") {
		t.Errorf("empty listing output = %q, must not advise 'br start'", empty)
	}

	buf.Reset()
	err := renderInstanceListings(&buf, []instanceListing{{
		Name:       "demo",
		Kind:       string(instance.KindCartridge),
		StateDir:   "/Volumes/bladerunner-demo",
		Running:    true,
		PID:        4242,
		Ports:      instance.Ports{SSH: 51001, API: 51002},
		Uptime:     "1m30s",
		SourcePath: "/Users/x/Downloads/demo.dmg",
	}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "KIND", "SSH", "API", "UPTIME", "PID", "STATE DIR", "SOURCE",
		"demo", "cartridge", "51001", "51002", "1m30s", "4242", "/Volumes/bladerunner-demo", "demo.dmg"} {
		if !strings.Contains(out, want) {
			t.Errorf("table is missing %q:\n%s", want, out)
		}
	}
}

func TestResolvedInstanceSSHAlias(t *testing.T) {
	// The alias has to agree with what internal/ssh wrote for that state dir:
	// the bare alias for the flat default, a suffixed one for a named slot.
	root := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", root)

	flat := resolvedInstance{Name: config.DefaultInstanceName, StateDir: root}
	if !flat.isDefaultSlot() {
		t.Error("flat.isDefaultSlot() = false, want true")
	}
	if got := flat.instanceName(); got != config.DefaultInstanceName {
		t.Errorf("flat instanceName() = %q, want %q", got, config.DefaultInstanceName)
	}

	named := resolvedInstance{Name: "demo", StateDir: filepath.Join(root, disksDirName, "demo")}
	if named.isDefaultSlot() {
		t.Error("named.isDefaultSlot() = true, want false")
	}
	if got := named.instanceName(); got != "demo" {
		t.Errorf("named instanceName() = %q, want %q", got, "demo")
	}
}

func TestEjectSlotLabelKeepsTheLegacyDefaultName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", root)

	if got := ejectSlotLabel(resolvedInstance{Name: config.DefaultInstanceName, StateDir: root}); got != defaultSlotAlias {
		t.Errorf("ejectSlotLabel(flat) = %q, want %q", got, defaultSlotAlias)
	}
	if got := ejectSlotLabel(resolvedInstance{Name: "demo", StateDir: filepath.Join(root, "mnt", "demo")}); got != "demo" {
		t.Errorf("ejectSlotLabel(cartridge) = %q, want %q", got, "demo")
	}
}

// A cartridge is addressed by the name the user typed, not by the volume macOS
// mounted it as. The state dir of a booted cartridge is
// /Volumes/bladerunner-demo, so deriving the alias from its basename produced
// "bladerunner-demo" — an ssh alias and an --instance name nobody could guess.
func TestInstanceNamePrefersTheRegisteredName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", root)

	cart := resolvedInstance{
		Name:       "demo",
		Kind:       instance.KindCartridge,
		StateDir:   filepath.Join(cartridge.VolumesRoot, cartridge.VolumeName("demo")),
		Mountpoint: filepath.Join(cartridge.VolumesRoot, cartridge.VolumeName("demo")),
	}
	if got := cart.instanceName(); got != "demo" {
		t.Fatalf("instanceName() = %q, want demo (the registry name, not the mountpoint basename)", got)
	}
	// With no name at all — a legacy slot — the state dir still supplies one.
	legacy := resolvedInstance{StateDir: filepath.Join(root, disksDirName, "builder")}
	if got := legacy.instanceName(); got != "builder" {
		t.Errorf("legacy instanceName() = %q, want builder", got)
	}
}

// `br eject demo` and `--instance demo` must reach a cartridge booted with
// `br boot demo.dmg`, whose state dir is a /Volumes path bladerunner did not
// choose. The registry is what makes that possible; resolution must go through
// it before any path guess.
func TestEjectResolvesACartridgeByItsRegisteredName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BLADERUNNER_STATE_DIR", root)
	mount := filepath.Join(cartridge.VolumesRoot, cartridge.VolumeName("demo"))
	register(t, root, instance.Entry{
		Name:       "demo",
		Kind:       instance.KindCartridge,
		StateDir:   mount,
		Mountpoint: mount,
		SourcePath: "/Users/someone/Downloads/demo.dmg",
		PID:        os.Getpid(),
	})

	baseDir, slot, err := resolveEjectSlot("demo")
	if err != nil {
		t.Fatalf("resolveEjectSlot(demo): %v", err)
	}
	if slot != "demo" {
		t.Errorf("slot = %q, want demo", slot)
	}
	if baseDir != mount {
		t.Fatalf("baseDir = %q, want the cartridge's real mountpoint %q", baseDir, mount)
	}
}

// The unregistered fallback has to look where a browsable cartridge actually
// lands, not only at the private slot that browsable mounts never occupy.
func TestCartridgeMountCandidatesCoverBothMountPolicies(t *testing.T) {
	root := t.TempDir()
	got := cartridgeMountCandidates(root, "demo")

	want := map[string]bool{
		filepath.Join(root, "mnt", "demo"):                                 false,
		filepath.Join(cartridge.VolumesRoot, cartridge.VolumeName("demo")): false,
	}
	for _, mp := range got {
		if _, ok := want[mp]; !ok {
			t.Errorf("unexpected candidate %q", mp)
			continue
		}
		want[mp] = true
	}
	for mp, seen := range want {
		if !seen {
			t.Errorf("candidate %q is missing from %v", mp, got)
		}
	}
}

// registerRaw writes a registry record verbatim, so a test can present the
// exact JSON some other version of bladerunner would have left behind — one
// that predates a field, or one that records a value this build never writes.
func registerRaw(t *testing.T, root, name, body string) {
	t.Helper()
	dir := instance.Dir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create registry dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write raw entry %q: %v", name, err)
	}
}

// A cartridge whose holder could NOT arm the DiskArbitration unmount veto is
// running with no orderly spin-down on eject, and until now the only trace of
// that was a Warn line in the holder's log. `br instances --json` has to carry
// it: a stable code a script can branch on, and the sentence a person reads.
func TestInstanceListingsReportUnmountProtection(t *testing.T) {
	root := t.TempDir()
	cart := filepath.Join(root, "mnt", "demo")
	registerRaw(t, root, "demo", `{
  "name": "demo",
  "kind": "cartridge",
  "stateDir": "`+cart+`",
  "mountpoint": "`+cart+`",
  "unmountProtection": "diskarbitration-unavailable",
  "ports": {"ssh": 51001}
}`)

	scanner := testScanner(root, cart)
	listings := scanner.listings(scanner.runningInstances())
	if len(listings) != 1 {
		t.Fatalf("listings = %d, want 1", len(listings))
	}

	raw, err := json.Marshal(listings)
	if err != nil {
		t.Fatalf("marshal listings: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal listings: %v", err)
	}

	prot, ok := decoded[0]["unmount_protection"].(map[string]any)
	if !ok {
		t.Fatalf("json[\"unmount_protection\"] = %v, want an object", decoded[0]["unmount_protection"])
	}
	if prot["protected"] != false {
		t.Errorf("protected = %v, want false", prot["protected"])
	}
	if prot["code"] != "diskarbitration-unavailable" {
		t.Errorf("code = %v, want the stable code from the record", prot["code"])
	}
	reason, _ := prot["reason"].(string)
	if reason == "" || strings.Contains(reason, "Protection") {
		t.Errorf("reason = %q, want a human sentence rather than a constant name", reason)
	}
}

// A flat VM has no cartridge and therefore nothing to protect. Reporting an
// eject state for it would be noise in the table and, worse, a field a script
// could branch on that means nothing.
func TestInstanceListingsOmitProtectionForNonCartridges(t *testing.T) {
	root := t.TempDir()
	slot := makeDiskSlot(t, root, "builder")
	register(t, root, instance.Entry{Name: "builder", Kind: instance.KindDisk, StateDir: slot})

	scanner := testScanner(root, slot)
	listings := scanner.listings(scanner.runningInstances())
	if len(listings) != 1 {
		t.Fatalf("listings = %d, want 1", len(listings))
	}
	if listings[0].UnmountProtection != nil {
		t.Errorf("a %s instance reports %+v, want no eject state at all",
			listings[0].Kind, listings[0].UnmountProtection)
	}

	raw, err := json.Marshal(listings)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "unmount_protection") {
		t.Errorf("json carries an eject state for a non-cartridge:\n%s", raw)
	}

	var buf bytes.Buffer
	if err := renderInstanceListings(&buf, listings); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), "eject protection") {
		t.Errorf("table warns about a cartridge that does not exist:\n%s", buf.String())
	}
}

// The table is where a user finds out. A protected cartridge says so in one
// word and adds no note; an unprotected one says so AND says why, in language
// that names the consequence rather than the constant.
func TestRenderInstanceListingsReportsEjectProtection(t *testing.T) {
	listing := func(name string, p instance.Protection) instanceListing {
		return instanceListing{
			Name:              name,
			Kind:              string(instance.KindCartridge),
			StateDir:          "/Volumes/bladerunner-" + name,
			Running:           true,
			UnmountProtection: protectionReportFor(instance.KindCartridge, p),
		}
	}

	t.Run("protected is visible and quiet", func(t *testing.T) {
		var buf bytes.Buffer
		if err := renderInstanceListings(&buf, []instanceListing{listing("safe", instance.ProtectionArmed)}); err != nil {
			t.Fatalf("render: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "EJECT") || !strings.Contains(out, "protected") {
			t.Errorf("a protected cartridge is not visibly protected:\n%s", out)
		}
		if strings.Contains(out, "eject protection is") {
			t.Errorf("a protected cartridge should need no note:\n%s", out)
		}
	})

	t.Run("unprotected says so and says why", func(t *testing.T) {
		var buf bytes.Buffer
		err := renderInstanceListings(&buf, []instanceListing{listing("demo", instance.ProtectionNoSession)})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		out := buf.String()
		for _, want := range []string{
			"UNPROTECTED",
			"demo: eject protection is off",
			instance.ProtectionNoSession.Reason(),
			unprotectedConsequence,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("table is missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "Protection") {
			t.Errorf("table leaks a constant name:\n%s", out)
		}
	})

	t.Run("a host without DiskArbitration is stated, not blamed", func(t *testing.T) {
		var buf bytes.Buffer
		err := renderInstanceListings(&buf, []instanceListing{listing("linuxish", instance.ProtectionUnsupported)})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		out := buf.String()
		if strings.Contains(out, "UNPROTECTED") {
			t.Errorf("a platform with no DiskArbitration is shouted at as a defect:\n%s", out)
		}
		if !strings.Contains(out, "unavailable") || !strings.Contains(out, instance.ProtectionUnsupported.Reason()) {
			t.Errorf("the platform limit is not explained:\n%s", out)
		}
	})

	t.Run("an unrecorded state is unknown, not protected", func(t *testing.T) {
		var buf bytes.Buffer
		err := renderInstanceListings(&buf, []instanceListing{listing("legacy", instance.ProtectionUnrecorded)})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		out := buf.String()
		if strings.Contains(out, "protected\n") || strings.Contains(out, "  protected  ") {
			t.Errorf("a record with no state claims protection:\n%s", out)
		}
		for _, want := range []string{"unknown", "legacy: eject protection is unknown"} {
			if !strings.Contains(out, want) {
				t.Errorf("table is missing %q:\n%s", want, out)
			}
		}
	})
}
