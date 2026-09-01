package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type TransactionResult struct {
	Event            Event
	Snapshot         Snapshot
	AlreadyCommitted bool
	DryRun           bool
}

type TransactionBuilder func(*ledgerState) (Event, error)

type BatchTransactionResult struct {
	Events           []Event
	Snapshot         Snapshot
	AlreadyCommitted bool
	DryRun           bool
}

// BatchTransactionBuilder returns events in their required append order. The
// final event carries the caller's command_id; preceding events receive stable
// child command ids. This lets a retry discover the completed business command
// while keeping a multi-event lifecycle change atomic. Builders run while both
// the registry lease and ledger lock are held, so they must not re-enter Store
// or mutate the registry.
type BatchTransactionBuilder func(*ledgerState) ([]Event, error)

type transactionJournal struct {
	Version       int    `json:"journal_version"`
	TransactionID string `json:"transaction_id"`
	LogFile       string `json:"log_file"`
	OldLogLength  int64  `json:"old_log_length"`
	OldLogDigest  string `json:"old_log_digest"`
	OldSequence   int64  `json:"old_sequence"`
	OldEventHash  string `json:"old_event_hash"`
	Batch         []byte `json:"batch"`
	BatchDigest   string `json:"batch_digest"`
	StateTempFile string `json:"state_temp_file"`
}

func (s *Store) hit(name string) error {
	if s.Failpoint == nil {
		return nil
	}
	if err := s.Failpoint(name); err != nil {
		return fmt.Errorf("failpoint %s: %w", name, err)
	}
	return nil
}

func mkdirDurable(path string, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("路径必须是非 symlink 目录：%s", path)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func digestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func readRegularFileIfExists(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("必须是非 symlink 普通文件：%s", path)
	}
	raw, err := os.ReadFile(path)
	return raw, true, err
}

func (s *Store) Transact(cfg Config, commandID, commandDigest string, dryRun bool, build TransactionBuilder) (TransactionResult, error) {
	if build == nil {
		return TransactionResult{}, fmt.Errorf("transaction builder 不能为空")
	}
	result, err := s.TransactBatch(cfg, commandID, commandDigest, dryRun, func(ledger *ledgerState) ([]Event, error) {
		event, buildErr := build(ledger)
		if buildErr != nil {
			return nil, buildErr
		}
		return []Event{event}, nil
	})
	if err != nil {
		return TransactionResult{}, err
	}
	return TransactionResult{
		Event: result.Events[len(result.Events)-1], Snapshot: result.Snapshot,
		AlreadyCommitted: result.AlreadyCommitted, DryRun: result.DryRun,
	}, nil
}

func (s *Store) TransactBatch(cfg Config, commandID, commandDigest string, dryRun bool, build BatchTransactionBuilder) (BatchTransactionResult, error) {
	if err := validateLedgerID("command_id", commandID); err != nil {
		return BatchTransactionResult{}, err
	}
	if err := validateDigest("command_digest", commandDigest); err != nil {
		return BatchTransactionResult{}, err
	}
	if build == nil {
		return BatchTransactionResult{}, fmt.Errorf("transaction builder 不能为空")
	}
	releaseRegistry, err := s.lockCurrentRegistry(cfg)
	if err != nil {
		return BatchTransactionResult{}, err
	}
	defer releaseRegistry()
	releaseLedger, err := s.lock()
	if err != nil {
		return BatchTransactionResult{}, fmt.Errorf("锁定事件账本：%w", err)
	}
	defer releaseLedger()
	if err := s.recoverLocked(cfg); err != nil {
		return BatchTransactionResult{}, err
	}
	events, ledger, err := s.readLedgerUnlocked(cfg, "", 0)
	if err != nil {
		return BatchTransactionResult{}, err
	}
	if committed, ok := ledger.commands[commandID]; ok {
		if committed.CommandDigest != commandDigest {
			return BatchTransactionResult{}, conflictf("command_id %s 已提交但 request digest 不同", commandID)
		}
		return BatchTransactionResult{Events: []Event{committed}, Snapshot: ledger.snapshot, AlreadyCommitted: true, DryRun: dryRun}, nil
	}
	// Sequence exhaustion must fail before the builder can create an event and
	// before any intent, event, or derived state write. Locks and strict replay
	// are read/check infrastructure, not sequence allocation side effects.
	if ledger.snapshot.LastSequence == math.MaxInt64 {
		return BatchTransactionResult{}, fmt.Errorf("sequence 已达 MaxInt64，拒绝分配新事件")
	}
	batch, err := build(ledger)
	if err != nil {
		return BatchTransactionResult{}, err
	}
	if len(batch) == 0 {
		return BatchTransactionResult{}, fmt.Errorf("transaction batch 不能为空")
	}
	if int64(len(batch)) > math.MaxInt64-ledger.snapshot.LastSequence {
		return BatchTransactionResult{}, fmt.Errorf("sequence 空间不足以提交 %d 个事件", len(batch))
	}
	previousHash := ledger.snapshot.LastEventHash
	if previousHash == "" {
		previousHash = genesisEventHash
	}
	for i := range batch {
		eventCommandID := commandID
		if i != len(batch)-1 {
			eventCommandID = fmt.Sprintf("%s:part:%d", commandID, i+1)
		}
		if err := validateLedgerID("batch command_id", eventCommandID); err != nil {
			return BatchTransactionResult{}, err
		}
		batch[i].Version = eventSchemaVersion
		batch[i].CommandID = eventCommandID
		batch[i].CommandDigest = commandDigest
		batch[i].Sequence = ledger.snapshot.LastSequence + 1
		batch[i].PreviousEventHash = previousHash
		batch[i].EventHash = ""
		batch[i].EventHash, err = hashEvent(batch[i])
		if err != nil {
			return BatchTransactionResult{}, err
		}
		if err := ledger.validateAndApply(batch[i], cfg); err != nil {
			return BatchTransactionResult{}, fmt.Errorf("拒绝事务 command=%s event=%s：%w", commandID, batch[i].ID, err)
		}
		previousHash = batch[i].EventHash
	}
	if err := ledger.validateLedgerFinalInvariants(cfg); err != nil {
		return BatchTransactionResult{}, fmt.Errorf("拒绝事务 command=%s 的 ledger 尾状态：%w", commandID, err)
	}
	result := BatchTransactionResult{Events: batch, Snapshot: ledger.snapshot, DryRun: dryRun}
	if dryRun {
		return result, nil
	}
	if err := s.commitBatchLocked(events, batch, ledger.snapshot); err != nil {
		return BatchTransactionResult{}, fmt.Errorf("提交事务 command=%s event=%s：%w", commandID, batch[len(batch)-1].ID, err)
	}
	return result, nil
}

