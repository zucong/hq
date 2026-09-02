package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const sessionEventVersion = 1

const (
	sessionStarted              = "started"
	sessionStopped              = "stopped"
	sessionHibernateAttempting  = "hibernate_attempting"
	sessionHibernateDeferred    = "hibernate_deferred"
	sessionHibernateFailed      = "hibernate_failed"
	sessionHibernateUnknown     = "hibernate_unknown"
	sessionFallbackAttempting   = "fallback_attempting"
	sessionFallbackFailed       = "fallback_failed"
	sessionFallbackUnknown      = "fallback_unknown"
	sessionFallbackRecoverySent = "fallback_recovery_sent"
)

type SessionEvent struct {
	Version            int    `json:"version"`
	EventID            string `json:"event_id"`
	SessionID          string `json:"session_id"`
	TabID              string `json:"tab_id"`
	PaneID             string `json:"pane_id"`
	TerminalID         string `json:"terminal_id,omitempty"`
	WorkspaceID        string `json:"workspace_id"`
	Agent              string `json:"agent"`
	Department         string `json:"department"`
	ReportsTo          string `json:"reports_to"`
	Type               string `json:"type"`
	At                 string `json:"time"`
	Actor              string `json:"actor"`
	Reason             string `json:"reason"`
	CWD                string `json:"cwd"`
	RuntimeKind        string `json:"runtime_kind,omitempty"`
	AgentSessionSource string `json:"agent_session_source,omitempty"`
	AgentSessionAgent  string `json:"agent_session_agent,omitempty"`
	AgentSessionKind   string `json:"agent_session_kind,omitempty"`
	AgentSessionValue  string `json:"agent_session_value,omitempty"`
	Revision           uint64 `json:"revision,omitempty"`
}

type SessionFilter struct {
	SessionID string
	Agent     string
	Type      string
}

type SessionStore interface {
	Append(SessionEvent) error
	List(SessionFilter) ([]SessionEvent, error)
}

type FileSessionStore struct {
	Root      string
	Failpoint func(string) error
	Context   context.Context
}

func (s *FileSessionStore) withRequestContext(ctx context.Context) *FileSessionStore {
	if s == nil {
		return nil
	}
	return &FileSessionStore{Root: s.Root, Failpoint: s.Failpoint, Context: nonNilContext(ctx)}
}

