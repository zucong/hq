package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type SQLiteRunner interface {
	Run(databasePath, fixedScript string) ([]byte, error)
}

type execSQLiteRunner struct {
	Path string
}

func (r execSQLiteRunner) Run(databasePath, fixedScript string) ([]byte, error) {
	if !filepath.IsAbs(r.Path) {
		return nil, fmt.Errorf("sqlite3 runner 必须是绝对路径，禁止 PATH 回落")
	}
	info, err := os.Lstat(r.Path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("sqlite3 runner 必须是可执行的非 symlink 普通文件：%s", r.Path)
	}
	command := exec.Command(r.Path, "-batch", "-bail", databasePath)
	command.Stdin = strings.NewReader(fixedScript)
	command.Env = []string{"LC_ALL=C", "LANG=C"}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("受控 sqlite3 runner 失败：%w：%s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

type DerivedIndex struct {
	Path      string
	Runner    SQLiteRunner
	Failpoint func(string) error
}

type documentIndexRow struct {
	Path       string
	Category   string
	Size       int64
	ModifiedNS int64
}

type IndexQuery struct {
	Entity    string
	CaseID    string
	EventType string
	Actor     string
	Recipient string
	Status    string
	TimeFrom  string
	TimeTo    string
	Path      string
}

type IndexRebuildResult struct {
	Path       string `json:"path"`
	Documents  int    `json:"documents"`
	FlowEvents int    `json:"flow_events"`
	Cases      int    `json:"cases"`
	Deliveries int    `json:"deliveries"`
}

func (d *DerivedIndex) hit(name string) error {
	if d.Failpoint == nil {
		return nil
	}
	if err := d.Failpoint(name); err != nil {
		return fmt.Errorf("index failpoint %s: %w", name, err)
	}
	return nil
}

func (d *DerivedIndex) lock() (*os.File, error) {
	if d == nil || d.Runner == nil || d.Path == "" || !filepath.IsAbs(d.Path) {
		return nil, fmt.Errorf("派生索引必须显式注入绝对路径与 sqlite3 runner")
	}
	parent := filepath.Dir(d.Path)
	info, err := os.Lstat(parent)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("index 父目录必须是非 symlink 目录：%s", parent)
	}
	lockPath := d.Path + ".rebuild.lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if info, err := lock.Stat(); err != nil || !info.Mode().IsRegular() {
		lock.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("index 锁必须是普通文件：%s", lockPath)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("index 正在重建，拒绝并发运行")
	}
	return lock, nil
}

