package ssh

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// Every test in this file uses the same two-phase rendezvous instead of a sleep:
// each goroutine reports itself scheduled on `ready` and then parks on `start`,
// and `start` is not closed until every goroutine has reported. Nothing here
// assumes a duration, and every assertion is an invariant that must hold on
// every interleaving, so the tests are meaningful under -race with any
// scheduler.

// runConcurrently starts one goroutine per job, releases them all at once, and
// returns when every one has finished.
func runConcurrently(jobs []func() error) []error {
	n := len(jobs)
	ready := make(chan struct{}, n)
	start := make(chan struct{})
	errs := make([]error, n)

	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			errs[i] = job()
		}()
	}
	for range n {
		<-ready
	}
	close(start)
	wg.Wait()
	return errs
}

// aggregatorWatcher samples the aggregator file as fast as it can while writers
// work, and records every observed state that check rejects.
//
// This is the part that makes these tests regression tests rather than lottery
// tickets. Asserting only on the FINAL file is unreliable, because a full
// rewrite by the default instance heals a duplicated Include that a named
// instance appended a moment earlier: the damage is real but transient, and
// whether it survives to the end of the test depends on which writer happens to
// go last. The invariants below must hold of EVERY state a reader can observe,
// and a real reader - the user's ssh, or a concurrent `br shell` - is entitled
// to look at any of them.
type aggregatorWatcher struct {
	mu         sync.Mutex
	violations []string
	done       chan struct{}
	wg         sync.WaitGroup
}

// watchAggregator starts sampling. check returns "" for an acceptable state or a
// description of what was wrong.
func watchAggregator(check func([]byte) string) *aggregatorWatcher {
	w := &aggregatorWatcher{done: make(chan struct{})}
	path := ConfigPath()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			select {
			case <-w.done:
				return
			default:
			}
			bad := ""
			data, err := os.ReadFile(path)
			if err != nil {
				bad = "read failed: " + err.Error()
			} else {
				bad = check(data)
			}
			if bad != "" {
				w.mu.Lock()
				w.violations = append(w.violations, bad)
				w.mu.Unlock()
			}
		}
	}()
	return w
}

// stop ends the watch and reports what it saw.
func (w *aggregatorWatcher) stop() []string {
	close(w.done)
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.violations
}

// report fails the test if the watcher saw anything, naming the first few.
func (w *aggregatorWatcher) report(t *testing.T, what string) {
	t.Helper()
	if bad := w.stop(); len(bad) > 0 {
		t.Errorf("a reader saw %s %d time(s); first few: %v", what, len(bad), bad[:min(len(bad), 5)])
	}
}

