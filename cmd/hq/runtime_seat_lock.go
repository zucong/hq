package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// runtimeSeatProcessLocks is deliberately independent from
// operationProcessLocks. Delivery holds its delivery/nudge operation lease
// before entering this domain, so sharing stripes would make an otherwise
// valid operation -> runtime-seat nesting self-deadlock on a hash collision.
var runtimeSeatProcessLocks keyedContextFairLocks

type runtimeSeatLockStore interface {
	lockRuntimeSeat(string) (func(), error)
}

// lockRuntimeSeat serializes every Prompt, cold-resume and hibernation decision
// for one stable registry seat across goroutines and HQ processes. The in-
// process half is FIFO/cancellable; flock provides the cross-process half.
//
// Global order for callers is:
//
//	delivery/nudge operation -> runtime-seat -> ESTOP admission -> up -> registry -> ledger
//
// Hibernation has no operation lease and starts at runtime-seat. No registry,
// ledger or ESTOP critical section may acquire a runtime-seat lease.
func (s *Store) lockRuntimeSeat(agent string) (func(), error) {
	if s == nil || s.Dir == "" {
		return nil, fmt.Errorf("runtime-seat lock Store 未配置")
	}
	if !agentNamePattern.MatchString(agent) {
		return nil, fmt.Errorf("runtime-seat agent slug 非法：%s", agent)
	}
	rootPath, err := filepath.Abs(s.Dir)
	if err != nil {
		return nil, fmt.Errorf("解析 runtime-seat data root：%w", err)
	}
	ctx := s.requestContext()
	processKey := filepath.Clean(rootPath) + "\x00" + agent
	releaseProcess, err := runtimeSeatProcessLocks.lock(ctx, processKey)
	if err != nil {
		return nil, fmt.Errorf("等待 runtime-seat process lease：%w", err)
	}
	releaseOnError := true
	defer func() {
		if releaseOnError {
			releaseProcess()
		}
	}()

	if err := mkdirDurable(s.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 runtime-seat data root：%w", err)
	}
	root := filepath.Join(s.Dir, ".runtime-seat-locks")
	if err := mkdirDurable(root, 0o700); err != nil {
		return nil, fmt.Errorf("创建 runtime-seat lock 目录：%w", err)
	}
	if err := validateOwnedMode(root, 0o700, true); err != nil {
		return nil, fmt.Errorf("runtime-seat lock 目录不安全：%w", err)
	}
	// The full digest gives each seat its own file; it is not a bounded stripe
	// and therefore does not serialize unrelated employees.
	path := filepath.Join(root, "seat-"+digestText(agent)+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开 runtime-seat lock：%w", err)
	}
	closeFile := func() { _ = file.Close() }
	if err := validateOwnedMode(path, 0o600, false); err != nil {
		closeFile()
		return nil, fmt.Errorf("runtime-seat lock 文件不安全：%w", err)
	}
	if err := flockContext(ctx, int(file.Fd()), syscall.LOCK_EX); err != nil {
		closeFile()
		return nil, fmt.Errorf("取得 runtime-seat lock：%w", err)
	}
	releaseOnError = false
	var once sync.Once
	return func() {
		once.Do(func() {
			unlock(file)
			releaseProcess()
		})
	}, nil
}

func (a *App) lockRuntimeSeat(agent string) (func(), error) {
	if a == nil || a.Store == nil {
		return nil, fmt.Errorf("runtime-seat lock Store 未注入")
	}
	store, ok := a.Store.(runtimeSeatLockStore)
	if !ok {
		return nil, fmt.Errorf("Store 不支持跨进程 runtime-seat lease")
	}
	return store.lockRuntimeSeat(agent)
}
