package converge

import (
	"testing"
	"time"
)

func TestTierString(t *testing.T) {
	cases := map[Tier]string{
		Critical:   "critical",
		Converging: "converging",
		Steady:     "steady",
		Backoff:    "backoff",
		Tier(99):   "unknown",
	}
	for tier, want := range cases {
		if got := tier.String(); got != want {
			t.Errorf("Tier(%d).String() = %q, want %q", int(tier), got, want)
		}
	}
}

func TestSchedulerStartsCritical(t *testing.T) {
	s := NewScheduler()
	if s.Tier() != Critical {
		t.Fatalf("new scheduler tier = %v, want Critical", s.Tier())
	}
}

// TestCriticalToSteady drives the happy path: Critical -> Converging on the
// first converged tick, then -> Steady only after convergedTicksForSteady
// consecutive converged ticks (so a single lucky probe can't reach Steady).
func TestCriticalToSteady(t *testing.T) {
	s := NewScheduler()

	s.Observe(true, false)
	if s.Tier() != Converging {
		t.Fatalf("after 1 converged tick tier = %v, want Converging", s.Tier())
	}

	// Need convergedTicksForSteady consecutive converged ticks in Converging.
	// The first converged tick above set the streak to 1; the promotion happens
	// once the streak reaches convergedTicksForSteady.
	for i := 1; i < convergedTicksForSteady; i++ {
		s.Observe(true, false)
	}
	if s.Tier() != Steady {
		t.Fatalf("after %d converged ticks tier = %v, want Steady", convergedTicksForSteady, s.Tier())
	}

	// Steady is sticky while converged.
	s.Observe(true, false)
	if s.Tier() != Steady {
		t.Fatalf("steady not sticky: tier = %v", s.Tier())
	}
}

// TestDriftPullsToCritical: any non-convergence while the host is still
// reconverging (within budget) snaps back to Critical from any tier.
func TestDriftPullsToCritical(t *testing.T) {
	s := NewScheduler()
	// Reach Steady.
	s.Observe(true, false)
	for i := 1; i < convergedTicksForSteady; i++ {
		s.Observe(true, false)
	}
	if s.Tier() != Steady {
		t.Fatalf("setup: tier = %v, want Steady", s.Tier())
	}

	// Drift detected, host still reconverging.
	s.Observe(false, true)
	if s.Tier() != Critical {
		t.Fatalf("after drift tier = %v, want Critical", s.Tier())
	}
}

// TestRepeatedFailureBackoff: once host attempts are exhausted (anyReconverging
// false while not converged), the scheduler enters Backoff and its interval
// grows exponentially, capped at BackoffCap.
func TestRepeatedFailureBackoff(t *testing.T) {
	s := NewScheduler()

	// First exhausted failure enters Backoff at exponent 0 (base interval).
	s.Observe(false, false)
	if s.Tier() != Backoff {
		t.Fatalf("after exhausted failure tier = %v, want Backoff", s.Tier())
	}
	first := s.baseInterval()
	if first != SteadyInterval {
		t.Fatalf("first backoff base = %v, want SteadyInterval %v", first, SteadyInterval)
	}

	// Each subsequent exhausted failure widens the interval (exponential) until
	// it saturates at BackoffCap.
	prev := first
	sawGrowth := false
	for range 10 {
		s.Observe(false, false)
		cur := s.baseInterval()
		if cur > prev {
			sawGrowth = true
		}
		if cur > BackoffCap {
			t.Fatalf("backoff interval %v exceeded cap %v", cur, BackoffCap)
		}
		prev = cur
	}
	if !sawGrowth {
		t.Fatal("expected the backoff interval to grow across ticks")
	}
	if prev != BackoffCap {
		t.Fatalf("deep backoff interval = %v, want saturated at cap %v", prev, BackoffCap)
	}
}

// TestBackoffRecovers: a converged tick out of Backoff returns to the
// aggressive Converging path (not straight to Steady) to confirm recovery.
func TestBackoffRecovers(t *testing.T) {
	s := NewScheduler()
	s.Observe(false, false) // -> Backoff
	if s.Tier() != Backoff {
		t.Fatalf("setup: tier = %v, want Backoff", s.Tier())
	}
	s.Observe(true, false)
	if s.Tier() != Converging {
		t.Fatalf("recovery tier = %v, want Converging", s.Tier())
	}
}

func TestForceCriticalResets(t *testing.T) {
	s := NewScheduler()
	// Push deep into backoff.
	for range 5 {
		s.Observe(false, false)
	}
	if s.Tier() != Backoff || s.backoffN == 0 {
		t.Fatalf("setup: tier = %v backoffN = %d, want deep Backoff", s.Tier(), s.backoffN)
	}

	s.ForceCritical()
	if s.Tier() != Critical {
		t.Fatalf("after ForceCritical tier = %v, want Critical", s.Tier())
	}
	if s.backoffN != 0 || s.convergedStreak != 0 {
		t.Fatalf("after ForceCritical backoffN=%d streak=%d, want 0/0", s.backoffN, s.convergedStreak)
	}
	if s.baseInterval() != CriticalInterval {
		t.Fatalf("after ForceCritical base = %v, want CriticalInterval", s.baseInterval())
	}
}

// TestBaseIntervalPerTier checks each tier maps to its documented base cadence.
func TestBaseIntervalPerTier(t *testing.T) {
	cases := []struct {
		tier Tier
		want time.Duration
	}{
		{Critical, CriticalInterval},
		{Converging, ConvergingInterval},
		{Steady, SteadyInterval},
	}
	for _, c := range cases {
		s := &Scheduler{tier: c.tier}
		if got := s.baseInterval(); got != c.want {
			t.Errorf("tier %v base = %v, want %v", c.tier, got, c.want)
		}
	}
}

// TestNextIntervalJitterBounds confirms jitter stays within +/- JitterFraction
// of the base and never returns a zero/negative interval.
func TestNextIntervalJitterBounds(t *testing.T) {
	s := NewScheduler() // Critical
	base := CriticalInterval
	lo := time.Duration(float64(base) * (1 - JitterFraction))
	hi := time.Duration(float64(base) * (1 + JitterFraction))
	for range 1000 {
		d := s.NextInterval()
		if d < lo || d > hi {
			t.Fatalf("jittered interval %v outside [%v, %v]", d, lo, hi)
		}
		if d <= 0 {
			t.Fatalf("jittered interval %v must be positive", d)
		}
	}
}

// TestSteadyStaysBelowGuestWatchdog guards the load-bearing rationale: the
// steady cadence must sit below the guest watchdog's 60s heal cycle so the host
// notices drift within a minute without front-running guest-local heal.
func TestSteadyStaysBelowGuestWatchdog(t *testing.T) {
	const guestWatchdogPeriod = 60 * time.Second
	if SteadyInterval >= guestWatchdogPeriod {
		t.Fatalf("SteadyInterval %v must be < guest watchdog period %v", SteadyInterval, guestWatchdogPeriod)
	}
}
