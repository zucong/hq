package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"
)

type EventStore interface {
	Transact(Config, string, string, bool, TransactionBuilder) (TransactionResult, error)
	Append(Event, Config) error
	Rebuild(Config) (Snapshot, error)
	Snapshot(Config) (Snapshot, error)
	ReadAll(Config) ([]Event, error)
	ReportAssignment(Config, string, string) (string, bool, error)
	Delivery(Config, string) (DeliveryView, bool, error)
	Deliveries(Config) ([]DeliveryView, error)
	EventRef(Event) string
	NowTime() time.Time
}

// candidateConfigReplayStore keeps the authoritative ledger unchanged between
// a candidate-registry replay and the config replacement guarded by that
// replay. It is intentionally narrower than EventStore so synthetic read-only
// stores do not accidentally claim this safety property.
type candidateConfigReplayStore interface {
	ReplayCandidateReadOnly(Config) error
	LockAndReplayCandidate(Config) (release func(), err error)
}

type Store struct {
	Dir        string
	ConfigPath string
	Now        func() time.Time
	Failpoint  func(string) error
	Context    context.Context

	configPathMu sync.RWMutex
}

func (s *Store) requestContext() context.Context {
	if s != nil {
		return nonNilContext(s.Context)
	}
	return context.Background()
}

func (s *Store) withRequestContext(ctx context.Context) *Store {
	if s == nil {
		return nil
	}
	return &Store{
		Dir: s.Dir, ConfigPath: s.boundConfigPath(), Now: s.Now, Failpoint: s.Failpoint,
		Context: nonNilContext(ctx),
	}
}

func NewStore(dir string) *Store {
	return &Store{Dir: dir, Now: time.Now}
}

func normalizeStoreConfigPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("Store config path 不能为空")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

// bindConfigPath permanently associates a production Store with the registry
// whose authorization rules its ledger is replayed against. Standalone Stores
// remain unbound so low-level replay and fixture tooling can explicitly
// choose a Config for each operation.
func (s *Store) bindConfigPath(path string) error {
	if s == nil {
		return fmt.Errorf("Store 不能为 nil")
	}
	normalized, err := normalizeStoreConfigPath(path)
	if err != nil {
		return err
	}
	s.configPathMu.Lock()
	defer s.configPathMu.Unlock()
	if s.ConfigPath != "" {
		bound, err := normalizeStoreConfigPath(s.ConfigPath)
		if err != nil {
			return err
		}
		if bound != normalized {
			return conflictf("Store 已绑定 config %s，拒绝改绑 %s", bound, normalized)
		}
	}
	s.ConfigPath = normalized
	return nil
}

func (s *Store) boundConfigPath() string {
	s.configPathMu.RLock()
	defer s.configPathMu.RUnlock()
	return s.ConfigPath
}

// lockCurrentRegistry establishes the global lock order used by every bound
// ledger reader/writer:
//
//	config process R/W -> config directory SH/EX -> ledger
//
// Keeping the shared registry lease until the ledger lock is released prevents
// a config replacement from overtaking a process that loaded the old registry
// but has not yet appended. Candidate replay deliberately does not use this
// helper: its caller already owns the exclusive registry lease and must replay
// the candidate (not the currently installed config).
func (s *Store) lockCurrentRegistry(cfg Config) (func(), error) {
	ctx := s.requestContext()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := s.boundConfigPath()
	if path == "" {
		return func() {}, nil
	}

	if err := lockRWMutexReadContext(ctx, &configMutationProcessMu); err != nil {
		return nil, fmt.Errorf("等待 config process lease：%w", err)
	}
	dirLock, err := os.Open(filepath.Dir(path))
	if err != nil {
		configMutationProcessMu.RUnlock()
		return nil, fmt.Errorf("锁定 config 目录：%w", err)
	}
	flocked := false
	cleanup := func() {
		if flocked {
			_ = syscall.Flock(int(dirLock.Fd()), syscall.LOCK_UN)
		}
		_ = dirLock.Close()
		configMutationProcessMu.RUnlock()
	}
	if info, err := dirLock.Stat(); err != nil {
		cleanup()
		return nil, fmt.Errorf("检查 config 目录：%w", err)
	} else if !info.IsDir() {
		cleanup()
		return nil, fmt.Errorf("config 父路径不是目录：%s", filepath.Dir(path))
	}
	if err := flockContext(ctx, int(dirLock.Fd()), syscall.LOCK_SH); err != nil {
		cleanup()
		return nil, fmt.Errorf("锁定 config 目录：%w", err)
	}
	flocked = true

	current, err := loadConfig(path)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("在 registry lease 下重载 config：%w", err)
	}
	if !reflect.DeepEqual(current, cfg) {
		cleanup()
		return nil, conflictf("调用者 config 已过期；必须重载 %s 后重试", path)
	}
	var once sync.Once
	return func() { once.Do(cleanup) }, nil
}

