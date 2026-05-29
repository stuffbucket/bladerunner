// Package main implements br-agent, the bladerunner in-guest configuration
// agent. It dials the host over vsock (CID 2) and applies the configuration
// commands documented in github.com/stuffbucket/bladerunner/internal/agent.
//
// br-agent is intentionally tiny: it is built only for linux/amd64 and
// linux/arm64, has no external dependencies beyond golang.org/x/sys/unix,
// and is expected to ship inside the pre-baked guest image (see #45) or be
// installed during cloud-init.
package main
