package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
)

const (
	operationScopeDelivery = "delivery"
	operationScopeNudge    = "nudge"
)

type operationLockStore interface {
	lockOperation(string, string) (func(), error)
}

// Some supported flock implementations associate locks with a process rather
// than treating two descriptors in that process as competitors. A bounded
// shared stripe table therefore provides the in-process half of the lock for
// every Store instance; flock supplies the cross-process half.
var operationProcessLocks [256]sync.Mutex

func operationLockStripe(scope, stableID string) uint8 {
	digest := digestText(scope + "\x00" + stableID)
	index, _ := strconv.ParseUint(digest[:2], 16, 8)
	return uint8(index)
}

func processOperationMutex(stripe uint8) *sync.Mutex {
	return &operationProcessLocks[stripe]
}

func validOperationScope(scope string) bool {
	return scope == operationScopeDelivery || scope == operationScopeNudge
}

// lockOperation serializes one external-effect protocol across processes. The
// lock intentionally spans ledger attempt, Herdr/transport mutation, and the
// durable terminal fact. A crashed process releases flock automatically, so a
// recovery command can distinguish an abandoned attempt from one still live.
//
// Global order for callers is process operation mutex -> operation flock ->
// registry -> ledger and, when a runtime mutation is involved, operation ->
// ESTOP admission -> registry -> ledger. No registry/ledger critical section
// may acquire an operation lock.
func (s *Store) lockOperation(scope, stableID string) (func(), error) {
	if s == nil || s.Dir == "" {
		return nil, fmt.Errorf("operation lock Store 未配置")
	}
	if !validOperationScope(scope) {
		return nil, fmt.Errorf("未知 operation lock scope：%s", scope)
	}
	if err := validateLedgerID("operation id", stableID); err != nil {
		return nil, err
	}
	stripe := operationLockStripe(scope, stableID)
	processMu := processOperationMutex(stripe)
	ctx := s.requestContext()
	if err := lockMutexContext(ctx, processMu); err != nil {
		return nil, fmt.Errorf("等待 operation process lock：%w", err)
	}
	releaseProcess := true
	defer func() {
		if releaseProcess {
			processMu.Unlock()
		}
	}()
	if err := mkdirDurable(s.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 operation lock data root：%w", err)
	}
	root := filepath.Join(s.Dir, ".operation-locks")
	if err := mkdirDurable(root, 0o700); err != nil {
		return nil, fmt.Errorf("创建 operation lock 目录：%w", err)
	}
	if err := validateOwnedMode(root, 0o700, true); err != nil {
		return nil, fmt.Errorf("operation lock 目录不安全：%w", err)
	}
	// Use the same bounded stripe for the process mutex and the filesystem
	// lock. This caps persistent lock files while preserving identical
	// serialization semantics across processes.
	name := fmt.Sprintf("%s-%02x.lock", scope, stripe)
	path := filepath.Join(root, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开 operation lock：%w", err)
	}
	cleanup := func() { _ = file.Close() }
	if err := validateOwnedMode(path, 0o600, false); err != nil {
		cleanup()
		return nil, fmt.Errorf("operation lock 文件不安全：%w", err)
	}
	if err := flockContext(ctx, int(file.Fd()), syscall.LOCK_EX); err != nil {
		cleanup()
		return nil, fmt.Errorf("取得 operation lock：%w", err)
	}
	releaseProcess = false
	var once sync.Once
	return func() {
		once.Do(func() {
			unlock(file)
			processMu.Unlock()
		})
	}, nil
}

func (a *App) lockOperation(scope, stableID string) (func(), error) {
	if a == nil || a.Store == nil {
		return nil, fmt.Errorf("operation lock Store 未注入")
	}
	store, ok := a.Store.(operationLockStore)
	if !ok {
		return nil, fmt.Errorf("Store 不支持跨进程 operation lock")
	}
	return store.lockOperation(scope, stableID)
}
