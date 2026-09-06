package clock

import (
	"testing"
	"time"
)

func TestRealAdvances(t *testing.T) {
	var c Clock = Real{}
	t0 := c.Now()
	tk := c.NewTicker(5 * time.Millisecond)
	defer tk.Stop()
	select {
	case <-tk.C():
	case <-time.After(2 * time.Second):
		t.Fatal("real ticker never fired")
	}
	if !c.Now().After(t0) {
		t.Fatal("real clock did not advance")
	}
}

func TestVirtualOnlyMovesWhenAdvanced(t *testing.T) {
	start := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	v := NewVirtual(start)
	var c Clock = v

	if !c.Now().Equal(start) {
		t.Fatalf("Now() = %v, want %v", c.Now(), start)
	}

	tk := c.NewTicker(time.Second)
	defer tk.Stop()

	select {
	case <-tk.C():
		t.Fatal("virtual ticker fired without Advance")
	default:
	}

	v.Advance(time.Second)
	if !c.Now().Equal(start.Add(time.Second)) {
		t.Fatalf("Now() = %v after Advance", c.Now())
	}
	select {
	case at := <-tk.C():
		if !at.Equal(start.Add(time.Second)) {
			t.Fatalf("tick carried %v, want %v", at, start.Add(time.Second))
		}
	default:
		t.Fatal("virtual ticker did not fire after Advance")
	}
}

func TestVirtualAdvanceCoversMultiplePeriods(t *testing.T) {
	v := NewVirtual(time.Unix(0, 0).UTC())
	tk := v.NewTicker(100 * time.Millisecond)
	defer tk.Stop()

	// A single large Advance must not lose ticks silently beyond the one-slot
	// buffer that time.Ticker also uses; it must deliver at least one and leave
	// the clock at the right instant.
	v.Advance(350 * time.Millisecond)
	select {
	case <-tk.C():
	default:
		t.Fatal("no tick after a multi-period Advance")
	}
	if got := v.Now().Sub(time.Unix(0, 0).UTC()); got != 350*time.Millisecond {
		t.Fatalf("clock at %v, want 350ms", got)
	}
}

func TestVirtualStopSilencesTicker(t *testing.T) {
	v := NewVirtual(time.Unix(0, 0).UTC())
	tk := v.NewTicker(time.Second)
	tk.Stop()
	v.Advance(10 * time.Second)
	select {
	case <-tk.C():
		t.Fatal("stopped ticker still fired")
	default:
	}
}
