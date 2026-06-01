//go:build !darwin

package vm

import (
	"context"
	"errors"

	"github.com/stuffbucket/bladerunner/internal/agent"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/report"
)

type Runner struct{}

type StartVMResult struct {
	Endpoint string
}

func NewRunner(*config.Config) (*Runner, error) {
	return nil, errors.New("bladerunner requires macOS (darwin)")
}

func (r *Runner) Start(context.Context) (*report.StartupReport, error) {
	return nil, errors.New("unsupported platform")
}

func (r *Runner) StartVM(context.Context) (*StartVMResult, error) {
	return nil, errors.New("unsupported platform")
}

func (r *Runner) WaitForIncus(context.Context) (*report.StartupReport, error) {
	return nil, errors.New("unsupported platform")
}

func (r *Runner) StartGUI() error                  { return errors.New("unsupported platform") }
func (r *Runner) Wait(context.Context) error       { return errors.New("unsupported platform") }
func (r *Runner) Stop() error                      { return nil }
func (r *Runner) SetProgress(Progress)             {}
func (r *Runner) ProbeGuest(context.Context) error { return errors.New("unsupported platform") }
func (r *Runner) NestedVirtState() string          { return "unsupported" }

// NestedVirtualizationSupported is always false off darwin.
func NestedVirtualizationSupported() bool { return false }

func (r *Runner) RunAgentHandshake(context.Context) (*agent.HandshakeResult, error) {
	return nil, errors.New("unsupported platform")
}
