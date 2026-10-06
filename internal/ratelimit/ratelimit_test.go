package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestLimiterBurstThenWaits(t *testing.T) {
	l := New(120) // 2/s refill, burst of 120
	for i := 0; i < 120; i++ {
		if err := l.Wait(t.Context()); err != nil {
			t.Fatalf("burst %d: %v", i, err)
		}
	}
	start := time.Now()
	if err := l.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Errorf("121st call should wait ~500ms, waited %v", waited)
	}
}

func TestLimiterHonoursContextAndBudget(t *testing.T) {
	l := New(1)
	_ = l.Wait(t.Context()) // use the single token
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	l.maxWait = time.Hour
	if err := l.Wait(ctx); err == nil {
		t.Error("expected context error")
	}
	l.maxWait = time.Millisecond
	if err := l.Wait(t.Context()); err == nil {
		t.Error("expected budget error")
	}
}

func TestBackoff(t *testing.T) {
	start := time.Now()
	if err := Backoff(t.Context(), 0, 20*time.Millisecond); err != nil || time.Since(start) < 15*time.Millisecond {
		t.Errorf("retry-after backoff: %v %v", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Backoff(ctx, 5, 0); err == nil {
		t.Error("cancelled backoff should fail")
	}
}
