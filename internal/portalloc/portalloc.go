// Package portalloc reserves host loopback TCP ports for a bladerunner
// instance.
//
// Historically every host-side port was a compile-time constant (SSH 6022,
// Incus API 18443, web UI 18444, OIDC 15556, SNTP 15557), which caps the host
// at a single running instance: the second one dies binding 127.0.0.1:6022.
// This package hands out those ports at runtime instead, preferring the
// well-known value when it is free and falling back to an ephemeral port when
// it is not.
//
// Reservations own a bound net.Listener rather than a bare port number. That
// is deliberate: returning a number and re-binding it later leaves a window in
// which another process can take the port (a TOCTOU race). Callers that need
// to accept connections take ownership of the live listener via Detach.
package portalloc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"syscall"
)

const (
	// loopbackHost is the only interface reservations ever bind. Instance
	// ports are host-private; the guest reaches them over vsock forwarders.
	loopbackHost = "127.0.0.1"

	// ephemeralPort asks the kernel to pick a free port.
	ephemeralPort = 0

	// maxPort is the highest valid TCP port number.
	maxPort = 65535
)

// ErrInvalidPort reports a preferred port outside the valid TCP range.
var ErrInvalidPort = errors.New("portalloc: preferred port out of range")

// ErrEmptyName reports a reservation requested without a label.
var ErrEmptyName = errors.New("portalloc: reservation name must not be empty")

// ErrDuplicateName reports two specs in the same set sharing a name.
var ErrDuplicateName = errors.New("portalloc: duplicate reservation name")

// ErrUnknownName reports a lookup for a name the set does not hold.
var ErrUnknownName = errors.New("portalloc: unknown reservation name")

// ErrDetached reports a hand-off attempt for a listener that was already
// detached or closed.
var ErrDetached = errors.New("portalloc: reservation no longer holds a listener")

// Reservation is a host loopback port held open by a bound listener.
//
// A Reservation is safe for concurrent use. Exactly one of Close or Detach
// should ultimately be called: Close releases the port, Detach transfers the
// listener (and the duty to close it) to the caller.
type Reservation struct {
	name string
	port int

	mu     sync.Mutex
	ln     net.Listener // nil once closed or detached
	closed bool
}

// Reserve binds a loopback port for name.
//
// When preferred is non-zero it is tried first; if that port is already in use
// the reservation falls back to a kernel-assigned ephemeral port. A preferred
// value of 0 skips straight to an ephemeral port. Any bind failure other than
// "address already in use" is returned rather than papered over, so genuine
// misconfiguration (bad interface, sandbox denial) still surfaces.
func Reserve(name string, preferred int) (*Reservation, error) {
	if name == "" {
		return nil, ErrEmptyName
	}
	if preferred < ephemeralPort || preferred > maxPort {
		return nil, fmt.Errorf("%w: %q requested %d", ErrInvalidPort, name, preferred)
	}

	if preferred != ephemeralPort {
		ln, err := listenLoopback(preferred)
		switch {
		case err == nil:
			return newReservation(name, ln)
		case isAddrInUse(err):
			// Preferred port taken (most likely by another instance);
			// fall through to an ephemeral one.
		default:
			return nil, fmt.Errorf("portalloc: reserve %q on port %d: %w", name, preferred, err)
		}
	}

	ln, err := listenLoopback(ephemeralPort)
	if err != nil {
		return nil, fmt.Errorf("portalloc: reserve %q on an ephemeral port: %w", name, err)
	}
	return newReservation(name, ln)
}

// Name returns the label the reservation was created with.
func (r *Reservation) Name() string { return r.name }

// Port returns the bound loopback port. It stays valid after Close or Detach
// so callers can still report what was used.
func (r *Reservation) Port() int { return r.port }

// Listener returns the bound listener, or nil once the reservation has been
// closed or detached.
func (r *Reservation) Listener() net.Listener {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ln
}

// Detach transfers ownership of the bound listener to the caller. The
// reservation stops tracking it, so a later Close is a no-op and the caller
// becomes responsible for closing the listener. It returns ErrDetached if the
// listener was already handed off or closed.
func (r *Reservation) Detach() (net.Listener, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln == nil {
		return nil, fmt.Errorf("%w: %q (port %d)", ErrDetached, r.name, r.port)
	}
	ln := r.ln
	r.ln = nil
	return ln, nil
}

// Close releases the reserved port. It is safe to call more than once, and is
// a no-op after Detach.
func (r *Reservation) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.ln == nil {
		r.closed = true
		return nil
	}
	ln := r.ln
	r.ln = nil
	r.closed = true
	if err := ln.Close(); err != nil {
		return fmt.Errorf("portalloc: close %q (port %d): %w", r.name, r.port, err)
	}
	return nil
}

// Spec describes one port to reserve as part of a Set.
type Spec struct {
	// Name labels the reservation, e.g. "ssh" or "api".
	Name string
	// Preferred is the well-known port to try first; 0 means ephemeral.
	Preferred int
}

