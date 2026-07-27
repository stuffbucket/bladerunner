package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The reporter is the only stateful part of `br watch`, and its two decisions —
// what to print, and whether to boot without asking — are what these cover.

func TestWatchReporterRendersEachVerdict(t *testing.T) {
	tests := []struct {
		name   string
		action watchAction
		want   []string
	}{
		{
			name: "offer names the cartridge and the file behind the mount",
			action: watchAction{
				Verdict: verdictOffer, Name: "demo",
				Mountpoint: "/Volumes/bladerunner-demo",
				SourcePath: "/Users/someone/Downloads/demo.dmg",
			},
			want: []string{"demo", "/Volumes/bladerunner-demo", "demo.dmg"},
		},
		{
			name: "warn carries the reason",
			action: watchAction{
				Verdict: verdictWarn, Name: "demo",
				Mountpoint: "/Volumes/bladerunner-demo",
				Reason:     "incomplete cartridge: missing root.img",
			},
			want: []string{"demo", "missing root.img"},
		},
		{
			name:   "booted reports the holder pid",
			action: watchAction{Verdict: verdictBooted, Name: "demo", PID: 4242},
			want:   []string{"demo", "4242"},
		},
		{
			name:   "failed reports why",
			action: watchAction{Verdict: verdictFailed, Name: "demo", Error: "unmount refused"},
			want:   []string{"demo", "unmount refused"},
		},
		{
			name:   "declined says nothing happened",
			action: watchAction{Verdict: verdictDeclined, Name: "demo"},
			want:   []string{"demo", "left alone"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := &watchReporter{out: &buf}
			r.emit(tt.action)
			got := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("output %q does not mention %q", got, want)
				}
			}
			if r.reported != 1 {
				t.Errorf("reported = %d, want 1", r.reported)
			}
		})
	}
}

// Under --json every event is a parseable object, so the stream stays usable
// while the watch is still running.
func TestWatchReporterEmitsJSON(t *testing.T) {
	var buf bytes.Buffer
	r := &watchReporter{json: true, out: &buf}
	r.emit(watchAction{
		Verdict: verdictOffer, Name: "demo",
		Mountpoint: "/Volumes/bladerunner-demo",
		SourcePath: "/Users/someone/Downloads/demo.dmg",
	})

	var got watchAction
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("emitted JSON does not parse: %v", err)
	}
	if got.Verdict != verdictOffer || got.Name != "demo" || got.SourcePath == "" {
		t.Fatalf("decoded = %+v, want the offer round-tripped", got)
	}
}

func TestWatchReporterAcceptPolicy(t *testing.T) {
	offer := watchAction{Verdict: verdictOffer, Name: "demo", SourcePath: "/tmp/demo.dmg"}
	tests := []struct {
		name       string
		reporter   watchReporter
		want       bool
		wantOutput string
	}{
		{name: "--yes boots without asking", reporter: watchReporter{auto: true}, want: true},
		{name: "--json never acts unasked", reporter: watchReporter{json: true}, want: false},
		{
			name:       "no terminal declines and says how to proceed",
			reporter:   watchReporter{},
			want:       false,
			wantOutput: "--yes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := tt.reporter
			r.out = &buf
			// go test's stdin is not a terminal, so the interactive branch is
			// deterministically the non-terminal one.
			if got := r.accept(offer); got != tt.want {
				t.Errorf("accept() = %v, want %v", got, tt.want)
			}
			if tt.wantOutput != "" && !strings.Contains(buf.String(), tt.wantOutput) {
				t.Errorf("output %q does not mention %q", buf.String(), tt.wantOutput)
			}
		})
	}
}

// A warn must never reach the boot path.
func TestWatchReporterDoesNotBootAWarning(t *testing.T) {
	var buf bytes.Buffer
	r := &watchReporter{auto: true, out: &buf}
	r.handle(watchAction{Verdict: verdictWarn, Name: "demo", Reason: "incomplete cartridge: missing root.img"})
	if r.reported != 1 {
		t.Fatalf("reported = %d, want 1 (the warning only)", r.reported)
	}
	if strings.Contains(buf.String(), "booting") {
		t.Fatalf("a warning was treated as bootable: %q", buf.String())
	}
}

func TestWatchReporterSummarizesAnEmptySweep(t *testing.T) {
	var buf bytes.Buffer
	r := &watchReporter{out: &buf}
	r.summarize()
	if !strings.Contains(buf.String(), "No bladerunner cartridges") {
		t.Fatalf("output = %q, want the empty-sweep line", buf.String())
	}

	buf.Reset()
	r = &watchReporter{out: &buf, reported: 1}
	r.summarize()
	if buf.Len() != 0 {
		t.Fatalf("output = %q, want nothing after a sweep that reported something", buf.String())
	}
}

// --auto is documented as an alias for --yes; both must land on one switch.
func TestWatchFlagAliases(t *testing.T) {
	for _, flag := range []string{"--yes", "--auto", "-y"} {
		t.Run(flag, func(t *testing.T) {
			watchFlags.auto = false
			t.Cleanup(func() {
				watchFlags.auto = false
				_ = watchCmd.Flags().Set("yes", "false")
				_ = watchCmd.Flags().Set("auto", "false")
			})
			if err := watchCmd.ParseFlags([]string{flag}); err != nil {
				t.Fatalf("ParseFlags(%q) error = %v", flag, err)
			}
			if !watchFlags.auto {
				t.Errorf("%s did not enable automatic boot", flag)
			}
		})
	}
}

// The verb has to be reachable and grouped, or it renders nowhere in `br --help`.
func TestWatchCommandIsRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Name() == "watch" {
			if c.GroupID != groupMedia {
				t.Errorf("watch GroupID = %q, want %q", c.GroupID, groupMedia)
			}
			return
		}
	}
	t.Fatal("watch is not registered on the root command")
}
