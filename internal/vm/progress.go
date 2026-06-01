package vm

import (
	"fmt"
	"sync"
	"time"

	"github.com/stuffbucket/bladerunner/internal/logging"
)

// Stage IDs reported by the runner. Consumers may switch on these to map
// runner events onto richer UIs (e.g. the buildx-style board).
const (
	StageVMBoot    = "vm-boot"
	StageIncusWait = "incus-wait"
)

// Progress receives lifecycle events for the long-running waits inside the
// runner. The default implementation drives a per-stage TimedProgress
// spinner so existing behavior is preserved; richer UIs can replace it via
// Runner.SetProgress.
//
// All methods must be safe for concurrent use. Implementations should treat
// unknown stage IDs as no-ops rather than errors so the runner can introduce
// new stages without breaking older UIs.
type Progress interface {
	Begin(stage, label string, budget time.Duration)
	Substatus(stage, msg string)
	Done(stage string)
	Fail(stage string, err error)
}

// noopProgress is used when a Runner has no progress reporter attached.
type noopProgress struct{}

func (noopProgress) Begin(string, string, time.Duration) {}
func (noopProgress) Substatus(string, string)            {}
func (noopProgress) Done(string)                         {}
func (noopProgress) Fail(string, error)                  {}

// timedProgress wraps logging.TimedProgress so it satisfies Progress with one
// spinner per active stage. This preserves the original `br start` UX when no
// richer renderer is attached.
type timedProgress struct {
	mu     sync.Mutex
	active map[string]*logging.TimedProgress
}

// NewTimedProgress returns the default Progress implementation backed by
// logging.TimedProgress spinners.
func NewTimedProgress() Progress {
	return &timedProgress{active: map[string]*logging.TimedProgress{}}
}

func (p *timedProgress) Begin(stage, label string, budget time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.active[stage]; ok {
		return
	}
	p.active[stage] = logging.NewTimedProgress(label, budget)
}

func (p *timedProgress) Substatus(stage, msg string) {
	p.mu.Lock()
	tp := p.active[stage]
	p.mu.Unlock()
	if tp != nil {
		tp.SetStatus(msg)
	}
}

func (p *timedProgress) Done(stage string) {
	p.mu.Lock()
	tp := p.active[stage]
	delete(p.active, stage)
	p.mu.Unlock()
	if tp != nil {
		tp.SetStatus("ready")
		tp.Finish()
	}
}

func (p *timedProgress) Fail(stage string, err error) {
	p.mu.Lock()
	tp := p.active[stage]
	delete(p.active, stage)
	p.mu.Unlock()
	if tp != nil {
		if err == nil {
			err = fmt.Errorf("stage %s failed", stage)
		}
		tp.Fail(err)
	}
}
