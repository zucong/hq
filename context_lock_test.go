package main

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func waitForContextFairWaiters(t *testing.T, mutex *contextFairMutex, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		mutex.mu.Lock()
		got := len(mutex.waiters)
		mutex.mu.Unlock()
		if got >= count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fair mutex waiters=%d want at least %d", got, count)
		}
		runtime.Gosched()
	}
}

func TestContextFairMutexQueuedWaiterCannotBeBarged(t *testing.T) {
	var mutex contextFairMutex
	if err := mutex.lock(context.Background()); err != nil {
		t.Fatal(err)
	}

	order := make(chan string, 2)
	releaseFirst := make(chan struct{})
	go func() {
		if err := mutex.lock(context.Background()); err != nil {
			order <- "first-error:" + err.Error()
			return
		}
		order <- "first"
		<-releaseFirst
		mutex.unlock()
	}()
	waitForContextFairWaiters(t, &mutex, 1)

	go func() {
		if err := mutex.lock(context.Background()); err != nil {
			order <- "barger-error:" + err.Error()
			return
		}
		order <- "barger"
		mutex.unlock()
	}()
	waitForContextFairWaiters(t, &mutex, 2)

	mutex.unlock()
	if got := <-order; got != "first" {
		t.Fatalf("new ledger contender barged ahead of queued admission: %s", got)
	}
	close(releaseFirst)
	if got := <-order; got != "barger" {
		t.Fatalf("unexpected second lock owner: %s", got)
	}
}

func TestContextFairMutexCancellationPreservesQueue(t *testing.T) {
	var mutex contextFairMutex
	if err := mutex.lock(context.Background()); err != nil {
		t.Fatal(err)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	canceledResult := make(chan error, 1)
	go func() { canceledResult <- mutex.lock(canceledCtx) }()
	waitForContextFairWaiters(t, &mutex, 1)

	nextAcquired := make(chan error, 1)
	go func() {
		err := mutex.lock(context.Background())
		nextAcquired <- err
		if err == nil {
			mutex.unlock()
		}
	}()
	waitForContextFairWaiters(t, &mutex, 2)

	cancel()
	if err := <-canceledResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter result=%v", err)
	}
	mutex.unlock()
	select {
	case err := <-nextAcquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter stranded the next FIFO waiter")
	}
}