func (s *Store) commitLocked(previous []Event, event Event, snapshot Snapshot) error {
	return s.commitBatchLocked(previous, []Event{event}, snapshot)
}

func (s *Store) commitBatchLocked(previous []Event, batchEvents []Event, snapshot Snapshot) error {
	eventsDir := filepath.Join(s.Dir, "events")
	if err := mkdirDurable(eventsDir, 0o755); err != nil {
		return err
	}
	txnDir := filepath.Join(s.Dir, "txn")
	if err := mkdirDurable(txnDir, 0o700); err != nil {
		return err
	}
	at, err := time.Parse(time.RFC3339, batchEvents[0].At)
	if err != nil {
		return fmt.Errorf("事件时间格式错误：%w", err)
	}
	logFile := at.Format("2006-01") + ".jsonl"
	if !monthlyFilenamePattern.MatchString(logFile) {
		return fmt.Errorf("非法月度事件文件名：%s", logFile)
	}
	logPath := filepath.Join(eventsDir, logFile)
	oldBytes, existed, err := readRegularFileIfExists(logPath)
	if err != nil {
		return err
	}
	var line []byte
	for _, event := range batchEvents {
		eventAt, parseErr := time.Parse(time.RFC3339, event.At)
		if parseErr != nil || eventAt.Format("2006-01") != at.Format("2006-01") {
			return fmt.Errorf("同一事务 batch 必须落入同一月度事件文件")
		}
		raw, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			return marshalErr
		}
		line = append(line, raw...)
		line = append(line, '\n')
	}
	oldHash := genesisEventHash
	oldSequence := int64(0)
	if len(previous) > 0 {
		oldHash = previous[len(previous)-1].EventHash
		oldSequence = previous[len(previous)-1].Sequence
	}
	transactionID := strings.NewReplacer(":", "-", "/", "-").Replace(batchEvents[len(batchEvents)-1].ID)
	journal := transactionJournal{
		Version: 1, TransactionID: transactionID, LogFile: logFile,
		OldLogLength: int64(len(oldBytes)), OldLogDigest: digestBytes(oldBytes),
		OldSequence: oldSequence, OldEventHash: oldHash,
		Batch: line, BatchDigest: digestBytes(line),
		StateTempFile: ".state-" + transactionID + ".tmp",
	}
	journalPath, err := s.writeIntentLocked(txnDir, journal)
	if err != nil {
		return err
	}
	if err := s.appendJournalBatchLocked(logPath, oldBytes, line, existed); err != nil {
		return err
	}
	if err := s.writeStateLocked(snapshot, journal.StateTempFile); err != nil {
		return err
	}
	if err := s.hit("journal_cleanup"); err != nil {
		return err
	}
	if err := os.Remove(journalPath); err != nil {
		return err
	}
	if err := s.hit("journal_cleanup_parent_fsync"); err != nil {
		return err
	}
	return syncDir(txnDir)
}

