package reservedpool

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// helper enum to prove generics work with custom key types
type cat int

const (
	catA cat = iota
	catB
	catC
	catD
)

/* ------------------------------------------------------------------
   1. Constructor sanity
-------------------------------------------------------------------*/

func TestNewPanicsWhenReserveTooLarge(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic, got none")
		}
	}()
	_ = New(3, map[cat]int{catA: 2, catB: 2}) // 4 > 3
}

/* ------------------------------------------------------------------
   2. Basic acquire / release semantics
-------------------------------------------------------------------*/

func TestAcquireRelease(t *testing.T) {
	p := New(2, map[cat]int{catA: 1}) // reserve 1 for cat 0

	if err := p.Acquire(catA); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := p.Acquire(catB); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	done := make(chan struct{})
	go func() {
		p.Release(catA) // free 1 slot
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Millisecond):
		t.Fatal("release blocked")
	}

	// Now another acquire should succeed immediately
	if err := p.Acquire(catA); err != nil {
		t.Fatalf("expected acquire to succeed after release: %v", err)
	}
}

/* ------------------------------------------------------------------
   3. Reserve guarantee
-------------------------------------------------------------------*/

func TestReserveGuarantee(t *testing.T) {
	p := New(4, map[cat]int{catA: 2, catB: 1})

	// consume full reserve of catA
	for range 2 {
		if err := p.Acquire(catA); err != nil {
			t.Fatal(err)
		}
	}
	// consume full reserve of catB
	for range 2 {
		if err := p.Acquire(catB); err != nil {
			t.Fatal(err)
		}
	}

	// next acquire by catA must block until catB releases
	unblocked := make(chan struct{})
	go func() {
		_ = p.Acquire(catA) // should wait
		close(unblocked)
	}()

	select {
	case <-unblocked:
		t.Fatalf("catA acquired despite no flex slot")
	case <-time.After(5 * time.Millisecond):
	}

	p.Release(catB) // free one of B's slots

	select {
	case <-unblocked: // now catA should proceed
	case <-time.After(time.Second):
		t.Fatalf("catA did not acquire after B released")
	}
}

/* ------------------------------------------------------------------
   4. Global max never exceeded under heavy parallelism
-------------------------------------------------------------------*/

func TestNeverExceedsMax(t *testing.T) {
	const max = 6
	p := New(max, map[int]int{0: 2, 1: 2})
	active := atomic.Int32{}

	var wg sync.WaitGroup
	work := func(cat int) {
		defer wg.Done()
		if err := p.Acquire(cat); err != nil {
			t.Error(err)
			return
		}
		n := active.Add(1)
		if n > max {
			t.Errorf("active workers %d exceeds max %d", n, max)
		}
		time.Sleep(time.Millisecond) // simulate work
		active.Add(-1)
		p.Release(cat)
	}

	for i := range 128 {
		wg.Add(1)
		go work(i % 2)
	}
	wg.Wait()
}

/* ------------------------------------------------------------------
   5. Blocking and wake-up ordering
-------------------------------------------------------------------*/

func TestAcquireBlocksUntilRelease(t *testing.T) {
	p := New(1, map[int]int{0: 1})

	if err := p.Acquire(0); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	got := make(chan struct{})
	go func() {
		close(start)
		_ = p.Acquire(0) // will block
		close(got)
	}()

	<-start
	select {
	case <-got:
		t.Fatal("second acquire should block")
	case <-time.After(5 * time.Millisecond):
	}

	p.Release(0) // wake the waiter

	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("blocked acquire did not wake up")
	}
}

/* ------------------------------------------------------------------
   6. Close semantics
-------------------------------------------------------------------*/

func TestCloseUnblocksAndRejects(t *testing.T) {
	p := New(2, map[string]int{"x": 1})

	// consume both slots
	_ = p.Acquire("x")
	_ = p.Acquire("x")

	// waiter goroutine
	done := make(chan error)
	go func() {
		done <- p.Acquire("x") // will block until close
	}()

	time.Sleep(5 * time.Millisecond) // ensure it's waiting
	p.Close()                        // should unblock waiter

	err := <-done
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed, got %v", err)
	}

	// further Acquire should fail fast
	if err := p.Acquire("x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("acquire after close should return ErrClosed, got %v", err)
	}

	// Release after close should not panic
	p.Release("x")
}

/* ------------------------------------------------------------------
   7. Release with zero usage should panic
-------------------------------------------------------------------*/

func TestReleaseWithZeroUsagePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on zero usage release")
		}
	}()
	p := New(1, map[int]int{0: 1})
	p.Release(0)
}

/* ------------------------------------------------------------------
   8. Acquire should fail when capacity is fully reserved
-------------------------------------------------------------------*/

func TestAcquireWhenFullyReserved(t *testing.T) {
	p := New(4, map[cat]int{catA: 2, catB: 2, catC: 0})

	// consume full reserve of catA
	for range 2 {
		if err := p.Acquire(catA); err != nil {
			t.Fatal(err)
		}
	}
	// consume full reserve of catB
	for range 2 {
		if err := p.Acquire(catB); err != nil {
			t.Fatal(err)
		}
	}

	// acquire by catC and catD should fail immediately
	if err := p.Acquire(catC); !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("expected error: %v, got: %v", ErrUnsatisfiable, err)
	}
	if err := p.Acquire(catD); !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("expected error: %v, got: %v", ErrUnsatisfiable, err)
	}
}

/* ------------------------------------------------------------------
   9. Stats results should be immutable
-------------------------------------------------------------------*/

func TestStats(t *testing.T) {
	p := New(10, map[cat]int{catA: 3, catB: 2})
	defer p.Close()

	// acquire some slots to test stats calculation
	for range 6 {
		if err := p.Acquire(catA); err != nil {
			t.Fatal(err)
		}
	}
	for range 7 {
		go func() {
			_ = p.Acquire(catB) // last attempts will block
		}()
	}
	time.Sleep(10 * time.Millisecond) // let goroutines start

	stats := p.Stats()

	// Max should equal original capacity
	if stats.Max != 10 {
		t.Errorf("expected Max=10, got %d", stats.Max)
	}

	// Used should reflect current usage
	if stats.Used[catA] != 6 {
		t.Errorf("expected Used[catA]=6, got %d", stats.Used[catA])
	}
	if stats.Used[catB] != 4 {
		t.Errorf("expected Used[catB]=4, got %d", stats.Used[catB])
	}

	// Reserves should match what was set
	if stats.Reserves[catA] != 3 {
		t.Errorf("expected Reserves[catA]=3, got %d", stats.Reserves[catA])
	}
	if stats.Reserves[catB] != 2 {
		t.Errorf("expected Reserves[catB]=2, got %d", stats.Reserves[catB])
	}

	if stats.Queued[catA] != 0 {
		t.Errorf("expected Queued[catA]=0, got %d", stats.Queued[catA])
	}
	if stats.Queued[catB] != 3 {
		t.Errorf("expected Queued[catB]=3, got %d", stats.Queued[catB])
	}

	// Modifying returned maps should not affect internal state
	stats.Used[catA] = 999
	stats.Reserves[catA] = 999
	newStats := p.Stats()
	if newStats.Used[catA] != 6 {
		t.Error("modifying returned stats affected internal state")
	}
	if newStats.Reserves[catA] != 3 {
		t.Error("modifying returned stats affected internal state")
	}
}