func (s *Store) ledgerStateReadOnly(cfg Config) (*ledgerState, error) {
	releaseRegistry, err := s.lockCurrentRegistry(cfg)
	if err != nil {
		return nil, err
	}
	defer releaseRegistry()

	txnDir := filepath.Join(s.Dir, "txn")
	if info, err := os.Lstat(txnDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("txn 必须是非 symlink 目录：%s", txnDir)
		}
		entries, readErr := os.ReadDir(txnDir)
		if readErr != nil {
			return nil, readErr
		}
		if len(entries) != 0 {
			return nil, fmt.Errorf("账本存在待恢复 txn intent；只读查询拒绝隐式恢复")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	_, ledger, err := s.readLedgerUnlocked(cfg, "", 0)
	return ledger, err
}

// LedgerStateReadOnly replays only fully appended events and never creates a
// records directory, lock file, snapshot, or recovery artifact. A durable txn
// intent is fail-closed here: a read-only query must not decide whether to
// recover authoritative state as a side effect.
func (s *Store) LedgerStateReadOnly(cfg Config) (*ledgerState, error) {
	return s.ledgerStateReadOnly(cfg)
}

// ReplayCandidateReadOnly is the dry-run form of candidate validation. It does
// not create the records directory or a lock sidecar when the ledger is empty.
func (s *Store) ReplayCandidateReadOnly(cfg Config) error {
	if err := s.replayCandidateUnlocked(cfg); err != nil {
		return conflictf("候选 config 无法完整重放现有 ledger：%v", err)
	}
	return nil
}

// LockAndReplayCandidate performs a complete strict replay under the same file
// lock used by every ledger transaction. The caller must hold the returned
// guard until the candidate config has either been atomically installed or
// abandoned, closing the replay-to-replacement TOCTOU window.
func (s *Store) LockAndReplayCandidate(cfg Config) (release func(), err error) {
	releaseLedger, err := s.lock()
	if err != nil {
		return nil, fmt.Errorf("锁定事件账本以验证候选配置：%w", err)
	}
	keepLock := false
	defer func() {
		if !keepLock {
			releaseLedger()
		}
	}()
	if err := s.replayCandidateUnlocked(cfg); err != nil {
		return nil, conflictf("候选 config 无法完整重放现有 ledger：%v", err)
	}
	keepLock = true
	var once sync.Once
	return func() { once.Do(releaseLedger) }, nil
}

// replayCandidateUnlocked validates the durable ledger state that recovery
// would expose, without performing recovery writes. A transaction intent is
// durable before its batch is appended, so ignoring txn/ here would allow a
// registry replacement that validates the visible log but invalidates the
// already-committed pending batch.
func (s *Store) replayCandidateUnlocked(cfg Config) error {
	txnDir := filepath.Join(s.Dir, "txn")
	if info, err := os.Lstat(txnDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("txn 必须是非 symlink 目录：%s", txnDir)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	entries, err := os.ReadDir(txnDir)
	if os.IsNotExist(err) {
		_, ledger, replayErr := s.readLedgerUnlocked(cfg, "", 0)
		if replayErr != nil {
			return replayErr
		}
		return ledger.validateCandidateSeatContinuity(cfg)
	}
	if err != nil {
		return err
	}
	var journalPath string
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("txn 目录含非法条目：%s", entry.Name())
		}
		if strings.HasSuffix(entry.Name(), ".tmp") {
			return fmt.Errorf("txn intent 不完整，拒绝候选 config：%s", filepath.Join(txnDir, entry.Name()))
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			return fmt.Errorf("txn 目录含未知文件：%s", entry.Name())
		}
		if journalPath != "" {
			return fmt.Errorf("txn 目录含多个待恢复 intent，拒绝候选 config")
		}
		journalPath = filepath.Join(txnDir, entry.Name())
	}
	if journalPath == "" {
		_, ledger, replayErr := s.readLedgerUnlocked(cfg, "", 0)
		if replayErr != nil {
			return replayErr
		}
		return ledger.validateCandidateSeatContinuity(cfg)
	}
	return s.replayCandidateJournalUnlocked(cfg, journalPath)
}

