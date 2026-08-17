package go2rtcmgr

import (
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// --- RingWatcher.HandleLogLine tests ---

func TestHandleLogLine_ValidRing(t *testing.T) {
	var mu sync.Mutex
	var got []RingEvent

	w := NewRingWatcher(zerolog.Nop())
	w.OnRing = func(ev RingEvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}

	line := `WYZE-NOTIFY {"stream":"front_doorbell","mac":"80482C2F8BDF","kind":"ring","ts":1786912182903}`
	consumed := w.HandleLogLine(line)

	if !consumed {
		t.Error("expected consumed=true for valid WYZE-NOTIFY line")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("OnRing called %d times, want 1", len(got))
	}
	if got[0].Stream != "front_doorbell" {
		t.Errorf("Stream = %q, want front_doorbell", got[0].Stream)
	}
	if got[0].MAC != "80482C2F8BDF" {
		t.Errorf("MAC = %q, want 80482C2F8BDF", got[0].MAC)
	}
	if got[0].TS.UnixMilli() != 1786912182903 {
		t.Errorf("TS = %v (ms=%d), want ms=1786912182903", got[0].TS, got[0].TS.UnixMilli())
	}
}

func TestHandleLogLine_NormalLine_NotConsumed(t *testing.T) {
	called := false
	w := NewRingWatcher(zerolog.Nop())
	w.OnRing = func(ev RingEvent) { called = true }

	for _, line := range []string{
		"21:38:15.743 INF go2rtc platform=linux/amd64",
		"[OOO] ch=0x05 some noise",
		"",
		"some random log line",
	} {
		if w.HandleLogLine(line) {
			t.Errorf("line %q should not be consumed", line)
		}
	}
	if called {
		t.Error("OnRing should not be called for non-notify lines")
	}
}

func TestHandleLogLine_MalformedJSON(t *testing.T) {
	called := false
	w := NewRingWatcher(zerolog.Nop())
	w.OnRing = func(ev RingEvent) { called = true }

	consumed := w.HandleLogLine(`WYZE-NOTIFY {not json}`)
	if !consumed {
		t.Error("malformed JSON should still be consumed (prevent log bleed)")
	}
	if called {
		t.Error("OnRing must not be called for malformed JSON")
	}
}

func TestHandleLogLine_NonRingKind(t *testing.T) {
	called := false
	w := NewRingWatcher(zerolog.Nop())
	w.OnRing = func(ev RingEvent) { called = true }

	// A non-ring kind (e.g. motion) should be consumed but not invoke OnRing.
	consumed := w.HandleLogLine(`WYZE-NOTIFY {"stream":"cam","mac":"AABB","kind":"motion","ts":123}`)
	if !consumed {
		t.Error("non-ring kind should still be consumed")
	}
	if called {
		t.Error("OnRing must not be called for non-ring kind")
	}
}

func TestHandleLogLine_MissingTS_FallsBackToNow(t *testing.T) {
	var got []RingEvent
	w := NewRingWatcher(zerolog.Nop())
	w.OnRing = func(ev RingEvent) { got = append(got, ev) }

	before := time.Now()
	w.HandleLogLine(`WYZE-NOTIFY {"stream":"cam","mac":"AABB","kind":"ring"}`)
	after := time.Now()

	if len(got) != 1 {
		t.Fatalf("OnRing called %d times, want 1", len(got))
	}
	if got[0].TS.Before(before) || got[0].TS.After(after) {
		t.Errorf("TS %v not within [%v, %v]", got[0].TS, before, after)
	}
}

func TestHandleLogLine_NilOnRing_NoPanic(t *testing.T) {
	w := NewRingWatcher(zerolog.Nop())
	w.OnRing = nil

	// Must not panic.
	consumed := w.HandleLogLine(`WYZE-NOTIFY {"stream":"x","mac":"AABB","kind":"ring","ts":1}`)
	if !consumed {
		t.Error("expected consumed=true")
	}
}

// --- RingDeduper tests ---

func TestRingDeduper_FirstCallAlwaysDispatches(t *testing.T) {
	d := NewRingDeduper(10 * time.Second)
	if !d.ShouldDispatch("cam1", time.Now()) {
		t.Error("first call should always dispatch")
	}
}

func TestRingDeduper_SecondCallWithinWindowSuppressed(t *testing.T) {
	d := NewRingDeduper(10 * time.Second)
	now := time.Now()
	d.ShouldDispatch("cam1", now)
	if d.ShouldDispatch("cam1", now.Add(5*time.Second)) {
		t.Error("second call within window should be suppressed")
	}
}

func TestRingDeduper_SecondCallOutsideWindowDispatches(t *testing.T) {
	d := NewRingDeduper(10 * time.Second)
	now := time.Now()
	d.ShouldDispatch("cam1", now)
	if !d.ShouldDispatch("cam1", now.Add(11*time.Second)) {
		t.Error("second call outside window should dispatch")
	}
}

func TestRingDeduper_DifferentCamerasIndependent(t *testing.T) {
	d := NewRingDeduper(10 * time.Second)
	now := time.Now()
	d.ShouldDispatch("cam1", now)

	// cam2 has never been seen; should dispatch regardless of cam1's window.
	if !d.ShouldDispatch("cam2", now.Add(1*time.Second)) {
		t.Error("different camera should dispatch independently")
	}
}

func TestRingDeduper_ConcurrentCallsRaceFree(t *testing.T) {
	// Run with -race to verify no data races.
	d := NewRingDeduper(5 * time.Second)
	const goroutines = 20
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.ShouldDispatch("cam1", time.Now())
		}()
	}
	wg.Wait()
}

// --- emitLogLine integration with RingWatcher ---

func TestEmitLogLine_WyzeNotifyConsumedByWatcher(t *testing.T) {
	called := false
	m := NewManager("", "", 0, zerolog.Nop())
	w := NewRingWatcher(zerolog.Nop())
	w.OnRing = func(ev RingEvent) { called = true }
	m.SetRingWatcher(w)

	// A WYZE-NOTIFY line should be consumed by the watcher and NOT reach
	// the normal level-mapping path (which would log it as trace noise).
	m.emitLogLine(`WYZE-NOTIFY {"stream":"cam","mac":"AABB","kind":"ring","ts":1786912182903}`)
	if !called {
		t.Error("OnRing should have been called via emitLogLine")
	}
}

func TestEmitLogLine_WyzeNotifyWithNoWatcher(t *testing.T) {
	// Manager with no watcher — WYZE-NOTIFY lines should not panic and
	// should fall through to normal (trace) level mapping without issue.
	m := NewManager("", "", 0, zerolog.Nop())
	// No panic is the assertion.
	m.emitLogLine(`WYZE-NOTIFY {"stream":"cam","mac":"AABB","kind":"ring","ts":1}`)
}