// seedLegacyAggregator writes the aggregator an older bladerunner left behind:
// one Host block and no Include. It is the state that makes every concurrent
// named-instance writer decide, at the same moment, that the Include is absent.
func seedLegacyAggregator(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(Dir(), dirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(ConfigPath(), []byte(legacyAggregator), filePerm); err != nil {
		t.Fatalf("seed legacy config: %v", err)
	}
}

// legacyAggregator is the pre-config.d aggregator: a bare default-instance block.
const legacyAggregator = "Host bladerunner\n    Port 6022\n"

// assertSingleInclude holds the invariant ssh actually cares about: the
// aggregator names config.d exactly once. Two copies are not cosmetic - the
// second one re-reads every fragment, and ssh takes the first value it obtains
// for a keyword, so a duplicated Include silently changes which instance wins an
// alias.
func assertSingleInclude(t *testing.T) string {
	t.Helper()
	agg := readFile(t, ConfigPath())
	if n := strings.Count(agg, includeLine()); n != 1 {
		t.Errorf("aggregator holds %d copies of %q, want exactly 1:\n%s", n, includeLine(), agg)
	}
	return agg
}

// atMostOneInclude is the watcher check for the Include-count invariant.
func atMostOneInclude(data []byte) string {
	if n := strings.Count(string(data), includeLine()); n > 1 {
		return fmt.Sprintf("%d Include directives", n)
	}
	return ""
}

// TestAggregatorIncludeSurvivesConcurrentNamedWriters is the regression test for
// the unlocked read/check/append in ensureAggregatorInclude. Every named
// instance read the aggregator, saw no Include, and appended one; nothing
// serialized the three steps, so concurrent first starts appended one Include
// each.
//
// The race is run in rounds, each starting from a freshly seeded legacy
// aggregator, because the window is only open on the FIRST start after an
// upgrade: once any writer has appended the Include, every later writer takes
// the early return. One round is a coin toss - the work WriteInstanceSSHConfig
// does before it reaches the aggregator (validate, mkdir, render the fragment)
// varies per goroutine and desynchronizes writers that were released together.
// Rounds turn that into a near-certainty without a single timing assumption:
// each round is fully quiesced before the next is seeded.
func TestAggregatorIncludeSurvivesConcurrentNamedWriters(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const writers = 12
	const rounds = 40
	const basePort = 51100
	jobs := make([]func() error, writers)
	for i := range writers {
		jobs[i] = func() error {
			_, err := WriteInstanceSSHConfig(fmt.Sprintf("inst%d", i), basePort+i, "testuser", "/path/to/key")
			return err
		}
	}

	for round := range rounds {
		seedLegacyAggregator(t)
		for i, err := range runConcurrently(jobs) {
			if err != nil {
				t.Fatalf("round %d writer %d: WriteInstanceSSHConfig() error = %v", round, i, err)
			}
		}
		agg := readFile(t, ConfigPath())
		if n := strings.Count(agg, includeLine()); n != 1 {
			t.Fatalf("round %d: aggregator holds %d copies of %q, want exactly 1:\n%s",
				round, n, includeLine(), agg)
		}
		if !strings.HasPrefix(agg, legacyAggregator) {
			t.Fatalf("round %d: legacy default block must stay first and intact:\n%s", round, agg)
		}
	}

	// Every fragment survived, and the real ssh client can reach each one
	// through the single Include.
	for i := range writers {
		alias := HostAlias(fmt.Sprintf("inst%d", i))
		if got, want := sshConfigResolves(t, ConfigPath(), alias), fmt.Sprint(basePort+i); got != want {
			t.Errorf("ssh -F <aggregator> %s => port %s, want %s", alias, got, want)
		}
	}
}

// TestAggregatorSurvivesConcurrentDefaultAndNamedWriters covers the worse half
// of the same bug: WriteSSHConfig rewrote the aggregator with O_TRUNC while
// named instances were appending to it, so an append could land inside a file
// that was being rewritten under it, and the rewriter's next write would then
// overwrite the appended bytes. The result is an aggregator the real ssh client
// refuses outright.
func TestAggregatorSurvivesConcurrentDefaultAndNamedWriters(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	seedLegacyAggregator(t)

	const named = 8
	const defaults = 4
	const rounds = 12
	const basePort = 51200
	const defaultPort = 6022

	watcher := watchAggregator(atMostOneInclude)

	jobs := make([]func() error, 0, named+defaults)
	for i := range named {
		jobs = append(jobs, func() error {
			// Rewritten repeatedly, as a restarting instance does, so a named
			// append and a default truncate overlap many times, not once.
			for range rounds {
				if _, err := WriteInstanceSSHConfig(fmt.Sprintf("mix%d", i), basePort+i, "testuser", "/path/to/key"); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for range defaults {
		jobs = append(jobs, func() error {
			for range rounds {
				if _, err := WriteSSHConfig(defaultPort, "testuser", "/path/to/key"); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for i, err := range runConcurrently(jobs) {
		if err != nil {
			t.Fatalf("writer %d error = %v", i, err)
		}
	}
	watcher.report(t, "a duplicated Include")

	assertSingleInclude(t)
	if got := sshConfigResolves(t, ConfigPath(), hostAliasPrefix); got != fmt.Sprint(defaultPort) {
		t.Errorf("ssh -F <aggregator> %s => port %s, want %d", hostAliasPrefix, got, defaultPort)
	}
	for i := range named {
		alias := HostAlias(fmt.Sprintf("mix%d", i))
		if got, want := sshConfigResolves(t, ConfigPath(), alias), fmt.Sprint(basePort+i); got != want {
			t.Errorf("ssh -F <aggregator> %s => port %s, want %s", alias, got, want)
		}
	}
}

// TestAggregatorIsNeverVisiblyTruncated is the regression test for renderConfig
// itself, which opened the visible file O_TRUNC and executed the template
// straight into it. That leaves a real window in which the aggregator is empty
// or holds half a Host block, and anything reading it in that window - the
// user's ssh, or a concurrent `br shell` - reads garbage. A crash inside the
// window leaves the garbage behind permanently.
//
// The check is a property of every observed state, not a sample of a race: once
// the file is published by rename it holds by construction, so the test cannot
// be flaky in the passing direction.
func TestAggregatorIsNeverVisiblyTruncated(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Publish once so the file exists before the watch starts: from here on any
	// short read is the truncation window, not a missing file.
	if _, err := WriteSSHConfig(6022, "testuser", "/path/to/key"); err != nil {
		t.Fatalf("seed aggregator: %v", err)
	}
	tail := includeLine() + "\n"
	watcher := watchAggregator(func(data []byte) string {
		if strings.HasSuffix(string(data), tail) {
			return ""
		}
		return fmt.Sprintf("a %d-byte partial config", len(data))
	})

	const rewrites = 80
	const altPort = 6023
	for i := range rewrites {
		port := 6022
		if i%2 == 1 {
			port = altPort
		}
		if _, err := WriteSSHConfig(port, "testuser", "/path/to/key"); err != nil {
			watcher.stop()
			t.Fatalf("rewrite %d: %v", i, err)
		}
	}
	watcher.report(t, "the aggregator mid-write")
}

// TestLeftoverStagingFileDoesNotBreakInclude holds the claim WriteInstanceSSHConfig
// makes about a different component: that the staging file util.WriteFileAtomic
// creates inside config.d, which "Include config.d/*" does match, cannot break
// the ssh client even if a killed process leaves one behind for good.
//
// A comment asserting that would be worth nothing - it is a statement about what
// OpenSSH does with an unexpected file, so the real ssh client has to answer it.
// Both shapes a leftover can take are covered: zero bytes, which is what a
// process killed before the single write leaves, and a complete copy of the
// fragment, which is what one killed between the write and the rename leaves.
func TestLeftoverStagingFileDoesNotBreakInclude(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const port = 51300
	fragment, err := WriteInstanceSSHConfig("demo", port, "testuser", "/path/to/key")
	if err != nil {
		t.Fatalf("WriteInstanceSSHConfig() error = %v", err)
	}
	complete := []byte(readFile(t, fragment))

	for name, debris := range map[string][]byte{
		"empty":    {},
		"complete": complete,
	} {
		t.Run(name, func(t *testing.T) {
			// The name util.WriteFileAtomic would have used: the destination
			// base name, then its ".tmp-" pattern with a random suffix.
			leftover := fragment + ".tmp-123456789"
			if err := os.WriteFile(leftover, debris, filePerm); err != nil {
				t.Fatalf("seed leftover staging file: %v", err)
			}
			defer func() { _ = os.Remove(leftover) }()

			alias := HostAlias("demo")
			if got := sshConfigResolves(t, ConfigPath(), alias); got != fmt.Sprint(port) {
				t.Errorf("ssh -F <aggregator> %s => port %s, want %d", alias, got, port)
			}
			if got := sshConfigResolves(t, ConfigPath(), "example.invalid"); got != "22" {
				t.Errorf("a leftover staging file changed an unrelated host: port %s, want 22", got)
			}
		})
	}
}