func collectMarkdownMetadata(root string) ([]documentIndexRow, error) {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, err
	}
	rows := []documentIndexRow{}
	err = filepath.WalkDir(canonical, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(canonical, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if rel != "." && (name == ".git" || name == "records" || name == "bin" || name == "node_modules" || name == "__pycache__") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.ToLower(filepath.Ext(entry.Name())) != ".md" {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("索引原文必须是非 symlink 普通 Markdown：%s", path)
		}
		if containsSensitive(path) {
			return fmt.Errorf("索引原文路径疑似敏感：%s", path)
		}
		rows = append(rows, documentIndexRow{Path: path, Category: documentCategory(filepath.ToSlash(rel)), Size: info.Size(), ModifiedNS: info.ModTime().UnixNano()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	return rows, nil
}

func documentCategory(relative string) string {
	switch {
	case strings.Contains(relative, "/decisions/") || strings.HasPrefix(relative, "ceo-office/decisions/"):
		return "decision"
	case strings.Contains(relative, "/tickets/") || strings.HasPrefix(relative, "product/tickets/"):
		return "ticket"
	case strings.Contains(relative, "/reports/") || strings.Contains(relative, "/findings/"):
		return "qa-report"
	case strings.HasSuffix(relative, "DESIGN.md") || strings.Contains(relative, "/org/") || strings.Contains(relative, "/notes/"):
		return "design-or-evidence"
	case strings.HasSuffix(relative, "AGENTS.md") || strings.Contains(relative, "/roles/") || strings.HasSuffix(relative, "/goals.md"):
		return "manual"
	default:
		return "markdown"
	}
}

func validateIndexReferences(events []Event, hqRoot string) error {
	for _, event := range events {
		for _, ref := range []struct {
			name  string
			value string
		}{{"source_ref", event.SourceRef}, {"artifact_ref", event.ArtifactRef}, {"approval_ref", event.ApprovalRef}, {"resolution_ref", event.ResolutionRef}} {
			if ref.value == "" {
				continue
			}
			canonical, err := normalizeRef(ref.value, hqRoot, true)
			if err != nil {
				return fmt.Errorf("event=%s %s 无效：%w", event.ID, ref.name, err)
			}
			if canonical != ref.value {
				return fmt.Errorf("event=%s %s 不是 canonical 引用：%s", event.ID, ref.name, ref.value)
			}
		}
	}
	return nil
}

func sqlText(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func buildIndexSQL(documents []documentIndexRow, events []Event, ledger *ledgerState) string {
	var script strings.Builder
	script.WriteString("PRAGMA journal_mode=DELETE;\nPRAGMA synchronous=FULL;\nPRAGMA foreign_keys=ON;\nBEGIN IMMEDIATE;\n")
	script.WriteString("CREATE TABLE documents(path TEXT PRIMARY KEY, category TEXT NOT NULL, size INTEGER NOT NULL, modified_ns INTEGER NOT NULL);\n")
	script.WriteString("CREATE TABLE flow_events(sequence INTEGER PRIMARY KEY CHECK(sequence > 0), event_id TEXT NOT NULL UNIQUE, case_id TEXT NOT NULL, at TEXT NOT NULL, event_type TEXT NOT NULL, actor TEXT NOT NULL, recipient TEXT, status TEXT, result TEXT, source_ref TEXT, artifact_ref TEXT, delivery_id TEXT);\n")
	script.WriteString("CREATE TABLE cases(case_id TEXT PRIMARY KEY, title TEXT NOT NULL, status TEXT NOT NULL, owner TEXT, source_ref TEXT, updated_at TEXT NOT NULL, last_event_id TEXT NOT NULL);\n")
	script.WriteString("CREATE TABLE deliveries(delivery_id TEXT PRIMARY KEY, case_id TEXT NOT NULL, origin_event_id TEXT NOT NULL, origin_type TEXT NOT NULL, actor TEXT NOT NULL, recipient TEXT NOT NULL, status TEXT NOT NULL, internal_status TEXT NOT NULL, acked_by TEXT, ack_event_id TEXT, payload_digest TEXT NOT NULL, attempt_count INTEGER NOT NULL);\n")
	for _, row := range documents {
		fmt.Fprintf(&script, "INSERT INTO documents VALUES(%s,%s,%d,%d);\n", sqlText(row.Path), sqlText(row.Category), row.Size, row.ModifiedNS)
	}
	for _, event := range events {
		fmt.Fprintf(&script, "INSERT INTO flow_events VALUES(%d,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s);\n",
			event.Sequence, sqlText(event.ID), sqlText(event.CaseID), sqlText(event.At), sqlText(event.Type), sqlText(event.Actor),
			sqlText(event.Recipient), sqlText(event.ToState), sqlText(event.Result), sqlText(event.SourceRef), sqlText(event.ArtifactRef), sqlText(event.DeliveryID))
	}
	caseIDs := make([]string, 0, len(ledger.snapshot.Cases))
	for caseID := range ledger.snapshot.Cases {
		caseIDs = append(caseIDs, caseID)
	}
	sort.Strings(caseIDs)
	for _, caseID := range caseIDs {
		state := ledger.snapshot.Cases[caseID]
		fmt.Fprintf(&script, "INSERT INTO cases VALUES(%s,%s,%s,%s,%s,%s,%s);\n", sqlText(state.ID), sqlText(state.Title), sqlText(state.Status), sqlText(state.Owner), sqlText(state.SourceRef), sqlText(state.UpdatedAt), sqlText(state.LastEventID))
	}
	for _, view := range ledger.deliveryViews() {
		record := ledger.deliveries[view.DeliveryID]
		fmt.Fprintf(&script, "INSERT INTO deliveries VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%d);\n",
			sqlText(view.DeliveryID), sqlText(view.CaseID), sqlText(view.OriginEventID), sqlText(view.OriginType), sqlText(record.Origin.Actor), sqlText(view.Target),
			sqlText(view.ProjectionStatus), sqlText(view.Status), sqlText(view.AckedBy), sqlText(view.AckEventID), sqlText(view.PayloadDigest), view.AttemptCount)
	}
	script.WriteString("CREATE INDEX flow_events_case_sequence ON flow_events(case_id, sequence);\n")
	script.WriteString("CREATE INDEX flow_events_type ON flow_events(event_type);\nCREATE INDEX flow_events_actor_recipient ON flow_events(actor, recipient);\nCREATE INDEX flow_events_status_time ON flow_events(status, at);\nCREATE INDEX flow_events_source ON flow_events(source_ref);\nCREATE INDEX flow_events_artifact ON flow_events(artifact_ref);\n")
	script.WriteString("CREATE INDEX cases_status ON cases(status);\nCREATE INDEX deliveries_case_status ON deliveries(case_id, status);\nCREATE INDEX documents_category ON documents(category);\nCOMMIT;\nPRAGMA quick_check;\n")
	return script.String()
}

func (d *DerivedIndex) Rebuild(store EventStore, cfg Config, hqRoot string) (result IndexRebuildResult, err error) {
	lock, err := d.lock()
	if err != nil {
		return result, err
	}
	defer unlock(lock)
	events, err := store.ReadAll(cfg)
	if err != nil {
		return result, err
	}
	ledger, err := validateLedger(events, cfg)
	if err != nil {
		return result, err
	}
	if err := validateIndexReferences(events, hqRoot); err != nil {
		return result, err
	}
	documents, err := collectMarkdownMetadata(hqRoot)
	if err != nil {
		return result, err
	}
	oldBytes, oldExists, err := readRegularFileIfExists(d.Path)
	if err != nil {
		return result, err
	}
	temp, err := os.CreateTemp(filepath.Dir(d.Path), ".hq-index-*.tmp")
	if err != nil {
		return result, err
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		os.Remove(tempPath)
		return result, err
	}
	defer func() { _ = os.Remove(tempPath) }()
	if err := d.hit("before_runner"); err != nil {
		return result, err
	}
	if _, err := d.Runner.Run(tempPath, buildIndexSQL(documents, events, ledger)); err != nil {
		return result, err
	}
	if err := d.hit("after_runner"); err != nil {
		return result, err
	}
	file, err := os.OpenFile(tempPath, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return result, err
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		file.Close()
		if statErr != nil {
			return result, statErr
		}
		return result, fmt.Errorf("sqlite3 runner 未生成非空普通数据库")
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return result, err
	}
	if err := file.Close(); err != nil {
		return result, err
	}
	if err := d.hit("after_file_fsync"); err != nil {
		return result, err
	}
	if err := d.hit("before_rename"); err != nil {
		return result, err
	}
	if err := os.Rename(tempPath, d.Path); err != nil {
		return result, err
	}
	restore := func(cause error) error {
		if restoreErr := restoreOldIndex(d.Path, oldBytes, oldExists); restoreErr != nil {
			return fmt.Errorf("%v；旧 index 恢复失败：%w", cause, restoreErr)
		}
		return cause
	}
	if err := d.hit("after_rename"); err != nil {
		return result, restore(err)
	}
	if err := syncDir(filepath.Dir(d.Path)); err != nil {
		return result, restore(err)
	}
	if err := d.hit("after_parent_fsync"); err != nil {
		return result, restore(err)
	}
	return IndexRebuildResult{Path: d.Path, Documents: len(documents), FlowEvents: len(events), Cases: len(ledger.snapshot.Cases), Deliveries: len(ledger.deliveries)}, nil
}

func restoreOldIndex(path string, old []byte, existed bool) error {
	parent := filepath.Dir(path)
	if !existed {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return syncDir(parent)
	}
	temp, err := os.CreateTemp(parent, ".hq-index-restore-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(old); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return syncDir(parent)
}

func (d *DerivedIndex) Query(filter IndexQuery) ([]map[string]any, error) {
	if d == nil || d.Runner == nil {
		return nil, fmt.Errorf("派生索引未注入")
	}
	info, err := os.Lstat(d.Path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("index 必须是非 symlink 普通文件")
	}
	query, err := structuredIndexSQL(filter)
	if err != nil {
		return nil, err
	}
	raw, err := d.Runner.Run(d.Path, ".mode json\n.headers on\n"+query+";\n")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return []map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var rows []map[string]any
	if err := decoder.Decode(&rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func structuredIndexSQL(filter IndexQuery) (string, error) {
	entity := filter.Entity
	if entity == "" {
		entity = "flow_events"
	}
	var selectClause string
	switch entity {
	case "flow_events":
		selectClause = "SELECT CAST(sequence AS TEXT) AS sequence,event_id,case_id,at,event_type,actor,recipient,status,result,source_ref,artifact_ref,delivery_id FROM flow_events"
	case "cases":
		selectClause = "SELECT case_id,title,status,owner,source_ref,updated_at,last_event_id FROM cases"
	case "deliveries":
		selectClause = "SELECT delivery_id,case_id,origin_event_id,origin_type,actor,recipient,status,internal_status,acked_by,ack_event_id,payload_digest,attempt_count FROM deliveries"
	case "documents":
		selectClause = "SELECT path,category,size,modified_ns FROM documents"
	default:
		return "", fmt.Errorf("entity 只能是 flow_events|cases|deliveries|documents")
	}
	conditions := []string{}
	add := func(column, value string) {
		if value != "" {
			conditions = append(conditions, column+"="+sqlText(value))
		}
	}
	switch entity {
	case "flow_events":
		add("case_id", filter.CaseID)
		add("event_type", filter.EventType)
		add("actor", filter.Actor)
		add("recipient", filter.Recipient)
		add("status", filter.Status)
		if filter.TimeFrom != "" {
			conditions = append(conditions, "at>="+sqlText(filter.TimeFrom))
		}
		if filter.TimeTo != "" {
			conditions = append(conditions, "at<="+sqlText(filter.TimeTo))
		}
		if filter.Path != "" {
			conditions = append(conditions, "(source_ref="+sqlText(filter.Path)+" OR artifact_ref="+sqlText(filter.Path)+")")
		}
	case "cases":
		add("case_id", filter.CaseID)
		add("status", filter.Status)
		add("source_ref", filter.Path)
	case "deliveries":
		add("case_id", filter.CaseID)
		add("actor", filter.Actor)
		add("recipient", filter.Recipient)
		add("status", filter.Status)
	case "documents":
		add("path", filter.Path)
	}
	if len(conditions) > 0 {
		selectClause += " WHERE " + strings.Join(conditions, " AND ")
	}
	order := map[string]string{"flow_events": "sequence", "cases": "case_id", "deliveries": "delivery_id", "documents": "path"}[entity]
	return selectClause + " ORDER BY " + order, nil
}

func (a *App) cmdIndex(args []string) error {
	if a.Index == nil {
		return fmt.Errorf("派生索引未显式注入；拒绝回落正式 index/sqlite3")
	}
	if len(args) == 0 {
		return fmt.Errorf("用法：hq index rebuild|query")
	}
	switch args[0] {
	case "rebuild":
		if len(args) != 1 {
			return fmt.Errorf("用法：hq index rebuild")
		}
		result, err := a.Index.Rebuild(a.Store, a.Config, a.HQRoot)
		if err != nil {
			return err
		}
		return a.output(result, fmt.Sprintf("已重建派生索引：documents=%d events=%d cases=%d deliveries=%d", result.Documents, result.FlowEvents, result.Cases, result.Deliveries))
	case "query":
		fs := newLeafParser("index query")
		fs.SetOutput(a.Err)
		filter := IndexQuery{}
		fs.StringVar(&filter.Entity, "entity", "flow_events", "flow_events|cases|deliveries|documents")
		fs.StringVar(&filter.CaseID, "case", "", "case_id")
		fs.StringVar(&filter.EventType, "type", "", "event type")
		fs.StringVar(&filter.Actor, "actor", "", "actor")
		fs.StringVar(&filter.Recipient, "recipient", "", "recipient")
		fs.StringVar(&filter.Status, "status", "", "status")
		fs.StringVar(&filter.TimeFrom, "from", "", "RFC3339 lower bound")
		fs.StringVar(&filter.TimeTo, "to", "", "RFC3339 upper bound")
		fs.StringVar(&filter.Path, "path", "", "exact canonical source/artifact/document path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("index query 不接受位置参数或任意 SQL")
		}
		rows, err := a.Index.Query(filter)
		if err != nil {
			return err
		}
		return a.output(rows, fmt.Sprintf("%d rows", len(rows)))
	default:
		return fmt.Errorf("未知 index 子命令 %q", args[0])
	}
}