func (s *Store) writeIntentLocked(txnDir string, journal transactionJournal) (string, error) {
	raw, err := json.Marshal(journal)
	if err != nil {
		return "", err
	}
	tmpPath := filepath.Join(txnDir, journal.TransactionID+".tmp")
	finalPath := filepath.Join(txnDir, journal.TransactionID+".json")
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		file.Close()
		return "", err
	}
	if err := s.hit("journal_intent_write"); err != nil {
		file.Close()
		return "", err
	}
	if err := s.hit("journal_intent_fsync"); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return "", err
	}
	if err := s.hit("journal_intent_rename"); err != nil {
		return "", err
	}
	if err := s.hit("journal_parent_fsync"); err != nil {
		return "", err
	}
	if err := syncDir(txnDir); err != nil {
		return "", err
	}
	return finalPath, nil
}

func (s *Store) appendJournalBatchLocked(logPath string, oldBytes, batch []byte, existed bool) error {
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil {
		return err
	} else if info.Size() != int64(len(oldBytes)) {
		return fmt.Errorf("事件日志长度在事务内变化：%s", logPath)
	}
	if err := s.hit("log_append_partial"); err != nil {
		partial := len(batch) / 2
		if partial == 0 {
			partial = 1
		}
		_, _ = file.WriteAt(batch[:partial], int64(len(oldBytes)))
		return err
	}
	if _, err := file.WriteAt(batch, int64(len(oldBytes))); err != nil {
		return err
	}
	if err := s.hit("log_file_fsync"); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if !existed {
		if err := s.hit("log_parent_fsync"); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(logPath)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writeStateLocked(snapshot Snapshot, tempBase string) error {
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := filepath.Join(s.Dir, tempBase)
	_ = os.Remove(tmpPath)
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := s.hit("state_temp_write"); err != nil {
		file.Close()
		return err
	}
	if err := s.hit("state_temp_fsync"); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(s.Dir, "state.json")); err != nil {
		return err
	}
	if err := s.hit("state_rename"); err != nil {
		return err
	}
	if err := s.hit("state_parent_fsync"); err != nil {
		return err
	}
	return syncDir(s.Dir)
}

func (s *Store) recoverLocked(cfg Config) error {
	txnDir := filepath.Join(s.Dir, "txn")
	if info, statErr := os.Lstat(txnDir); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("txn 必须是非 symlink 目录：%s", txnDir)
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	entries, err := os.ReadDir(txnDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("txn 目录含非法条目：%s", entry.Name())
		}
		if strings.HasSuffix(entry.Name(), ".tmp") {
			return fmt.Errorf("txn intent 不完整，拒绝自动恢复：%s", filepath.Join(txnDir, entry.Name()))
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			return fmt.Errorf("txn 目录含未知文件：%s", entry.Name())
		}
		if err := s.recoverJournalLocked(cfg, filepath.Join(txnDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) recoverJournalLocked(cfg Config, journalPath string) error {
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		return err
	}
	var journal transactionJournal
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return fmt.Errorf("txn intent 损坏 %s：%w", journalPath, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return fmt.Errorf("txn intent 多值 %s：%w", journalPath, err)
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
	if journal.OldLogLength < 0 || int64(len(actual)) < journal.OldLogLength ||
		int64(len(actual)) > journal.OldLogLength+int64(len(journal.Batch)) {
		return fmt.Errorf("txn 恢复时日志长度与 intent 不匹配：%s", logPath)
	}
	prefix := actual[:journal.OldLogLength]
	if digestBytes(prefix) != journal.OldLogDigest {
		return fmt.Errorf("txn 恢复时旧日志 digest 不匹配：%s", logPath)
	}
	_, oldLedger, err := s.readLedgerUnlocked(cfg, logPath, journal.OldLogLength)
	if err != nil {
		return fmt.Errorf("txn 恢复前旧账本无效：%w", err)
	}
	oldHash := oldLedger.snapshot.LastEventHash
	if oldHash == "" {
		oldHash = genesisEventHash
	}
	if oldLedger.snapshot.LastSequence != journal.OldSequence || oldHash != journal.OldEventHash {
		return fmt.Errorf("txn 恢复旧尾证据不匹配：sequence/hash")
	}
	suffix := actual[journal.OldLogLength:]
	if !bytes.Equal(suffix, journal.Batch[:len(suffix)]) {
		return fmt.Errorf("txn 恢复时日志后缀不是 intent batch 前缀：%s", logPath)
	}
	if len(suffix) < len(journal.Batch) {
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := file.WriteAt(journal.Batch[len(suffix):], int64(len(actual)))
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := syncDir(filepath.Dir(logPath)); err != nil {
			return err
		}
	}
	_, ledger, err := s.readLedgerUnlocked(cfg, "", 0)
	if err != nil {
		return fmt.Errorf("txn 恢复后账本无效：%w", err)
	}
	tmpPath := filepath.Join(s.Dir, journal.StateTempFile)
	if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := s.writeStateLocked(ledger.snapshot, journal.StateTempFile); err != nil {
		return err
	}
	if err := os.Remove(journalPath); err != nil {
		return err
	}
	return syncDir(filepath.Dir(journalPath))
}
