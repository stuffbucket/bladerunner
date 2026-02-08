# Project Goals

## Primary Objective
Run Incus VMs on macOS using Apple's Virtualization framework with full container environment support.

## Core Components
1. **VM Runner** - Virtualization.framework integration (darwin-only)
2. **Control Protocol** - IPC for VM lifecycle management
3. **Cloud-Init** - VM provisioning with user/network config
4. **Port Forwarding** - vsock-based SSH/API access
5. **Incus Integration** - Wait for server, client certificates

## Success Criteria
- [ ] `br start` provisions and boots a VM
- [ ] `br ssh` connects via forwarded port
- [ ] `br stop` gracefully shuts down
- [ ] Incus API accessible at localhost:18443
- [ ] Console logging captures boot output
- [ ] GUI mode displays framebuffer

## Constraints
- macOS-only (Virtualization.framework)
- Requires entitlements for virtualization
- Unix sockets for local control (108 char path limit)
- VM disk must be raw format (qcow2 converted)

## Quality Gates
- golangci-lint with 35+ linters (∂S aligned)
- Pre-commit hooks enforce checks
- Test coverage targets: 80%+ for core packages
