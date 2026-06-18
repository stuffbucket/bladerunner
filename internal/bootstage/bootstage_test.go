package bootstage

import (
	"testing"
	"time"
)

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1700000000, 0).UTC()
	if err := Write(dir, Incus, now); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, ok := Read(dir)
	if !ok {
		t.Fatal("Read: ok=false after Write")
	}
	if got.Stage != Incus {
		t.Errorf("Stage = %q, want %q", got.Stage, Incus)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, now)
	}
}

func TestReadMissing(t *testing.T) {
	if _, ok := Read(t.TempDir()); ok {
		t.Error("Read: ok=true for missing file")
	}
}

func TestClear(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Boot, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	Clear(dir)
	if _, ok := Read(dir); ok {
		t.Error("Read: ok=true after Clear")
	}
}

func TestRankMonotonic(t *testing.T) {
	order := []Stage{Boot, Setup, Connect, Incus, Ready}
	for i := 1; i < len(order); i++ {
		if Rank(order[i]) <= Rank(order[i-1]) {
			t.Errorf("Rank(%q)=%d not greater than Rank(%q)=%d", order[i], Rank(order[i]), order[i-1], Rank(order[i-1]))
		}
	}
	if Rank("nonsense") != -1 {
		t.Errorf("Rank(unknown) = %d, want -1", Rank("nonsense"))
	}
}

func TestMessageAllStages(t *testing.T) {
	for _, s := range []Stage{Boot, Setup, Connect, Incus, Ready, Failed} {
		if Message(s) == "" {
			t.Errorf("Message(%q) is empty", s)
		}
	}
	if Message("unknown") != "Starting…" {
		t.Errorf("Message(unknown) = %q, want fallback", Message("unknown"))
	}
}