func (s *Store) replayCandidateJournalUnlocked(cfg Config, journalPath string) error {
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		return err
	}
	var journal transactionJournal
	if err := decodeStrictJSON(raw, &journal); err != nil {
		return fmt.Errorf("txn intent 损坏 %s：%w", journalPath, err)
	}
	if journal.Version != 1 || !monthlyFilenamePattern.MatchString(journal.LogFile) ||
		filepath.Base(journal.StateTempFile) != journal.StateTempFile || !strings.HasPrefix(journal.StateTempFile, ".state-") {
		return fmt.Errorf("txn intent 元数据非法：%s", journalPath)
	}
	if digestBytes(journal.Batch) != journal.BatchDigest {
		return fmt.Errorf("txn intent batch digest 不匹配：%s", journalPath)
	}
	logPath := filepath.Join(s.Dir, "events", journal.LogFile)
	actual, _, err := readRegularFileIfExists(logPath)
	if err != nil {
		return err
	}
	maxLogLength := journal.OldLogLength + int64(len(journal.Batch))
	if journal.OldLogLength < 0 || maxLogLength < journal.OldLogLength ||
		int64(len(actual)) < journal.OldLogLength || int64(len(actual)) > maxLogLength {
		return fmt.Errorf("txn 候选重放时日志长度与 intent 不匹配：%s", logPath)
	}
	prefix := actual[:journal.OldLogLength]
	if digestBytes(prefix) != journal.OldLogDigest {
		return fmt.Errorf("txn 候选重放时旧日志 digest 不匹配：%s", logPath)
	}
	_, ledger, err := s.readLedgerUnlocked(cfg, logPath, journal.OldLogLength)
	if err != nil {
		return fmt.Errorf("txn 候选重放前旧账本无效：%w", err)
	}
	oldHash := ledger.snapshot.LastEventHash
	if oldHash == "" {
		oldHash = genesisEventHash
	}
	if ledger.snapshot.LastSequence != journal.OldSequence || oldHash != journal.OldEventHash {
		return fmt.Errorf("txn 候选重放旧尾证据不匹配：sequence/hash")
	}
	suffix := actual[journal.OldLogLength:]
	if !bytes.Equal(suffix, journal.Batch[:len(suffix)]) {
		return fmt.Errorf("txn 候选重放时日志后缀不是 intent batch 前缀：%s", logPath)
	}
	pending, err := parseEventFile(logPath, journal.LogFile, journal.Batch)
	if err != nil {
		return fmt.Errorf("txn intent batch 无效：%w", err)
	}
	for _, item := range pending {
		if err := ledger.validateAndApply(item.event, cfg); err != nil {
			return fmt.Errorf("%s:%d pending 事件语义无效：%w", item.path, item.line, err)
		}
	}
	if err := ledger.validateLedgerFinalInvariants(cfg); err != nil {
		return fmt.Errorf("txn intent batch 尾状态无效：%w", err)
	}
	return nil
}

func (s *Store) NowTime() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

var ledgerProcessLocks keyedContextFairLocks

