//go:build darwin

package vm

import "testing"

func TestAssessGrubRisk(t *testing.T) {
	tests := []struct {
		name string
		in   GrubCheckInput
		want GrubRisk
	}{
		{
			name: "restoring short-circuits to safe even when unhardened+unclean",
			in:   GrubCheckInput{Restoring: true, DiskExists: true, GrubHardened: false, LastShutdownClean: false},
			want: GrubSafe,
		},
		{
			name: "hardened disk is safe even when unclean",
			in:   GrubCheckInput{GrubHardened: true, DiskExists: true, LastShutdownClean: false},
			want: GrubSafe,
		},
		{
			name: "no disk is safe (fresh boot, no recordfail)",
			in:   GrubCheckInput{DiskExists: false, GrubHardened: false, LastShutdownClean: false},
			want: GrubSafe,
		},
		{
			name: "unhardened + clean shutdown is at-risk",
			in:   GrubCheckInput{DiskExists: true, GrubHardened: false, LastShutdownClean: true},
			want: GrubAtRisk,
		},
		{
			name: "unhardened + unclean shutdown is known-wedged",
			in:   GrubCheckInput{DiskExists: true, GrubHardened: false, LastShutdownClean: false},
			want: GrubKnownWedged,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AssessGrubRisk(tc.in); got != tc.want {
				t.Errorf("AssessGrubRisk(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestGrubRiskString(t *testing.T) {
	cases := map[GrubRisk]string{
		GrubSafe:        "safe",
		GrubAtRisk:      "at-risk",
		GrubKnownWedged: "known-wedged",
	}
	for risk, want := range cases {
		if got := risk.String(); got != want {
			t.Errorf("GrubRisk(%d).String() = %q, want %q", risk, got, want)
		}
	}
}