// ReserveSetError reports which member of a set failed to reserve, and which
// already-bound ports were rolled back as a result.
type ReserveSetError struct {
	// Name is the spec that failed.
	Name string
	// Err is the underlying reservation failure.
	Err error
	// ReleasedPorts lists ports that were bound before the failure and have
	// since been closed again.
	ReleasedPorts []int
	// RollbackErr aggregates any errors seen while releasing those ports.
	RollbackErr error
}

// Error implements error.
func (e *ReserveSetError) Error() string {
	msg := fmt.Sprintf("portalloc: reserving %q failed, released %v: %v", e.Name, e.ReleasedPorts, e.Err)
	if e.RollbackErr != nil {
		msg += fmt.Sprintf(" (rollback: %v)", e.RollbackErr)
	}
	return msg
}

// Unwrap exposes the underlying reservation failure to errors.Is/As.
func (e *ReserveSetError) Unwrap() error { return e.Err }

// Set is an all-or-nothing group of reservations addressed by name.
type Set struct {
	mu     sync.Mutex
	order  []string
	byName map[string]*Reservation
}

// ReserveSet reserves every spec or none of them. On the first failure all
// reservations already bound are closed and a *ReserveSetError naming the
// failed spec is returned.
func ReserveSet(specs ...Spec) (*Set, error) {
	set := &Set{
		order:  make([]string, 0, len(specs)),
		byName: make(map[string]*Reservation, len(specs)),
	}

	for _, spec := range specs {
		if _, dup := set.byName[spec.Name]; dup {
			return nil, set.rollback(spec.Name, fmt.Errorf("%w: %q", ErrDuplicateName, spec.Name))
		}
		res, err := Reserve(spec.Name, spec.Preferred)
		if err != nil {
			return nil, set.rollback(spec.Name, err)
		}
		set.order = append(set.order, spec.Name)
		set.byName[spec.Name] = res
	}
	return set, nil
}

// rollback closes everything reserved so far and builds the failure report.
func (s *Set) rollback(failed string, cause error) *ReserveSetError {
	released := make([]int, 0, len(s.order))
	var closeErrs []error
	for _, name := range s.order {
		res := s.byName[name]
		released = append(released, res.Port())
		if err := res.Close(); err != nil {
			closeErrs = append(closeErrs, err)
		}
	}
	s.order = nil
	s.byName = nil
	return &ReserveSetError{
		Name:          failed,
		Err:           cause,
		ReleasedPorts: released,
		RollbackErr:   errors.Join(closeErrs...),
	}
}

// Names returns the reservation names in the order they were requested.
func (s *Set) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

// Reservation returns the named reservation, or nil if the set has no such
// name.
func (s *Set) Reservation(name string) *Reservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byName[name]
}

// Port returns the bound port for name, or 0 if the set has no such name.
func (s *Set) Port(name string) int {
	res := s.Reservation(name)
	if res == nil {
		return 0
	}
	return res.Port()
}

// Listener returns the bound listener for name, or nil if the set has no such
// name or the listener was closed or detached.
func (s *Set) Listener(name string) net.Listener {
	res := s.Reservation(name)
	if res == nil {
		return nil
	}
	return res.Listener()
}

// Detach hands the named listener to the caller and drops it from the set, so
// a later Close leaves it running. The reserved port remains readable via the
// returned listener's address.
func (s *Set) Detach(name string) (net.Listener, error) {
	s.mu.Lock()
	res, ok := s.byName[name]
	if ok {
		delete(s.byName, name)
		s.order = removeName(s.order, name)
	}
	s.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownName, name)
	}
	return res.Detach()
}

// Close releases every reservation still held by the set, joining any errors.
// It is safe to call more than once.
func (s *Set) Close() error {
	s.mu.Lock()
	names := s.order
	byName := s.byName
	s.order = nil
	s.byName = nil
	s.mu.Unlock()

	errs := make([]error, 0, len(names))
	for _, name := range names {
		if err := byName[name].Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeName returns names without the first occurrence of target.
func removeName(names []string, target string) []string {
	for i, name := range names {
		if name == target {
			return append(names[:i], names[i+1:]...)
		}
	}
	return names
}

// listenLoopback binds port on the loopback interface. Binding a literal IP
// needs no name resolution, so the background context never blocks here.
func listenLoopback(port int) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(context.Background(), "tcp", net.JoinHostPort(loopbackHost, strconv.Itoa(port)))
}

// newReservation wraps a freshly bound listener, resolving its actual port.
func newReservation(name string, ln net.Listener) (*Reservation, error) {
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		closeErr := ln.Close()
		err := fmt.Errorf("portalloc: reserve %q: unexpected listener address %T", name, ln.Addr())
		if closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		return nil, err
	}
	return &Reservation{name: name, port: addr.Port, ln: ln}, nil
}

// isAddrInUse reports whether err is the kernel refusing a bind because the
// address is already taken.
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}
