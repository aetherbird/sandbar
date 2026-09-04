package tools

import (
	"context"
	"testing"
)

func TestTropicalLimiterCapsConcurrency(t *testing.T) {
	lim := NewTropicalLimiter(2)
	if err := lim.TryAcquire(); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := lim.TryAcquire(); err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if err := lim.TryAcquire(); err == nil {
		t.Fatal("third acquire past cap of 2 must fail")
	}
	lim.Release()
	if err := lim.TryAcquire(); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestTropicalLimiterUnlimited(t *testing.T) {
	lim := NewTropicalLimiter(-1)
	for i := 0; i < 100; i++ {
		if err := lim.TryAcquire(); err != nil {
			t.Fatalf("unlimited acquire %d: %v", i, err)
		}
	}
	var nilLim *TropicalLimiter
	if err := nilLim.TryAcquire(); err != nil {
		t.Fatalf("nil limiter must allow: %v", err)
	}
	nilLim.Release() // must not panic
}

func TestTropicalTotalCapsSpawns(t *testing.T) {
	tot := NewTropicalTotal(2)
	if err := tot.TryIncrement(); err != nil {
		t.Fatalf("first increment: %v", err)
	}
	if err := tot.TryIncrement(); err != nil {
		t.Fatalf("second increment: %v", err)
	}
	if err := tot.TryIncrement(); err == nil {
		t.Fatal("third increment past total of 2 must fail")
	}
}

func TestTropicalTotalUnlimited(t *testing.T) {
	tot := NewTropicalTotal(-1)
	for i := 0; i < 100; i++ {
		if err := tot.TryIncrement(); err != nil {
			t.Fatalf("unlimited increment %d: %v", i, err)
		}
	}
	var nilTot *TropicalTotal
	if err := nilTot.TryIncrement(); err != nil {
		t.Fatalf("nil total must allow: %v", err)
	}
}

func TestTropicalContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if TropicalFromContext(ctx) {
		t.Fatal("background context must not be tropical")
	}
	if TropicalLimiterFromContext(ctx) != nil {
		t.Fatal("background context must not carry a limiter")
	}
	if TropicalTotalFromContext(ctx) != nil {
		t.Fatal("background context must not carry a total")
	}
	ctx = WithTropical(ctx)
	if !TropicalFromContext(ctx) {
		t.Fatal("WithTropical must mark the context")
	}
}
