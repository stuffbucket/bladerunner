// Package converge is a platform-neutral, observe-first convergence controller
// for the bladerunner VM. It watches a small set of host-observable invariants
// (the VM host process is up; the guest answers a liveness probe) on an adaptive
// schedule — aggressive during critical periods (boot, wake, drift), invisible
// and cheap once steady — and, only after repeated host-observed failure,
// escalates to the existing debounced notifier.
//
// It deliberately does NOT self-heal the guest. The guest already heals itself:
// the vsock relays run Restart=always, the guest watchdog restarts wedged relays
// and steps the clock every 60s, and chrony owns the post-sleep clock. The host
// controller monitors, prefers to let guest-local heal work, and only escalates.
// Probes are INJECTED (see Invariant), so this package is pure and unit-testable
// with no cgo / VZ dependency.
package converge

import (
	"math/rand/v2"
	"time"
)

// Cadence constants for the adaptive schedule. Named (no magic numbers) so the
// rationale lives with the value.
const (
	// CriticalInterval is the fast cadence used during boot, wake, and observed
	// drift. It intentionally equals the existing 3s menubar poll period so the
	// controller reuses that one loop instead of adding a faster competing timer.
	CriticalInterval = 3 * time.Second
	// ConvergingInterval is the medium cadence used while the VM is coming up but
	// not yet sustained-healthy.
	ConvergingInterval = 7 * time.Second
	// SteadyInterval is the slow, cheap cadence used once the VM is sustained
	// healthy. It sits BELOW the guest watchdog's 60s heal cycle so the host
	// notices drift within a minute, yet never front-runs guest-local heal.
	SteadyInterval = 45 * time.Second
	// BackoffCap bounds the exponential backoff interval after repeated failed
	// host-observed reconvergence, so the controller never sleeps longer than
	// this even deep in backoff.
	BackoffCap = 5 * time.Minute
)

const (
	// JitterFraction is the +/- proportion of the base interval applied as
	// random jitter, so many hosts (or a restart storm) don't self-synchronize
	// their probes onto the same instant.
	JitterFraction = 0.1
	// MaxHostAttempts bounds how many times the host will attempt to reconverge
	// a single failing invariant before escalating and entering backoff. The
	// guest owns real recovery; this is a small ceiling on host-side nudging.
	MaxHostAttempts = 3
	// convergedTicksForSteady is how many consecutive fully-converged ticks are
	// required before promoting from Converging to Steady, so a single lucky
	// probe can't drop us straight into the slow cadence.
	convergedTicksForSteady = 2
	// backoffExpBase is the base of the exponential backoff (base^n).
	backoffExpBase = 2
)

// Tier is the current urgency of the convergence schedule. It selects both the
// probe cadence and (in Controller) whether heavy probes may run.
type Tier int

const (
	// Critical: aggressive cadence for boot / wake / observed drift.
	Critical Tier = iota
	// Converging: the VM is coming up but not yet sustained-healthy.
	Converging
	// Steady: sustained healthy; slow, cheap cadence. Heavy probes run here.
	Steady
	// Backoff: repeated host-observed failure; exponentially widening cadence.
	Backoff
)

// String implements fmt.Stringer for Tier.
func (t Tier) String() string {
	switch t {
	case Critical:
		return "critical"
	case Converging:
		return "converging"
	case Steady:
		return "steady"
	case Backoff:
		return "backoff"
	default:
		return "unknown"
	}
}

// Scheduler is the adaptive-cadence state machine. It is driven by Observe (one
// call per tick with the tick's convergence outcome) and queried via
// NextInterval for how long to wait before the next tick. It never sleeps itself
// — the caller owns the timer — and holds no clock, so it is deterministic and
// unit-testable by driving Observe directly.
type Scheduler struct {
	tier Tier
	// convergedStreak counts consecutive fully-converged ticks while Converging,
	// gating promotion to Steady.
	convergedStreak int
	// backoffN is the exponential backoff exponent (0 on first backoff entry).
	backoffN int
}

