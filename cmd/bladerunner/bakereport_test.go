package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/imagebuild"
)

// The failure from #263, kept verbatim, because it is the one this report has
// to be able to show.
const (
	skippedDesc   = "extract incus-ui-canonical to /opt/incus/ui and point incusd at it"
	skippedReason = "exit status 100: E: Unable to locate package incus-ui-canonical"
)

// uiSkipped is what imagebuild.Bake hands `br disk bake` after a Zabbly outage.
func uiSkipped() []imagebuild.Skipped {
	return []imagebuild.Skipped{{
		Step: imagebuild.Step{Kind: imagebuild.StepRun, Desc: skippedDesc, Optional: true},
		Err:  errors.New(skippedReason),
	}}
}

// TestBakeReportNamesTheStepsItSkipped holds the reporting half of #265.
//
// A bake that skipped an optional step published an image missing a component.
// The release workflow bakes the guest image that is the default for every
// fresh install, so "the build was green" is not enough: the report has to say
// what is absent, in the summary line and in --json.
func TestBakeReportNamesTheStepsItSkipped(t *testing.T) {
	rep := bakeReport("ci-guest", "arm64", "/tmp/out/guest.qcow2", "134e61c1", uiSkipped())

	if len(rep.Skipped) != 1 {
		t.Fatalf("report carries %d skipped steps, want 1 — the bake result was dropped on the way to the caller", len(rep.Skipped))
	}
	if rep.Skipped[0].Step != skippedDesc {
		t.Errorf("skipped step = %q, want %q", rep.Skipped[0].Step, skippedDesc)
	}
	if rep.Skipped[0].Reason != skippedReason {
		t.Errorf("skipped reason = %q, want %q — an error value marshals to {} if it is not rendered here",
			rep.Skipped[0].Reason, skippedReason)
	}

	var out bytes.Buffer
	printBakeResult(&out, rep)
	printed := out.String()
	if !strings.Contains(printed, "Baked ci-guest") {
		t.Errorf("the summary line is missing from:\n%s", printed)
	}
	for _, want := range []string{skippedDesc, skippedReason} {
		if !strings.Contains(printed, want) {
			t.Errorf("`br disk bake` printed a success line and nothing about %q:\n%s", want, printed)
		}
	}
}

// The JSON report is what the release workflow reads to annotate its run, so
// the skipped steps must survive a marshal/unmarshal round trip.
func TestDiskActionReportRoundTripsSkippedSteps(t *testing.T) {
	want := bakeReport("ci-guest", "arm64", "/tmp/out/guest.qcow2", "134e61c1", uiSkipped())

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal the bake report: %v", err)
	}
	var got diskActionReport
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal the bake report: %v", err)
	}

	if len(got.Skipped) != len(want.Skipped) {
		t.Fatalf("read back %d skipped steps from %s, want %d", len(got.Skipped), encoded, len(want.Skipped))
	}
	for i := range want.Skipped {
		if got.Skipped[i] != want.Skipped[i] {
			t.Errorf("skipped[%d] = %+v, want %+v", i, got.Skipped[i], want.Skipped[i])
		}
	}
	if got.Status != want.Status || got.Name != want.Name || got.Arch != want.Arch ||
		got.Output != want.Output || got.SHA256 != want.SHA256 {
		t.Errorf("report round-tripped as %+v, want %+v", got, want)
	}
}

// A clean bake must not grow a "skipped" key, so a consumer can treat its
// presence as the signal that something is missing from the image.
func TestBakeReportOmitsSkippedWhenNothingWasSkipped(t *testing.T) {
	rep := bakeReport("ci-guest", "arm64", "/tmp/out/guest.qcow2", "134e61c1", nil)

	encoded, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal the bake report: %v", err)
	}
	if strings.Contains(string(encoded), "skipped") {
		t.Errorf("a clean bake emitted a skipped key: %s", encoded)
	}

	var out bytes.Buffer
	printBakeResult(&out, rep)
	if strings.Contains(out.String(), "Skipped") {
		t.Errorf("a clean bake printed a skipped line:\n%s", out.String())
	}
}
