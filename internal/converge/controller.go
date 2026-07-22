package converge

import (
	"context"
	"sync/atomic"
	"time"
)

// probeTimeout bounds every individual probe/reconverge so one wedged call can
// never block the controller tick. Kept short: these are cheap socket pings.
const probeTimeout = 2 * time.Second

// Invariant is one host-observable property of a converged VM. Probe reports
// whether it currently holds; Reconverge (nil = observe-only) is the bounded
// host-side nudge attempted when it does not. Heavy marks a probe expensive
// enough that it runs only in the Steady tier and only when the cheaper
// invariants ahead of it passed.
//
// Ordering matters: invariants are evaluated in slice order and the controller
// short-circuits — a false lower invariant (e.g. "vm not running") skips every
// higher one (e.g. "incus authorized"), which would be meaningless.
type Invariant struct {
	// Name identifies the invariant in escalation reasons and results.
	Name string
	// Probe reports whether the invariant currently holds. Each call gets its own
	// short context; it must not block beyond that.
	Probe func(ctx context.Context) (ok bool, err error)
	// Heavy gates the probe to the Steady tier (and only after cheaper invariants
	// passed), so expensive checks never run during the hot boot/wake path.
	Heavy bool
	// Reconverge, when non-nil, is the bounded host-side attempt to fix a failing
	// invariant. Observe-only invariants (the MVP for vm-running / guest-reachable,
	// which the guest heals itself) leave this nil.
	Reconverge func(ctx context.Context) error
}

// Result is the per-invariant outcome of one Tick.
type Result struct {
	Name string
	// OK is the probe result (false if the probe errored or the invariant was
	// short-circuited without running).
	OK bool
	// Err is a probe or reconverge error, if any.
	Err error
	// Skipped is true when the invariant did not run this tick (short-circuited
	// by a lower false invariant, or Heavy-gated outside Steady).
	Skipped bool
	// Reconverged is true when a Reconverge attempt ran this tick.
	Reconverged bool
	// Escalated is true when this invariant exhausted its host attempts this tick
	// and onEscalate fired.
	Escalated bool
}

// Controller runs the invariant set on each Tick, short-circuiting on the first
// failure, gating heavy probes to Steady, attempting bounded reconvergence, and
// escalating once per invariant when host attempts are exhausted. It is
// single-flight: a Tick that arrives while a prior Tick is still running is
// skipped rather than overlapped.
type Controller struct {
	invariants []Invariant
	sched      *Scheduler
	// onEscalate is called at most once per invariant when it exhausts its host
	// attempt budget. Wired to the existing debounced notifier so rate-limit and
	// app-bundle gating are reused.
	onEscalate func(reason string)

	// attempts counts host reconverge attempts per invariant name.
	attempts map[string]int
	// escalated latches per invariant so onEscalate fires at most once until the
	// invariant recovers.
	escalated map[string]bool

	// running is the single-flight guard: a Tick already in progress skips a new
	// one rather than overlapping probes.
	running atomic.Bool

	// timeout is the per-probe context deadline (overridable in tests).
	timeout time.Duration
}

// NewController builds a Controller over the given invariants and scheduler.
// onEscalate may be nil (escalation becomes a no-op), which is handy in tests.
func NewController(invariants []Invariant, sched *Scheduler, onEscalate func(reason string)) *Controller {
	return &Controller{
		invariants: invariants,
		sched:      sched,
		onEscalate: onEscalate,
		attempts:   make(map[string]int),
		escalated:  make(map[string]bool),
		timeout:    probeTimeout,
	}
}

// Tick runs one pass over the invariants and returns a Result per invariant (in
// invariant order). It short-circuits on the first false invariant, runs Heavy
// invariants only in Steady after the cheap prechecks passed, attempts bounded
// reconvergence for failing invariants, and escalates once when an invariant
// exhausts its attempts. It does NOT drive the scheduler; the caller feeds the
// derived (allConverged, anyReconverging) outcome into Scheduler.Observe.
//
// Tick is single-flight: if a prior Tick is still running it returns nil
// immediately rather than overlapping.
func (c *Controller) Tick(ctx context.Context) []Result {
	if !c.running.CompareAndSwap(false, true) {
		return nil // a prior Tick is still in flight; skip this one.
	}
	defer c.running.Store(false)

	steady := c.sched.Tier() == Steady
	results := make([]Result, 0, len(c.invariants))
	// shortCircuit becomes true once a lower invariant fails: every higher
	// invariant is then meaningless and skipped.
	shortCircuit := false

	for i := range c.invariants {
		inv := &c.invariants[i]
		res := Result{Name: inv.Name}

		// A lower invariant already failed: skip the rest (their meaning depends
		// on the failed one holding).
		if shortCircuit {
			res.Skipped = true
			results = append(results, res)
			continue
		}
		// Heavy probes run only in Steady (and, by short-circuit, only when the
		// cheap prechecks ahead of them passed this tick).
		if inv.Heavy && !steady {
			res.Skipped = true
			results = append(results, res)
			continue
		}

		ok, err := c.probe(ctx, inv)
		res.OK = ok
		res.Err = err

		if ok {
			// Recovered: clear this invariant's attempt/escalation latch.
			c.attempts[inv.Name] = 0
			c.escalated[inv.Name] = false
			results = append(results, res)
			continue
		}

		// Failed: everything above this depends on it, so short-circuit.
		shortCircuit = true
		c.handleFailure(ctx, inv, &res)
		results = append(results, res)
	}
	return results
}

// probe runs one invariant's probe under a short per-probe timeout so a wedged
// call can never block the tick.
func (c *Controller) probe(ctx context.Context, inv *Invariant) (bool, error) {
	pctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return inv.Probe(pctx)
}

// handleFailure applies the bounded reconverge/escalate policy for a failing
// invariant, mutating res in place. Observe-only invariants (nil Reconverge)
// escalate directly on the first failure, since there is nothing for the host to
// try — the guest owns recovery and the notifier's own debounce absorbs flaps.
func (c *Controller) handleFailure(ctx context.Context, inv *Invariant, res *Result) {
	if inv.Reconverge == nil {
		// Observe-only: no host action to take. Escalate at most once.
		c.escalate(inv.Name, res)
		return
	}

	if c.attempts[inv.Name] < MaxHostAttempts {
		c.attempts[inv.Name]++
		res.Reconverged = true
		rctx, cancel := context.WithTimeout(ctx, c.timeout)
		if err := inv.Reconverge(rctx); err != nil && res.Err == nil {
			res.Err = err
		}
		cancel()
		return
	}
	// Attempts exhausted: escalate at most once.
	c.escalate(inv.Name, res)
}

// escalate fires onEscalate at most once per invariant (until it recovers) and
// latches res.Escalated.
func (c *Controller) escalate(name string, res *Result) {
	if c.escalated[name] {
		return
	}
	c.escalated[name] = true
	res.Escalated = true
	if c.onEscalate != nil {
		c.onEscalate(name)
	}
}

// Converged reports whether every non-skipped invariant held this tick — the
// allConverged input to Scheduler.Observe. Skipped (Heavy-gated) invariants
// don't count against convergence; a short-circuited (false-caused) skip does,
// because it only exists because a lower invariant failed.
func Converged(results []Result) bool {
	for _, r := range results {
		if r.Skipped {
			continue
		}
		if !r.OK {
			return false
		}
	}
	return true
}

// Reconverging reports whether the host actively attempted reconvergence this
// tick — the anyReconverging input to Scheduler.Observe.
func Reconverging(results []Result) bool {
	for _, r := range results {
		if r.Reconverged {
			return true
		}
	}
	return false
}