// NewScheduler returns a Scheduler starting in the Critical tier, matching the
// state right after a Start/wake where aggressive probing is wanted.
func NewScheduler() *Scheduler {
	return &Scheduler{tier: Critical}
}

// Tier returns the current tier (exposed for the Controller's heavy-probe gate
// and for tests).
func (s *Scheduler) Tier() Tier { return s.tier }

// NextInterval returns the current tier's base interval with +/- JitterFraction
// applied. In Backoff the base grows exponentially (base^n) capped at BackoffCap.
func (s *Scheduler) NextInterval() time.Duration {
	return jitter(s.baseInterval())
}

// baseInterval is the un-jittered interval for the current tier.
func (s *Scheduler) baseInterval() time.Duration {
	switch s.tier {
	case Critical:
		return CriticalInterval
	case Converging:
		return ConvergingInterval
	case Steady:
		return SteadyInterval
	case Backoff:
		return s.backoffInterval()
	default:
		return CriticalInterval
	}
}

// backoffInterval is SteadyInterval * base^backoffN, capped at BackoffCap.
func (s *Scheduler) backoffInterval() time.Duration {
	d := SteadyInterval
	for range s.backoffN {
		d *= backoffExpBase
		if d >= BackoffCap {
			return BackoffCap
		}
	}
	return min(d, BackoffCap)
}

// jitter applies +/- JitterFraction random jitter to d.
func jitter(d time.Duration) time.Duration {
	// rand.Float64() in [0,1) -> factor in [1-JitterFraction, 1+JitterFraction).
	// Timer de-synchronization jitter is non-cryptographic by design; a weak PRNG
	// is correct here.
	factor := 1 - JitterFraction + rand.Float64()*(2*JitterFraction) //nolint:gosec // jitter is not security-sensitive
	return time.Duration(float64(d) * factor)
}

// ForceCritical resets to the Critical tier and clears all backoff/streak state.
// Called on Start, wake, and observed drift — anything that should re-arm the
// aggressive cadence immediately.
func (s *Scheduler) ForceCritical() {
	s.tier = Critical
	s.convergedStreak = 0
	s.backoffN = 0
}

// Observe drives tier transitions from one tick's outcome:
//
//   - allConverged: every observed invariant held this tick.
//   - anyReconverging: the host is actively (and still within its attempt
//     budget) trying to reconverge a failing invariant this tick.
//
// Transitions:
//   - Fully converged in Critical -> Converging (start counting the streak).
//   - Fully converged in Converging for convergedTicksForSteady ticks -> Steady.
//   - Any non-convergence (in any tier) pulls us back toward Critical, unless the
//     host has exhausted its reconverge attempts (anyReconverging is false while
//     something is still failing), which escalates to Backoff with a widening
//     interval.
func (s *Scheduler) Observe(allConverged, anyReconverging bool) {
	if allConverged {
		s.observeConverged()
		return
	}
	s.observeNotConverged(anyReconverging)
}

// observeConverged handles a fully-converged tick: advance toward Steady and
// clear backoff.
func (s *Scheduler) observeConverged() {
	s.backoffN = 0
	switch s.tier {
	case Critical:
		s.tier = Converging
		s.convergedStreak = 1
	case Converging:
		s.convergedStreak++
		if s.convergedStreak >= convergedTicksForSteady {
			s.tier = Steady
		}
	case Steady:
		// Stay steady.
	case Backoff:
		// Recovered out of backoff; re-enter the aggressive path to confirm.
		s.tier = Converging
		s.convergedStreak = 1
	}
}

// observeNotConverged handles a tick where some invariant did not hold. While
// the host is still reconverging (within budget) we pull back toward Critical;
// once it has exhausted attempts (anyReconverging false) we widen into Backoff.
func (s *Scheduler) observeNotConverged(anyReconverging bool) {
	s.convergedStreak = 0
	if anyReconverging {
		// Still trying, within budget: stay aggressive.
		s.tier = Critical
		return
	}
	// Exhausted host attempts on a persistent failure: widen the cadence so we
	// stop hammering, but keep observing.
	if s.tier == Backoff {
		s.backoffN++
	} else {
		s.tier = Backoff
		s.backoffN = 0
	}
}
