package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"
	"unicode/utf8"
)

type locatedEvent struct {
	event Event
	path  string
	line  int
}

func (s *Store) readLedgerUnlocked(cfg Config, limitPath string, limit int64) ([]Event, *ledgerState, error) {
	eventsDir := filepath.Join(s.Dir, "events")
	if info, statErr := os.Lstat(eventsDir); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, nil, fmt.Errorf("events 必须是非 symlink 目录：%s", eventsDir)
		}
	} else if !os.IsNotExist(statErr) {
		return nil, nil, statErr
	}
	entries, err := os.ReadDir(eventsDir)
	if os.IsNotExist(err) {
		return nil, newLedgerState(), nil
	}
	if err != nil {
		return nil, nil, err
	}
	var located []locatedEvent
	for _, entry := range entries {
		if !monthlyFilenamePattern.MatchString(entry.Name()) {
			return nil, nil, fmt.Errorf("events 目录含非法月度文件名：%s:1", filepath.Join(eventsDir, entry.Name()))
		}
		info, err := entry.Info()
		if err != nil {
			return nil, nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("事件文件必须是非 symlink 普通文件：%s", entry.Name())
		}
		path := filepath.Join(eventsDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}
		if path == limitPath {
			if limit < 0 || limit > int64(len(raw)) {
				return nil, nil, fmt.Errorf("事务旧日志长度越界：%s", path)
			}
			raw = raw[:limit]
			if limit == 0 {
				continue
			}
		}
		fileEvents, err := parseEventFile(path, entry.Name(), raw)
		if err != nil {
			return nil, nil, err
		}
		located = append(located, fileEvents...)
	}
	sort.SliceStable(located, func(i, j int) bool { return located[i].event.Sequence < located[j].event.Sequence })
	ledger := newLedgerState()
	events := make([]Event, 0, len(located))
	for _, item := range located {
		if err := ledger.validateAndApply(item.event, cfg); err != nil {
			return nil, nil, fmt.Errorf("%s:%d 事件语义无效：%w", item.path, item.line, err)
		}
		events = append(events, item.event)
	}
	if err := ledger.validateLedgerFinalInvariants(cfg); err != nil {
		return nil, nil, fmt.Errorf("ledger 尾状态无效：%w", err)
	}
	return events, ledger, nil
}

func parseEventFile(path, filename string, raw []byte) ([]locatedEvent, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s:1 空事件文件", path)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%s:1 事件文件不是 UTF-8", path)
	}
	if raw[len(raw)-1] != '\n' {
		return nil, fmt.Errorf("%s:%d 末行截断且无合法事务恢复证据", path, bytes.Count(raw, []byte{'\n'})+1)
	}
	lines := bytes.Split(raw, []byte{'\n'})
	result := make([]locatedEvent, 0, len(lines)-1)
	var previousPhysicalSequence int64
	for index, line := range lines[:len(lines)-1] {
		lineNo := index + 1
		if len(bytes.TrimSpace(line)) == 0 {
			return nil, fmt.Errorf("%s:%d 空事件行", path, lineNo)
		}
		var event Event
		if err := decodeStrictEvent(line, &event); err != nil {
			return nil, fmt.Errorf("%s:%d 事件损坏：%w", path, lineNo, err)
		}
		at, err := time.Parse(time.RFC3339, event.At)
		if err != nil {
			return nil, fmt.Errorf("%s:%d 事件时间格式错误：%w", path, lineNo, err)
		}
		if at.Format("2006-01")+".jsonl" != filename {
			return nil, fmt.Errorf("%s:%d 事件时间与月度文件名不一致", path, lineNo)
		}
		if previousPhysicalSequence != 0 && event.Sequence <= previousPhysicalSequence {
			return nil, fmt.Errorf("%s:%d 月度文件内 sequence 物理顺序倒退或重复：%d <= %d", path, lineNo, event.Sequence, previousPhysicalSequence)
		}
		previousPhysicalSequence = event.Sequence
		result = append(result, locatedEvent{event: event, path: path, line: lineNo})
	}
	return result, nil
}

func (s *Store) readSnapshotStrict() (Snapshot, error) {
	raw, err := os.ReadFile(filepath.Join(s.Dir, "state.json"))
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return Snapshot{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Snapshot{}, err
	}
	if snapshot.Version != snapshotSchemaVersion || snapshot.Cases == nil {
		return Snapshot{}, fmt.Errorf("派生 state 版本或 cases 无效")
	}
	return snapshot, nil
}

func snapshotsEqual(left, right Snapshot) bool {
	return reflect.DeepEqual(left, right)
}

func (s *Store) Delivery(cfg Config, deliveryID string) (DeliveryView, bool, error) {
	releaseRegistry, err := s.lockCurrentRegistry(cfg)
	if err != nil {
		return DeliveryView{}, false, err
	}
	defer releaseRegistry()
	releaseLedger, err := s.lock()
	if err != nil {
		return DeliveryView{}, false, err
	}
	defer releaseLedger()
	if err := s.recoverLocked(cfg); err != nil {
		return DeliveryView{}, false, err
	}
	_, ledger, err := s.readLedgerUnlocked(cfg, "", 0)
	if err != nil {
		return DeliveryView{}, false, err
	}
	view, ok := ledger.deliveryView(deliveryID)
	return view, ok, nil
}

func (s *Store) Deliveries(cfg Config) ([]DeliveryView, error) {
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
	_, ledger, err := s.readLedgerUnlocked(cfg, "", 0)
	if err != nil {
		return nil, err
	}
	return ledger.deliveryViews(), nil
}
