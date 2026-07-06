package fastdb

import (
	"sync"
	"sync/atomic"
)

// Subscription is a handle to a write-notifier registered with a Pool.
// Each subscription's callback fires on every INSERT/UPDATE/DELETE made
// through any of the pool's connections, plus on data_version bumps
// (delivered as a generic "all tables changed" event with table=="").
//
// The callback runs synchronously on the writing connection's thread
// (inside SQLite's update_hook). Keep it short — defer real work to a
// dedicated goroutine if it could block or do I/O.
//
// Subscriptions outlive the goroutine that created them; close them via
// Close() to detach. Forgotten subscriptions leak.
type Subscription struct {
	pool   *Pool
	fn     func(table string)
	closed atomic.Bool
}

// Subscribe registers fn as a write-notifier on the pool. Returns the
// subscription handle for later Close(). Passing a nil fn returns nil.
//
// Subscribers are stored in a copy-on-write slice; read paths
// (notifySubscribers, called from every write hook) are lock-free for
// the iteration. Subscribe/Close serialize on a mutex; both are rare.
func (p *Pool) Subscribe(fn func(table string)) *Subscription {
	if fn == nil {
		return nil
	}
	sub := &Subscription{pool: p, fn: fn}

	p.subMu.Lock()
	cur := p.subs.Load()
	var next []*Subscription
	if cur == nil {
		next = []*Subscription{sub}
	} else {
		next = make([]*Subscription, len(*cur)+1)
		copy(next, *cur)
		next[len(*cur)] = sub
	}
	p.subs.Store(&next)
	p.subMu.Unlock()
	return sub
}

// Close detaches the subscription. Idempotent — second and later calls
// are no-ops.
func (s *Subscription) Close() {
	if s == nil || !s.closed.CompareAndSwap(false, true) {
		return
	}
	p := s.pool

	p.subMu.Lock()
	defer p.subMu.Unlock()
	cur := p.subs.Load()
	if cur == nil {
		return
	}
	next := make([]*Subscription, 0, len(*cur))
	for _, x := range *cur {
		if x != s {
			next = append(next, x)
		}
	}
	p.subs.Store(&next)
}

// notifySubscribers fires every active subscription with the given
// table. table=="" means "all tables may have changed" — used by the
// data_version watcher when an out-of-pool write is detected.
//
// Called from the update_hook trampoline (per row write) and the
// dataVersionWatcher (per detected external write). Lock-free read of
// the subscriptions slice: atomic load + range.
func (p *Pool) notifySubscribers(table string) {
	cur := p.subs.Load()
	if cur == nil {
		return
	}
	for _, s := range *cur {
		s.fn(table)
	}
}

// Subscribe is the package-level form. Registers fn against the default
// pool (the one Init created). Returns nil if Init hasn't run yet.
func Subscribe(fn func(table string)) *Subscription {
	if defaultPool == nil {
		return nil
	}
	return defaultPool.Subscribe(fn)
}

// subscribers field is added to Pool in fastdb.go — declared here as a
// methodless type alias to keep all subscription machinery in one file.
// (Pool struct itself is defined in fastdb.go.)
var _ sync.Mutex // touch sync to keep import even if other Pool methods drop it
