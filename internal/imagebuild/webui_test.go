package imagebuild

import (
	"os"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/util"
)

// uiSteps returns the steps that bake the Incus web UI.
func uiSteps(t *testing.T) []Step {
	t.Helper()
	var out []Step
	for _, s := range DefaultRecipe(testVersion).Steps() {
		line := strings.Join(s.Argv, " ") + s.Path + s.Content
		if strings.Contains(line, "incus-ui") || strings.Contains(line, "zabbly") ||
			strings.Contains(line, "INCUS_UI") {
			out = append(out, s)
		}
	}
	return out
}

// The web UI is baked into the image so a fresh guest serves it without doing
// network work on first boot. The shell build does this on its nbd path; the
// recipe has to as well, or a Go-built image silently loses it — cloud-init
// re-installs it at boot, so nothing fails and nobody notices.
func TestRecipeBakesTheIncusWebUI(t *testing.T) {
	steps := uiSteps(t)
	if len(steps) == 0 {
		t.Fatal("the recipe bakes no Incus web UI")
	}

	joined := func() string {
		var b strings.Builder
		for _, s := range steps {
			b.WriteString(strings.Join(s.Argv, " "))
			b.WriteString(s.Path)
			b.WriteString(s.Content)
			b.WriteByte('\n')
		}
		return b.String()
	}()

	for _, want := range []string{
		"incus-ui-canonical", // the package the UI files come from
		"/opt/incus/ui",      // where incusd is told to serve them from
		"INCUS_UI",           // the drop-in that points it there
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the UI bake never mentions %q:\n%s", want, joined)
		}
	}
}

// Zabbly is a third party outside Debian main. A mirror outage there must not
// block a guest image release for a cosmetic component — and with
// fail-fast:false plus needs:build in the workflow, one architecture failing
// blocks both.
func TestTheWebUIBakeIsFailureTolerant(t *testing.T) {
	for _, s := range uiSteps(t) {
		if isUICleanup(s) {
			continue
		}
		if !s.Optional {
			t.Errorf("UI step %q is required; a Zabbly outage would fail the whole build", s.Desc)
		}
	}
}

// isUICleanup reports whether a step removes build-time Zabbly apt state.
func isUICleanup(s Step) bool {
	return strings.Contains(strings.Join(s.Argv, " "), "rm ")
}

// The Zabbly apt source must never survive into the image. If it does, a
// guest's routine `apt upgrade` pulls Zabbly's incus to satisfy its
// "Depends: incus" and swaps out Debian's under a running host, days later.
//
// The signing key must not survive either. The shell build removes the source
// but never the key, so every published image today carries an unadvertised
// third-party apt trust anchor. That omission is not ported.
func TestTheWebUIBakeLeavesNoZabblyStateBehind(t *testing.T) {
	steps := DefaultRecipe(testVersion).Steps()

	for _, target := range []string{
		"/etc/apt/sources.list.d/zabbly-incus-stable.sources",
		"/etc/apt/keyrings/zabbly.asc",
	} {
		removed := false
		for _, s := range steps {
			if s.Kind != StepRun {
				continue
			}
			line := strings.Join(s.Argv, " ")
			if strings.Contains(line, "rm ") && strings.Contains(line, target) {
				removed = true
				if s.Optional {
					t.Errorf("removal of %s is optional; it must always run", target)
				}
			}
		}
		if !removed {
			t.Errorf("nothing removes %s, so it ships inside the image", target)
		}
	}
}

// Cleanup has to run even when the UI bake did not happen, so it must come
// after every optional UI step in the sequence.
func TestZabblyCleanupRunsAfterTheUIBake(t *testing.T) {
	steps := DefaultRecipe(testVersion).Steps()

	lastOptional, firstCleanup := -1, -1
	for i, s := range steps {
		if s.Optional {
			lastOptional = i
		}
		line := strings.Join(s.Argv, " ")
		if firstCleanup < 0 && strings.Contains(line, "zabbly") && strings.Contains(line, "rm ") {
			firstCleanup = i
		}
	}

	if firstCleanup < 0 {
		t.Fatal("no Zabbly cleanup step found")
	}
	if lastOptional >= 0 && firstCleanup < lastOptional {
		t.Errorf("Zabbly cleanup at step %d runs before the last optional UI step at %d",
			firstCleanup, lastOptional)
	}
}

// The drop-in must not outlive the files it points at.
//
// Apply continues past an optional failure, so a Zabbly outage skips the
// extract and every later optional step still runs. If the drop-in is one of
// them, the image ships incusd configured to serve a UI directory that does
// not exist. The shell build could not reach that state — it wrote the drop-in
// inside the branch that ran only when the .deb was fetched.
func TestNoWebUIDropInWithoutTheWebUI(t *testing.T) {
	root := t.TempDir()
	// dpkg-deb appears only in the step that unpacks the UI payload.
	runner := &recordingRunner{failOn: "dpkg-deb"}

	if _, err := Apply(t.Context(), root, uiSteps(t), runner); err != nil {
		t.Fatalf("an optional failure aborted the build: %v", err)
	}

	dropIn, err := util.SafeJoin(root, uiDropInPath)
	if err != nil {
		t.Fatalf("resolve %s: %v", uiDropInPath, err)
	}
	if _, err := os.Stat(dropIn); err == nil {
		t.Errorf("the extract failed but %s was written; incusd would point at a missing %s",
			uiDropInPath, uiRoot)
	}
}

// A skipped optional step must be reported, not swallowed. An image quietly
// missing its web UI is exactly the kind of difference this build is supposed
// to surface rather than hide.
func TestApplyReportsSkippedOptionalSteps(t *testing.T) {
	runner := &recordingRunner{failOn: "flaky"}
	skipped, err := Apply(t.Context(), t.TempDir(), []Step{
		{Kind: StepRun, Desc: "required", Argv: []string{"ok"}},
		{Kind: StepRun, Desc: "best effort", Argv: []string{"flaky"}, Optional: true},
		{Kind: StepRun, Desc: "after", Argv: []string{"later"}},
	}, runner)

	if err != nil {
		t.Fatalf("an optional step's failure aborted the build: %v", err)
	}
	if len(skipped) != 1 {
		t.Fatalf("reported %d skipped steps, want 1", len(skipped))
	}
	if skipped[0].Step.Desc != "best effort" {
		t.Errorf("skipped step is %q, want %q", skipped[0].Step.Desc, "best effort")
	}
	if skipped[0].Err == nil {
		t.Error("the skipped step carries no reason")
	}
	if len(runner.commands) != 3 {
		t.Errorf("ran %d commands, want 3 — the build should continue past an optional failure", len(runner.commands))
	}
}

// A required step failing must still stop the build.
func TestApplyStillStopsOnARequiredFailure(t *testing.T) {
	runner := &recordingRunner{failOn: "boom"}
	_, err := Apply(t.Context(), t.TempDir(), []Step{
		{Kind: StepRun, Desc: "the failing one", Argv: []string{"boom"}},
		{Kind: StepRun, Desc: "never reached", Argv: []string{"after"}},
	}, runner)

	if err == nil {
		t.Fatal("a required step's failure did not stop the build")
	}
	if len(runner.commands) != 1 {
		t.Errorf("ran %d commands, want 1", len(runner.commands))
	}
}
