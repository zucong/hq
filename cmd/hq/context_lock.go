package main

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"time"
)

const contextLockPollInterval = 10 * time.Millisecond

// contextFairMutex is a cancellable FIFO mutex. It is used in front of the
// ledger flock so a caller that just released the ledger cannot barge ahead of
// admissions which are already waiting. That ordering is part of the delivery
// wake-budget contract: all admissions in the current batch must observe the
// reserved budget before a later delivery attempt can establish a natural-turn
// boundary and reset it.
//
// A canceled waiter is removed without disturbing the relative order of the
// remaining waiters. If cancellation races with a grant, the waiter hands the
// lock to the next waiter before returning the context error.
type contextFairMutex struct {
	mu      sync.Mutex
	locked  bool
	waiters []*contextFairWaiter
}

type contextFairWaiter struct {
	ready    chan struct{}
	granted  bool
	canceled bool
}

func (m *contextFairMutex) lock(ctx context.Context) error {
	ctx = nonNilContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	if !m.locked {
		m.locked = true
		m.mu.Unlock()
		if err := ctx.Err(); err != nil {
			m.unlock()
			return err
		}
		return nil
	}
	waiter := &contextFairWaiter{ready: make(chan struct{})}
	m.waiters = append(m.waiters, waiter)
	m.mu.Unlock()

	select {
	case <-waiter.ready:
		if err := ctx.Err(); err != nil {
			m.unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		m.mu.Lock()
		if !waiter.granted {
			waiter.canceled = true
			m.mu.Unlock()
			return ctx.Err()
		}
		m.mu.Unlock()
		m.unlock()
		return ctx.Err()
	}
}

func (m *contextFairMutex) unlock() {
	m.mu.Lock()
	if !m.locked {
		m.mu.Unlock()
		panic("unlock of unlocked contextFairMutex")
	}
	for len(m.waiters) > 0 {
		waiter := m.waiters[0]
		m.waiters[0] = nil
		m.waiters = m.waiters[1:]
		if waiter.canceled {
			continue
		}
		waiter.granted = true
		close(waiter.ready)
		m.mu.Unlock()
		return
	}
	m.locked = false
	m.mu.Unlock()
}

// keyedContextFairLocks shares one FIFO lock between all Store instances bound
// to the same ledger path. Entries are reference-counted so short-lived test or
// company directories do not accumulate forever in a long-running gateway.
type keyedContextFairLocks struct {
	mu      sync.Mutex
	entries map[string]*keyedContextFairLock
}

type keyedContextFairLock struct {
	mutex contextFairMutex
	refs  int
}

func (locks *keyedContextFairLocks) lock(ctx context.Context, key string) (func(), error) {
	locks.mu.Lock()
	if locks.entries == nil {
		locks.entries = make(map[string]*keyedContextFairLock)
	}
	entry := locks.entries[key]
	if entry == nil {
		entry = &keyedContextFairLock{}
		locks.entries[key] = entry
	}
	entry.refs++
	locks.mu.Unlock()

	releaseRef := func() {
		locks.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(locks.entries, key)
		}
		locks.mu.Unlock()
	}
	if err := entry.mutex.lock(ctx); err != nil {
		releaseRef()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mutex.unlock()
			releaseRef()
		})
	}, nil
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func acquireTryLockContext(ctx context.Context, try func() bool, unlock func()) error {
	ctx = nonNilContext(ctx)
	attempt := func() (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !try() {
			return false, nil
		}
		if err := ctx.Err(); err != nil {
			unlock()
			return false, err
		}
		return true, nil
	}
	if acquired, err := attempt(); acquired || err != nil {
		return err
	}
	ticker := time.NewTicker(contextLockPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if acquired, err := attempt(); acquired || err != nil {
				return err
			}
		}
	}
}

func lockMutexContext(ctx context.Context, mutex *sync.Mutex) error {
	ctx = nonNilContext(ctx)
	if ctx.Done() == nil {
		mutex.Lock()
		return nil
	}
	return acquireTryLockContext(ctx, mutex.TryLock, mutex.Unlock)
}

func lockRWMutexReadContext(ctx context.Context, mutex *sync.RWMutex) error {
	ctx = nonNilContext(ctx)
	if ctx.Done() == nil {
		mutex.RLock()
		return nil
	}
	return acquireTryLockContext(ctx, mutex.TryRLock, mutex.RUnlock)
}

func lockRWMutexContext(ctx context.Context, mutex *sync.RWMutex) error {
	ctx = nonNilContext(ctx)
	if ctx.Done() == nil {
		mutex.Lock()
		return nil
	}
	return acquireTryLockContext(ctx, mutex.TryLock, mutex.Unlock)
}

func flockContext(ctx context.Context, fd int, mode int) error {
	ctx = nonNilContext(ctx)
	// Preserve the kernel's blocking waiter queue whenever cancellation is not
	// requested. Gateway calls carry a deadline and use the cancellable path
	// below; the per-ledger FIFO gate keeps those callers from barging within the
	// gateway process before they contend on the cross-process flock.
	if ctx.Done() == nil {
		return syscall.Flock(fd, mode)
	}
	attempt := func() (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		err := syscall.Flock(fd, mode|syscall.LOCK_NB)
		if err == nil {
			if contextErr := ctx.Err(); contextErr != nil {
				_ = syscall.Flock(fd, syscall.LOCK_UN)
				return false, contextErr
			}
			return true, nil
		}
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil
		}
		return false, err
	}
	if acquired, err := attempt(); acquired || err != nil {
		return err
	}
	ticker := time.NewTicker(contextLockPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if acquired, err := attempt(); acquired || err != nil {
				return err
			}
		}
	}
}
