package portalloc

import (
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"
)

// occupyPort binds a loopback port for the duration of the test and returns
// it. Anything that later tries to reserve that port must fall back.
func occupyPort(t *testing.T) int {
	t.Helper()
	ln, err := listenLoopback(ephemeralPort)
	if err != nil {
		t.Fatalf("occupyPort: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("occupyPort: unexpected address type %T", ln.Addr())
	}
	return addr.Port
}

// freePort binds and immediately releases a loopback port, yielding a number
// that is very likely free. Used to exercise the "preferred port available"
// path without hardcoding a port the machine may already be using.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := listenLoopback(ephemeralPort)
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		t.Fatalf("freePort: unexpected address type %T", ln.Addr())
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("freePort close: %v", err)
	}
	return addr.Port
}

// assertBindable fails the test if port cannot be bound, i.e. if a reservation
// leaked it.
func assertBindable(t *testing.T, port int) {
	t.Helper()
	ln, err := listenLoopback(port)
	if err != nil {
		t.Fatalf("port %d still held: %v", port, err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("closing probe listener on %d: %v", port, err)
	}
}

func TestReservePreferredWhenFree(t *testing.T) {
	want := freePort(t)

	res, err := Reserve("api", want)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	if got := res.Port(); got != want {
		t.Errorf("Port() = %d, want preferred %d", got, want)
	}
	if got := res.Name(); got != "api" {
		t.Errorf("Name() = %q, want %q", got, "api")
	}
	if res.Listener() == nil {
		t.Fatal("Listener() = nil, want a bound listener")
	}
	if addr := res.Listener().Addr().String(); addr != net.JoinHostPort(loopbackHost, strconv.Itoa(want)) {
		t.Errorf("listener addr = %q, want loopback:%d", addr, want)
	}
}

func TestReserveFallsBackWhenPreferredTaken(t *testing.T) {
	taken := occupyPort(t)

	res, err := Reserve("ssh", taken)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	switch {
	case res.Port() == ephemeralPort:
		t.Fatal("Port() = 0, want a concrete fallback port")
	case res.Port() == taken:
		t.Fatalf("Port() = %d, want a port other than the occupied one", taken)
	}
	if res.Listener() == nil {
		t.Fatal("Listener() = nil after fallback")
	}
}

func TestReserveEphemeral(t *testing.T) {
	res, err := Reserve("oidc", ephemeralPort)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	if res.Port() <= ephemeralPort || res.Port() > maxPort {
		t.Errorf("Port() = %d, want a valid ephemeral port", res.Port())
	}
}

func TestReserveInvalidArgs(t *testing.T) {
	tests := []struct {
		name      string
		label     string
		preferred int
		wantErr   error
	}{
		{name: "empty name", label: "", preferred: ephemeralPort, wantErr: ErrEmptyName},
		{name: "negative port", label: "ssh", preferred: -1, wantErr: ErrInvalidPort},
		{name: "port above range", label: "ssh", preferred: maxPort + 1, wantErr: ErrInvalidPort},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := Reserve(tt.label, tt.preferred)
			if res != nil {
				_ = res.Close()
				t.Fatalf("Reserve returned a reservation, want nil")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestReservationCloseIsIdempotent(t *testing.T) {
	res, err := Reserve("web", ephemeralPort)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	port := res.Port()

	if err := res.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if res.Listener() != nil {
		t.Error("Listener() non-nil after Close")
	}
	if res.Port() != port {
		t.Errorf("Port() = %d after Close, want it preserved as %d", res.Port(), port)
	}
	assertBindable(t, port)
}

func TestReservationDetachTransfersOwnership(t *testing.T) {
	res, err := Reserve("ntp", ephemeralPort)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	port := res.Port()

	ln, err := res.Detach()
	if err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if ln == nil {
		t.Fatal("Detach returned a nil listener")
	}
	if res.Listener() != nil {
		t.Error("Listener() non-nil after Detach")
	}

	// Close must not touch the detached listener: the port stays held.
	if err := res.Close(); err != nil {
		t.Fatalf("Close after Detach: %v", err)
	}
	if probe, probeErr := listenLoopback(port); probeErr == nil {
		_ = probe.Close()
		t.Fatalf("port %d was released by Close after Detach", port)
	}

	if _, err := res.Detach(); !errors.Is(err, ErrDetached) {
		t.Fatalf("second Detach err = %v, want %v", err, ErrDetached)
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("closing detached listener: %v", err)
	}
	assertBindable(t, port)
}

func TestReserveSetSucceeds(t *testing.T) {
	preferred := freePort(t)
	set, err := ReserveSet(
		Spec{Name: "ssh", Preferred: preferred},
		Spec{Name: "api", Preferred: ephemeralPort},
	)
	if err != nil {
		t.Fatalf("ReserveSet: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })

	if got := set.Port("ssh"); got != preferred {
		t.Errorf("Port(ssh) = %d, want %d", got, preferred)
	}
	if set.Port("api") <= ephemeralPort {
		t.Errorf("Port(api) = %d, want a bound port", set.Port("api"))
	}
	if set.Listener("ssh") == nil || set.Listener("api") == nil {
		t.Error("Listener returned nil for a reserved name")
	}
	if got := set.Port("nope"); got != 0 {
		t.Errorf("Port(nope) = %d, want 0", got)
	}
	if set.Listener("nope") != nil {
		t.Error("Listener(nope) non-nil, want nil")
	}
	if names := set.Names(); len(names) != 2 || names[0] != "ssh" || names[1] != "api" {
		t.Errorf("Names() = %v, want [ssh api]", names)
	}
}

func TestReserveSetRollsBackOnFailure(t *testing.T) {
	// The second "ssh" spec is rejected only after the first has bound, so
	// the failure path has something to roll back.
	set, err := ReserveSet(
		Spec{Name: "ssh", Preferred: ephemeralPort},
		Spec{Name: "api", Preferred: ephemeralPort},
		Spec{Name: "ssh", Preferred: ephemeralPort},
	)
	if set != nil {
		_ = set.Close()
		t.Fatal("ReserveSet returned a set alongside an error")
	}
	if !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("err = %v, want %v", err, ErrDuplicateName)
	}

	var setErr *ReserveSetError
	if !errors.As(err, &setErr) {
		t.Fatalf("err = %T, want *ReserveSetError", err)
	}
	if setErr.Name != "ssh" {
		t.Errorf("failed spec = %q, want %q", setErr.Name, "ssh")
	}
	if setErr.RollbackErr != nil {
		t.Errorf("RollbackErr = %v, want nil", setErr.RollbackErr)
	}
	if len(setErr.ReleasedPorts) != 2 {
		t.Fatalf("ReleasedPorts = %v, want 2 entries", setErr.ReleasedPorts)
	}
	for _, port := range setErr.ReleasedPorts {
		assertBindable(t, port)
	}
}

func TestReserveSetRollsBackOnInvalidSpec(t *testing.T) {
	set, err := ReserveSet(
		Spec{Name: "ssh", Preferred: ephemeralPort},
		Spec{Name: "api", Preferred: maxPort + 1},
	)
	if set != nil {
		_ = set.Close()
		t.Fatal("ReserveSet returned a set alongside an error")
	}
	if !errors.Is(err, ErrInvalidPort) {
		t.Fatalf("err = %v, want %v", err, ErrInvalidPort)
	}

	var setErr *ReserveSetError
	if !errors.As(err, &setErr) {
		t.Fatalf("err = %T, want *ReserveSetError", err)
	}
	if setErr.Name != "api" {
		t.Errorf("failed spec = %q, want %q", setErr.Name, "api")
	}
	if len(setErr.ReleasedPorts) != 1 {
		t.Fatalf("ReleasedPorts = %v, want 1 entry", setErr.ReleasedPorts)
	}
	assertBindable(t, setErr.ReleasedPorts[0])
	if msg := setErr.Error(); msg == "" {
		t.Error("Error() returned an empty message")
	}
}

func TestSetDetachAndClose(t *testing.T) {
	set, err := ReserveSet(
		Spec{Name: "ssh", Preferred: ephemeralPort},
		Spec{Name: "api", Preferred: ephemeralPort},
	)
	if err != nil {
		t.Fatalf("ReserveSet: %v", err)
	}
	sshPort := set.Port("ssh")
	apiPort := set.Port("api")

	ln, err := set.Detach("ssh")
	if err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if set.Port("ssh") != 0 {
		t.Errorf("Port(ssh) = %d after Detach, want 0", set.Port("ssh"))
	}
	if names := set.Names(); len(names) != 1 || names[0] != "api" {
		t.Errorf("Names() = %v, want [api]", names)
	}
	if _, err := set.Detach("ssh"); !errors.Is(err, ErrUnknownName) {
		t.Fatalf("second Detach err = %v, want %v", err, ErrUnknownName)
	}

	if err := set.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := set.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	assertBindable(t, apiPort)

	// The detached listener survived Close and is still ours to release.
	if probe, probeErr := listenLoopback(sshPort); probeErr == nil {
		_ = probe.Close()
		t.Fatalf("detached port %d was released by Set.Close", sshPort)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("closing detached listener: %v", err)
	}
	assertBindable(t, sshPort)
}

func TestConcurrentReserveYieldsDistinctPorts(t *testing.T) {
	const workers = 32

	var wg sync.WaitGroup
	ports := make([]int, workers)
	errs := make([]error, workers)
	reservations := make([]*Reservation, workers)

	wg.Add(workers)
	for i := range workers {
		go func(i int) {
			defer wg.Done()
			res, err := Reserve("worker", ephemeralPort)
			if err != nil {
				errs[i] = err
				return
			}
			reservations[i] = res
			ports[i] = res.Port()
		}(i)
	}
	wg.Wait()

	t.Cleanup(func() {
		for _, res := range reservations {
			if res != nil {
				_ = res.Close()
			}
		}
	})

	seen := make(map[int]int, workers)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if prev, dup := seen[ports[i]]; dup {
			t.Fatalf("workers %d and %d both got port %d", prev, i, ports[i])
		}
		seen[ports[i]] = i
	}
}
