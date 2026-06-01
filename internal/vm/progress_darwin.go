//go:build darwin

package vm

import "time"

// noopProgress is used when a Runner has no progress reporter attached. It
// lives in a darwin-only file because only the darwin Runner.SetProgress
// references it; on other platforms it would be flagged as unused.
type noopProgress struct{}

func (noopProgress) Begin(string, string, time.Duration) {}
func (noopProgress) Substatus(string, string)            {}
func (noopProgress) Done(string)                         {}
func (noopProgress) Fail(string, error)                  {}
