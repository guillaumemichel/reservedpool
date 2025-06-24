package reservedpool

import (
	"errors"
	"sync"
)

var (
	ErrClosed        = errors.New("reservedpool: closed")
	ErrUnsatisfiable = errors.New("reservedpool: unsatisfiable request")
)

type Pool[K comparable] struct {
	available    int
	reserve      map[K]int // per-category reserve
	used         map[K]int // currently held per category
	mu           sync.Mutex
	cond         *sync.Cond
	reservedOnly bool // if true, only categories with reserves can be used
	closed       bool
}

// New returns a pool with global limit max and the given reserves.
// The sum of reserves must be ≤ max.
func New[K comparable](max int, reserves map[K]int) *Pool[K] {
	p := &Pool[K]{
		available: max,
		reserve:   make(map[K]int, len(reserves)),
		used:      make(map[K]int),
	}
	sum := 0
	for i, r := range reserves {
		p.reserve[i] = r
		sum += r
	}
	if sum == max {
		// Reserves use all available slots
		p.reservedOnly = true
	} else if sum > max {
		panic("sum(reserves) > max")
	}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Acquire blocks until the caller may consume one slot for the given category.
// It returns ctx.Err() if the context is cancelled while waiting.
func (p *Pool[K]) Acquire(cat K) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.reservedOnly && p.reserve[cat] == 0 {
		return ErrUnsatisfiable
	}

	for {
		if p.closed {
			return ErrClosed
		}
		if p.canUse(cat) {
			p.used[cat]++
			p.available--
			return nil
		}
		p.cond.Wait() // releases p.mu while blocked
	}
}

// Release frees one slot previously acquired by cat.
func (p *Pool[K]) Release(cat K) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		// after Close(), extra releases are ignored
		return
	}
	if p.used[cat] == 0 {
		panic("reservedpool: release with zero usage")
	}
	p.used[cat]--
	p.available++
	p.cond.Broadcast()
}

// Close marks the pool closed and wakes all waiters.
// Further Acquire calls return ErrClosed. Releases are ignored.
func (p *Pool[K]) Close() {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		p.cond.Broadcast()
	}
	p.mu.Unlock()
}

// canUse implements the rule described above (called with p.mu held).
func (p *Pool[K]) canUse(cat K) bool {
	if p.available == 0 {
		return false
	}
	reservedForOthers := 0
	for k, rsv := range p.reserve {
		if k == cat {
			continue
		}
		if deficit := rsv - p.used[k]; deficit > 0 {
			reservedForOthers += deficit
		}
	}
	return p.available > reservedForOthers
}