func (s *FileSessionStore) Append(event SessionEvent) error {
	if err := validateSessionEvent(event); err != nil {
		return err
	}
	if err := mkdirDurable(s.Root, 0o755); err != nil {
		return err
	}
	lockPath := filepath.Join(s.Root, ".session.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if info, err := lock.Stat(); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("session lock 必须是普通文件：%s", lockPath)
	}
	if err := flockContext(nonNilContext(s.Context), int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	events, err := s.listUnlocked()
	if err != nil {
		return err
	}
	for _, existing := range events {
		if existing.EventID != event.EventID {
			continue
		}
		if existing == event {
			return nil
		}
		return fmt.Errorf("重复 session event_id 内容冲突：%s", event.EventID)
	}
	if err := validateSessionTransition(events, event); err != nil {
		return err
	}
	if err := s.hit("before_append"); err != nil {
		return err
	}
	at, _ := time.Parse(time.RFC3339, event.At)
	path := filepath.Join(s.Root, at.Format("2006-01")+".jsonl")
	_, statErr := os.Lstat(path)
	newMonth := os.IsNotExist(statErr)
	if statErr != nil && !newMonth {
		return statErr
	}
	if !newMonth {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("session 月文件必须是非 symlink 普通文件：%s", path)
		}
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	writeErr := error(nil)
	if _, err := file.Write(append(raw, '\n')); err != nil {
		writeErr = err
	} else if err := file.Sync(); err != nil {
		writeErr = err
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := s.hit("after_file_fsync"); err != nil {
		// The event may already be durable. Strict replay resolves this crash
		// window idempotently instead of appending a duplicate on retry.
		replayed, replayErr := s.listUnlocked()
		if replayErr == nil {
			for _, existing := range replayed {
				if existing == event {
					if newMonth {
						if syncErr := syncDir(s.Root); syncErr != nil {
							return syncErr
						}
					}
					return nil
				}
			}
		}
		return err
	}
	if newMonth {
		if err := syncDir(s.Root); err != nil {
			return err
		}
	}
	return nil
}

func (s *FileSessionStore) hit(name string) error {
	if s.Failpoint == nil {
		return nil
	}
	if err := s.Failpoint(name); err != nil {
		return fmt.Errorf("session failpoint %s: %w", name, err)
	}
	return nil
}

func (s *FileSessionStore) List(filter SessionFilter) ([]SessionEvent, error) {
	info, err := os.Lstat(s.Root)
	if os.IsNotExist(err) {
		return []SessionEvent{}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("sessions root 必须是非 symlink 目录：%s", s.Root)
	}
	// Append holds this file with LOCK_EX across strict replay and the physical
	// JSONL append. Readers must take LOCK_SH as well; otherwise another HQ
	// process can expose a temporarily truncated final line to strict replay.
	lockPath := filepath.Join(s.Root, ".session.lock")
	// A sessions root may legitimately predate the lock file (for example an
	// operator-restored ledger or a strict-replay fixture). Creating the lock is
	// safe because it is coordination metadata, not a session fact; O_NOFOLLOW
	// and the post-open mode check keep a hostile replacement fail closed.
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开 session shared lock：%w", err)
	}
	defer lock.Close()
	lockInfo, statErr := lock.Stat()
	if statErr != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("session lock 必须是普通文件：%s", lockPath)
	}
	if err := flockContext(nonNilContext(s.Context), int(lock.Fd()), syscall.LOCK_SH); err != nil {
		return nil, fmt.Errorf("取得 session shared lock：%w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	events, err := s.listUnlocked()
	if err != nil {
		return nil, err
	}
	result := make([]SessionEvent, 0, len(events))
	for _, event := range events {
		if filter.SessionID != "" && event.SessionID != filter.SessionID {
			continue
		}
		if filter.Agent != "" && event.Agent != filter.Agent {
			continue
		}
		if filter.Type != "" && event.Type != filter.Type {
			continue
		}
		result = append(result, event)
	}
	return result, nil
}

func (s *FileSessionStore) listUnlocked() ([]SessionEvent, error) {
	info, lstatErr := os.Lstat(s.Root)
	if os.IsNotExist(lstatErr) {
		return []SessionEvent{}, nil
	}
	if lstatErr != nil {
		return nil, lstatErr
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("sessions root 必须是非 symlink 目录：%s", s.Root)
	}
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Name() == ".session.lock" {
			continue
		}
		if !monthlyFilenamePattern.MatchString(entry.Name()) {
			return nil, fmt.Errorf("sessions 目录含非法月度文件名：%s:1", filepath.Join(s.Root, entry.Name()))
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("session 文件必须是非 symlink 普通文件：%s", filepath.Join(s.Root, entry.Name()))
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	var events []SessionEvent
	seenIDs := map[string]bool{}
	startedByID := map[string]SessionEvent{}
	stoppedByID := map[string]bool{}
	for _, name := range names {
		path := filepath.Join(s.Root, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("%s:1 空 session 文件", path)
		}
		if !utf8.Valid(raw) {
			return nil, fmt.Errorf("%s:1 session 文件不是 UTF-8", path)
		}
		if raw[len(raw)-1] != '\n' {
			return nil, fmt.Errorf("%s:%d session 末行截断", path, bytes.Count(raw, []byte{'\n'})+1)
		}
		lines := bytes.Split(raw, []byte{'\n'})
		for index, line := range lines[:len(lines)-1] {
			lineNo := index + 1
			if len(bytes.TrimSpace(line)) == 0 {
				return nil, fmt.Errorf("%s:%d 空 session 行", path, lineNo)
			}
			var event SessionEvent
			if err := decodeStrictJSON(line, &event); err != nil {
				return nil, fmt.Errorf("%s:%d session JSON 无效：%w", path, lineNo, err)
			}
			if err := validateSessionEvent(event); err != nil {
				return nil, fmt.Errorf("%s:%d session schema 无效：%w", path, lineNo, err)
			}
			at, _ := time.Parse(time.RFC3339, event.At)
			if at.Format("2006-01")+".jsonl" != name {
				return nil, fmt.Errorf("%s:%d session 时间与月份不符", path, lineNo)
			}
			if seenIDs[event.EventID] {
				return nil, fmt.Errorf("%s:%d 重复 session event_id：%s", path, lineNo, event.EventID)
			}
			seenIDs[event.EventID] = true
			switch event.Type {
			case sessionStarted:
				if _, exists := startedByID[event.SessionID]; exists {
					return nil, fmt.Errorf("%s:%d session 双 start：%s", path, lineNo, event.SessionID)
				}
				startedByID[event.SessionID] = event
			case sessionStopped:
				started, exists := startedByID[event.SessionID]
				if !exists {
					return nil, fmt.Errorf("%s:%d session stop-before-start：%s", path, lineNo, event.SessionID)
				}
				if stoppedByID[event.SessionID] {
					return nil, fmt.Errorf("%s:%d session 双 stop：%s", path, lineNo, event.SessionID)
				}
				if err := validateSessionPair(started, event); err != nil {
					return nil, fmt.Errorf("%s:%d session stop 身份关系无效：%w", path, lineNo, err)
				}
				stoppedByID[event.SessionID] = true
			case sessionHibernateAttempting, sessionHibernateDeferred, sessionHibernateFailed, sessionHibernateUnknown,
				sessionFallbackAttempting, sessionFallbackFailed, sessionFallbackUnknown, sessionFallbackRecoverySent:
				started, exists := startedByID[event.SessionID]
				if !exists || stoppedByID[event.SessionID] {
					return nil, fmt.Errorf("%s:%d session runtime 诊断必须引用 active start：%s", path, lineNo, event.SessionID)
				}
				if err := validateSessionPair(started, event); err != nil {
					return nil, fmt.Errorf("%s:%d session runtime 诊断身份关系无效：%w", path, lineNo, err)
				}
			}
			events = append(events, event)
		}
	}
	return events, nil
}

func validateSessionTransition(events []SessionEvent, event SessionEvent) error {
	var started SessionEvent
	stopped := false
	for _, existing := range events {
		if existing.SessionID != event.SessionID {
			continue
		}
		switch existing.Type {
		case sessionStarted:
			started = existing
		case sessionStopped:
			stopped = true
		}
	}
	if event.Type == sessionStarted && started.Type != "" {
		return fmt.Errorf("session 双 start：%s", event.SessionID)
	}
	if event.Type != sessionStarted && started.Type == "" {
		return fmt.Errorf("session stop-before-start：%s", event.SessionID)
	}
	if event.Type != sessionStarted && stopped {
		return fmt.Errorf("session 双 stop：%s", event.SessionID)
	}
	if event.Type != sessionStarted {
		return validateSessionPair(started, event)
	}
	return nil
}

func validateSessionPair(started, stopped SessionEvent) error {
	if started.SessionID != stopped.SessionID || started.TabID != stopped.TabID || started.PaneID != stopped.PaneID ||
		started.WorkspaceID != stopped.WorkspaceID || started.Agent != stopped.Agent || started.Department != stopped.Department ||
		started.ReportsTo != stopped.ReportsTo || started.CWD != stopped.CWD || started.TerminalID != stopped.TerminalID ||
		started.RuntimeKind != stopped.RuntimeKind ||
		started.AgentSessionSource != stopped.AgentSessionSource || started.AgentSessionAgent != stopped.AgentSessionAgent ||
		started.AgentSessionKind != stopped.AgentSessionKind || started.AgentSessionValue != stopped.AgentSessionValue ||
		started.Revision != stopped.Revision {
		return fmt.Errorf("session 派生事件必须逐字继承 started 的 runtime incarnation 与组织身份")
	}
	return nil
}

func validateSessionEvent(event SessionEvent) error {
	if event.Version != sessionEventVersion {
		return fmt.Errorf("未知 session version：%d", event.Version)
	}
	if event.Type != sessionStarted && event.Type != sessionStopped && event.Type != sessionHibernateAttempting && event.Type != sessionHibernateDeferred && event.Type != sessionHibernateFailed && event.Type != sessionHibernateUnknown && event.Type != sessionFallbackAttempting && event.Type != sessionFallbackFailed && event.Type != sessionFallbackUnknown && event.Type != sessionFallbackRecoverySent {
		return fmt.Errorf("session type 必须是 started|stopped|hibernate_attempting|hibernate_deferred|hibernate_failed|hibernate_unknown|fallback_attempting|fallback_failed|fallback_unknown|fallback_recovery_sent")
	}
	if _, err := time.Parse(time.RFC3339, event.At); err != nil {
		return fmt.Errorf("session time 必须是 RFC3339：%w", err)
	}
	fields := []struct {
		name     string
		value    string
		required bool
	}{
		{"event_id", event.EventID, true}, {"session_id", event.SessionID, true},
		{"tab_id", event.TabID, true}, {"pane_id", event.PaneID, true},
		{"terminal_id", event.TerminalID, false},
		{"workspace_id", event.WorkspaceID, true}, {"agent", event.Agent, true},
		{"department", event.Department, true}, {"reports_to", event.ReportsTo, false},
		{"actor", event.Actor, true}, {"reason", event.Reason, true}, {"cwd", event.CWD, true},
		{"runtime_kind", event.RuntimeKind, false},
		{"agent_session_source", event.AgentSessionSource, false}, {"agent_session_agent", event.AgentSessionAgent, false},
		{"agent_session_kind", event.AgentSessionKind, false}, {"agent_session_value", event.AgentSessionValue, false},
	}
	for _, field := range fields {
		if field.required && strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("session 缺少 %s", field.name)
		}
		if strings.ContainsAny(field.value, "\r\n") || utf8.RuneCountInString(field.value) > 200 {
			return fmt.Errorf("session 字段 %s 必须是至多 200 rune 的单行文本", field.name)
		}
	}
	sessionIdentityFields := []string{event.AgentSessionSource, event.AgentSessionAgent, event.AgentSessionKind, event.AgentSessionValue}
	hasSessionIdentity := false
	for _, value := range sessionIdentityFields {
		hasSessionIdentity = hasSessionIdentity || value != ""
	}
	if hasSessionIdentity {
		for _, value := range sessionIdentityFields {
			if value == "" {
				return fmt.Errorf("agent_session incarnation 字段必须全有或全无")
			}
		}
	}
	return nil
}

func newSessionEvent(now time.Time, eventType string, created HerdrTabCreated, workspaceID string, rule AgentRule, actor, reason, cwd string) (SessionEvent, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return SessionEvent{}, err
	}
	event := SessionEvent{
		Version: sessionEventVersion, EventID: "SESSION-" + strings.ToUpper(hex.EncodeToString(random)),
		SessionID: created.Tab.ID, TabID: created.Tab.ID, PaneID: created.Pane.ID,
		WorkspaceID: workspaceID, Agent: rule.Name, Department: rule.Department,
		ReportsTo: rule.ReportsTo, Type: eventType, At: now.UTC().Format(time.RFC3339),
		Actor: actor, Reason: reason, CWD: cwd,
	}
	return event, validateSessionEvent(event)
}

func bindSessionEventRuntime(event SessionEvent, binding LiveBinding) (SessionEvent, error) {
	if event.TabID != binding.TabID || event.PaneID != binding.PaneID || event.WorkspaceID != binding.WorkspaceID || event.Agent != binding.Seat {
		return SessionEvent{}, fmt.Errorf("session event 与 live binding 的 workspace/tab/pane/seat 不一致")
	}
	event.TerminalID = binding.TerminalID
	event.RuntimeKind = binding.Kind
	event.Revision = binding.Revision
	if binding.AgentSession != nil {
		event.AgentSessionSource = binding.AgentSession.Source
		event.AgentSessionAgent = binding.AgentSession.Agent
		event.AgentSessionKind = binding.AgentSession.Kind
		event.AgentSessionValue = binding.AgentSession.Value
	}
	return event, validateSessionEvent(event)
}

func newDerivedSessionEvent(now time.Time, eventType string, started SessionEvent, actor, reason string) (SessionEvent, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return SessionEvent{}, err
	}
	event := started
	event.EventID = "SESSION-" + strings.ToUpper(hex.EncodeToString(random))
	event.Type = eventType
	event.At = now.UTC().Format(time.RFC3339)
	event.Actor = actor
	event.Reason = reason
	return event, validateSessionEvent(event)
}