func (s *Store) lock() (func(), error) {
	ctx := s.requestContext()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := mkdirDurable(s.Dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(s.Dir, ".hq.lock")
	releaseProcess, err := ledgerProcessLocks.lock(ctx, filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	releaseOnError := true
	defer func() {
		if releaseOnError {
			releaseProcess()
		}
	}()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("账本锁必须是非 symlink 普通文件：%s", path)
	}
	if err := flockContext(ctx, int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
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

func unlock(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func (s *Store) EventLogPath(at time.Time) string {
	return filepath.Join(s.Dir, "events", at.Format("2006-01")+".jsonl")
}

func (s *Store) EventRef(event Event) string {
	at, err := time.Parse(time.RFC3339, event.At)
	if err != nil {
		at = s.NowTime()
	}
	return s.EventLogPath(at) + "#" + event.ID
}

// Append is a convenience wrapper used by synthetic fixtures. Production
// command paths use Transact's single-lock read/replay/check/append transaction.
func (s *Store) Append(event Event, cfg Config) error {
	commandID := event.CommandID
	if commandID == "" {
		commandID = stableCommandID("append", event.ID)
	}
	digest := event.CommandDigest
	if digest == "" {
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		digest = digestText(string(raw))
	}
	_, err := s.Transact(cfg, commandID, digest, false, func(*ledgerState) (Event, error) {
		event.Sequence = 0
		event.PreviousEventHash = ""
		event.EventHash = ""
		return event, nil
	})
	return err
}

func (s *Store) Rebuild(cfg Config) (Snapshot, error) {
	releaseRegistry, err := s.lockCurrentRegistry(cfg)
	if err != nil {
		return Snapshot{}, err
	}
	defer releaseRegistry()
	releaseLedger, err := s.lock()
	if err != nil {
		return Snapshot{}, fmt.Errorf("锁定事件账本：%w", err)
	}
	defer releaseLedger()
	if err := s.recoverLocked(cfg); err != nil {
		return Snapshot{}, err
	}
	return s.rebuildLocked(cfg)
}

func (s *Store) rebuildLocked(cfg Config) (Snapshot, error) {
	_, ledger, err := s.readLedgerUnlocked(cfg, "", 0)
	if err != nil {
		return Snapshot{}, err
	}
	if err := s.writeStateLocked(ledger.snapshot, ".state-rebuild.tmp"); err != nil {
		return Snapshot{}, err
	}
	return ledger.snapshot, nil
}

func (s *Store) Snapshot(cfg Config) (Snapshot, error) {
	releaseRegistry, err := s.lockCurrentRegistry(cfg)
	if err != nil {
		return Snapshot{}, err
	}
	defer releaseRegistry()
	releaseLedger, err := s.lock()
	if err != nil {
		return Snapshot{}, fmt.Errorf("锁定事件账本：%w", err)
	}
	defer releaseLedger()
	if err := s.recoverLocked(cfg); err != nil {
		return Snapshot{}, err
	}
	_, ledger, err := s.readLedgerUnlocked(cfg, "", 0)
	if err != nil {
		return Snapshot{}, err
	}
	stored, stateErr := s.readSnapshotStrict()
	if stateErr != nil || !snapshotsEqual(stored, ledger.snapshot) {
		if err := s.writeStateLocked(ledger.snapshot, ".state-refresh.tmp"); err != nil {
			return Snapshot{}, err
		}
	}
	return ledger.snapshot, nil
}

// SnapshotReadOnly replays the authoritative event files without creating a
// lock or refreshing derived state. Like LedgerStateReadOnly, it fails closed
// on a durable txn intent instead of deciding recovery from a read path.
func (s *Store) SnapshotReadOnly(cfg Config) (Snapshot, error) {
	ledger, err := s.ledgerStateReadOnly(cfg)
	if err != nil {
		return Snapshot{}, err
	}
	return ledger.snapshot, nil
}

func (s *Store) ReadAll(cfg Config) ([]Event, error) {
	releaseRegistry, err := s.lockCurrentRegistry(cfg)
	if err != nil {
		return nil, err
	}
	defer releaseRegistry()
	releaseLedger, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer releaseLedger()
	if err := s.recoverLocked(cfg); err != nil {
		return nil, err
	}
	return s.readAllUnlocked(cfg)
}

func (s *Store) readAllUnlocked(cfg Config) ([]Event, error) {
	events, _, err := s.readLedgerUnlocked(cfg, "", 0)
	return events, err
}

func (s *Store) ReportAssignment(cfg Config, caseID, actor string) (string, bool, error) {
	releaseRegistry, err := s.lockCurrentRegistry(cfg)
	if err != nil {
		return "", false, err
	}
	defer releaseRegistry()
	releaseLedger, err := s.lock()
	if err != nil {
		return "", false, err
	}
	defer releaseLedger()
	if err := s.recoverLocked(cfg); err != nil {
		return "", false, err
	}
	_, ledger, err := s.readLedgerUnlocked(cfg, "", 0)
	if err != nil {
		return "", false, err
	}
	id, ok := ledger.assignmentFor(caseID, actor)
	return id, ok, nil
}

func findEvent(events []Event, id string) (Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].ID == id {
			return events[i], true
		}
	}
	return Event{}, false
}

func pendingEvents(events []Event, recipient string) []Event {
	byID := make(map[string]Event, len(events))
	resolved := map[string]bool{}
	for _, event := range events {
		byID[event.ID] = event
		if event.RelatedEventID != "" && (event.Type == "event_accepted" || event.Type == "event_returned" || event.Type == "message_acked") {
			resolved[event.RelatedEventID] = true
		}
	}
	var pending []Event
	for _, event := range events {
		semantic, ok := semanticDeliveredEvent(event, byID)
		if !ok || semantic.Recipient != recipient || resolved[event.ID] {
			continue
		}
		pending = append(pending, event)
	}
	return pending
}