func activeSessionStarts(events []SessionEvent) []SessionEvent {
	started := make(map[string]SessionEvent)
	stopped := make(map[string]bool)
	for _, event := range events {
		switch event.Type {
		case sessionStarted:
			started[event.SessionID] = event
		case sessionStopped:
			stopped[event.SessionID] = true
		}
	}
	result := make([]SessionEvent, 0, len(started))
	for sessionID, event := range started {
		if !stopped[sessionID] {
			result = append(result, event)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].At == result[j].At {
			return result[i].SessionID < result[j].SessionID
		}
		return result[i].At < result[j].At
	})
	return result
}

func latestSessionDiagnostic(events []SessionEvent, sessionID string) SessionEvent {
	var latest SessionEvent
	for _, event := range events {
		if event.SessionID == sessionID && (event.Type == sessionHibernateAttempting || event.Type == sessionHibernateDeferred || event.Type == sessionHibernateFailed || event.Type == sessionHibernateUnknown || event.Type == sessionFallbackAttempting || event.Type == sessionFallbackFailed || event.Type == sessionFallbackUnknown || event.Type == sessionFallbackRecoverySent) {
			latest = event
		}
	}
	return latest
}

func (a *App) cmdSession(args []string) error {
	if len(args) == 0 || args[0] != "list" {
		return fmt.Errorf("用法：hq session list [--session ID] [--agent NAME] [--type started|stopped|hibernate_*|fallback_*]")
	}
	fs := newLeafParser("session list")
	fs.SetOutput(a.Err)
	sessionID := fs.String("session", "", "按 session id 过滤")
	agent := fs.String("agent", "", "按 agent 过滤")
	eventType := fs.String("type", "", "按 started|stopped|hibernate_*|fallback_* 过滤")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || (*eventType != "" && *eventType != sessionStarted && *eventType != sessionStopped && *eventType != sessionHibernateAttempting && *eventType != sessionHibernateDeferred && *eventType != sessionHibernateFailed && *eventType != sessionHibernateUnknown && *eventType != sessionFallbackAttempting && *eventType != sessionFallbackFailed && *eventType != sessionFallbackUnknown && *eventType != sessionFallbackRecoverySent) {
		return fmt.Errorf("用法：hq session list [--session ID] [--agent NAME] [--type started|stopped|hibernate_*|fallback_*]")
	}
	if a.Sessions == nil {
		return fmt.Errorf("session store 未注入")
	}
	events, err := a.Sessions.List(SessionFilter{SessionID: *sessionID, Agent: *agent, Type: *eventType})
	if err != nil {
		return err
	}
	if a.JSON {
		encoder := json.NewEncoder(a.Out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(events)
	}
	for _, event := range events {
		if _, err := fmt.Fprintf(a.Out, "%s %s %s agent=%s workspace=%s tab=%s pane=%s actor=%s reason=%s cwd=%s\n", event.At, event.Type, event.SessionID, event.Agent, event.WorkspaceID, event.TabID, event.PaneID, event.Actor, event.Reason, event.CWD); err != nil {
			return err
		}
	}
	return nil
}
