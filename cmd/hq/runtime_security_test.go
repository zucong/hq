package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type countingStore struct {
	calls int
}

func (s *countingStore) Transact(Config, string, string, bool, TransactionBuilder) (TransactionResult, error) {
	s.calls++
	return TransactionResult{}, nil
}

func (s *countingStore) Append(Event, Config) error {
	s.calls++
	return nil
}

func (s *countingStore) Rebuild(Config) (Snapshot, error) {
	s.calls++
	return newSnapshot(), nil
}

func (s *countingStore) Snapshot(Config) (Snapshot, error) {
	s.calls++
	return newSnapshot(), nil
}

func (s *countingStore) ReadAll(Config) ([]Event, error) {
	s.calls++
	return nil, nil
}

func (s *countingStore) ReportAssignment(Config, string, string) (string, bool, error) {
	s.calls++
	return "", false, nil
}

func (s *countingStore) Delivery(Config, string) (DeliveryView, bool, error) {
	s.calls++
	return DeliveryView{}, false, nil
}

func (s *countingStore) Deliveries(Config) ([]DeliveryView, error) {
	s.calls++
	return nil, nil
}

func (s *countingStore) EventRef(Event) string {
	s.calls++
	return ""
}

func (s *countingStore) NowTime() time.Time {
	s.calls++
	return time.Unix(0, 0)
}

func injectedApp(t *testing.T, cfg Config, store EventStore, identity IdentityProvider, transport DeliveryTransport) *App {
	t.Helper()
	root := canonicalTestTempDir(t)
	office := filepath.Join(root, "ceo-office")
	if err := os.MkdirAll(office, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rule := range cfg.Agents {
		if err := os.MkdirAll(testRuleCWD(root, rule), 0o755); err != nil {
			t.Fatal(err)
		}
		manual := filepath.Join(root, rule.ManualPath)
		if err := os.MkdirAll(filepath.Dir(manual), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manual, testRoleManual(rule.Name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(office, "tools", "hq", "config.yaml")
	writeConfigFixture(t, configPath, cfg)
	app, err := newAppWithDependencies(runtimePaths{
		Office: office, HQRoot: root, DataDir: filepath.Join(office, "records"),
		ConfigPath: configPath, HerdrBin: filepath.Join(root, "must-not-run"),
	}, cfg, globalOptions{}, AppDependencies{Store: store, Identity: identity, Transport: transport}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func actorFor(cfg Config, name, pane, cwd string) Actor {
	rule, _ := configRuleIncludingDisabled(cfg, name)
	return Actor{Name: name, Label: rule.Label, Department: rule.Department, PaneID: pane, CWD: cwd, Rule: rule}
}

func TestDirectRejectsEveryBusinessMutationBeforeStoreOrTransport(t *testing.T) {
	cfg := testConfig()
	commands := [][]string{
		{"case", "create"}, {"report"}, {"issue"}, {"accept"}, {"return"}, {"close"},
		{"approval", "grant"}, {"message"},
		{"staff", "add"}, {"staff", "update"}, {"staff", "remove"},
	}
	for _, actorName := range []string{"penny", "zantianyou"} {
		for _, command := range commands {
			t.Run(actorName+"/"+strings.Join(command, "_"), func(t *testing.T) {
				store := &countingStore{}
				transport := &fakeTransport{}
				identity := &fakeIdentityProvider{actors: map[string]Actor{
					"pane": actorFor(cfg, actorName, "pane", "/test"),
				}}
				app := injectedApp(t, cfg, store, identity, transport)
				app.Direct = true
				err := app.run(command)
				if err == nil || !strings.Contains(err.Error(), "业务写只能经 gateway") {
					t.Fatalf("direct mutation should fail closed, got %v", err)
				}
				if store.calls != 0 || len(transport.calls) != 0 || identity.calls != 0 {
					t.Fatalf("failure had side effects: store=%d transport=%d identity=%d", store.calls, len(transport.calls), identity.calls)
				}
			})
		}
	}
}

func TestInjectedConstructorRequiresAllDependencies(t *testing.T) {
	cfg := testConfig()
	paths := runtimePaths{Office: "/test/ceo-office", HQRoot: "/test", DataDir: "/test/data", ConfigPath: "/test/config", HerdrBin: "must-not-run"}
	validStore := &countingStore{}
	validIdentity := &fakeIdentityProvider{actors: map[string]Actor{}}
	validTransport := &fakeTransport{}
	cases := []AppDependencies{
		{Identity: validIdentity, Transport: validTransport},
		{Store: validStore, Transport: validTransport},
		{Store: validStore, Identity: validIdentity},
	}
	for i, deps := range cases {
		if _, err := newAppWithDependencies(paths, cfg, globalOptions{}, deps, io.Discard, io.Discard); err == nil {
			t.Fatalf("case %d missing dependency was accepted", i)
		}
	}
}

func TestInjectedBusinessFlowNeverFallsBackToPathHerdr(t *testing.T) {
	cfg := testConfig()
	root := canonicalTestTempDir(t)
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	guardLog := filepath.Join(root, "path-herdr-called")
	guard := filepath.Join(binDir, "herdr")
	script := "#!/bin/sh\nprintf called > \"$HQ_PATH_HERDR_GUARD\"\nexit 99\n"
	if err := os.WriteFile(guard, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HQ_PATH_HERDR_GUARD", guardLog)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "pane")
	identity := &fakeIdentityProvider{actors: map[string]Actor{
		"pane": actorFor(cfg, "penny", "pane", testAgentCWD(cfg, root, "penny")),
	}}
	transport := &fakeTransport{}
	store := NewStore(filepath.Join(root, "records"))
	app := injectedApp(t, cfg, store, identity, transport)
	app.FromGateway = true
	source := writeTestFile(t, filepath.Join(app.HQRoot, "source.md"), "# source\n")
	if err := app.run([]string{"case", "create", "--id", "NO-PATH-HERDR-001", "--title", "isolated", "--project", "test-project", "--source", source}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(guardLog); !os.IsNotExist(err) {
		t.Fatalf("injected flow invoked PATH herdr: %v", err)
	}
}

func TestMaintenanceWhitelistUsesExactActiveManageStaffRule(t *testing.T) {
	cfg := testConfig()
	disabled := AgentRule{Name: "disabled-ops", Label: "disabled", Nickname: "disabled", DepartmentLabel: "总裁办", Workspace: cfg.WorkspaceLabel, Responsibilities: []string{"disabled_ops"}, ManualPath: "ceo-office/AGENTS.md", Department: "ceo-office", ReportsTo: "zantianyou", Disabled: true, CanManageStaff: true}
	cfg.Agents = append(cfg.Agents, disabled)
	cfg = bindTestRoleContracts(cfg)
	tests := []struct {
		name    string
		actor   string
		wantErr bool
	}{
		{name: "whitelisted", actor: "penny"},
		{name: "not_whitelisted", actor: "zantianyou", wantErr: true},
		{name: "disabled", actor: "disabled-ops", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(filepath.Join(canonicalTestTempDir(t), "records"))
			identity := &fakeIdentityProvider{actors: map[string]Actor{
				"pane": actorFor(cfg, tc.actor, "pane", "/test"),
			}}
			transport := &fakeTransport{}
			app := injectedApp(t, cfg, store, identity, transport)
			app.Direct, app.MaintenancePane = true, "pane"
			err := app.run([]string{"rebuild"})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "运维白名单") {
					t.Fatalf("expected whitelist failure, got %v", err)
				}
				if _, statErr := os.Stat(filepath.Join(store.Dir, "state.json")); !os.IsNotExist(statErr) {
					t.Fatalf("failed maintenance wrote state: %v", statErr)
				}
			} else if err != nil {
				t.Fatalf("whitelisted rebuild failed: %v", err)
			}
			if len(transport.calls) != 0 {
				t.Fatalf("rebuild must not deliver: %+v", transport.calls)
			}
		})
	}
}

func writeConfigFixture(t *testing.T, path string, cfg Config) {
	t.Helper()
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func productionOfficeFixture(t *testing.T) string {
	t.Helper()
	root := canonicalTestTempDir(t)
	office := filepath.Join(root, "ceo-office")
	if err := os.MkdirAll(filepath.Join(office, "records"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfigFixture(t, filepath.Join(office, "tools", "hq", "config.yaml"), testConfig())
	return office
}

func TestProductionRuntimeFixesCanonicalConfigAndData(t *testing.T) {
	office := productionOfficeFixture(t)
	paths, err := resolveProductionRuntime(globalOptions{Office: office})
	if err != nil {
		t.Fatal(err)
	}
	if paths.ConfigPath != filepath.Join(office, "tools", "hq", "config.yaml") || paths.DataDir != filepath.Join(office, "records") {
		t.Fatalf("production roots not fixed: %+v", paths)
	}
	for _, options := range []globalOptions{
		{Office: office, Data: filepath.Join(office, "other")},
		{Office: office, Config: filepath.Join(office, "other.json")},
		{Office: office, Herdr: filepath.Join(office, "fake")},
	} {
		if _, err := resolveProductionRuntime(options); err == nil {
			t.Fatalf("caller override was accepted: %+v", options)
		}
	}
}

func TestProductionRuntimeRejectsSymlinkAndNonRegularRoots(t *testing.T) {
	t.Run("office symlink", func(t *testing.T) {
		office := productionOfficeFixture(t)
		link := filepath.Join(filepath.Dir(office), "office-link")
		if err := os.Symlink(office, link); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveProductionRuntime(globalOptions{Office: link}); err == nil {
			t.Fatal("symlink office accepted")
		}
	})
	t.Run("config symlink", func(t *testing.T) {
		office := productionOfficeFixture(t)
		config := filepath.Join(office, "tools", "hq", "config.yaml")
		target := filepath.Join(filepath.Dir(config), "target.yaml")
		writeConfigFixture(t, target, testConfig())
		if err := os.Remove(config); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, config); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveProductionRuntime(globalOptions{Office: office}); err == nil {
			t.Fatal("symlink config accepted")
		}
	})
	t.Run("data symlink", func(t *testing.T) {
		office := productionOfficeFixture(t)
		data := filepath.Join(office, "records")
		target := filepath.Join(filepath.Dir(office), "other-records")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(data); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, data); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveProductionRuntime(globalOptions{Office: office}); err == nil {
			t.Fatal("symlink data accepted")
		}
	})
	t.Run("data regular file", func(t *testing.T) {
		office := productionOfficeFixture(t)
		data := filepath.Join(office, "records")
		if err := os.Remove(data); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(data, []byte("not a directory\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveProductionRuntime(globalOptions{Office: office}); err == nil {
			t.Fatal("non-directory data accepted")
		}
	})
}

func TestAcceptStateFlagIsUnknownAndWritesNothing(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "source.md"), "# source\n")
	order := writeTestFile(t, filepath.Join(e.office, "order.md"), "# order\n")
	decision := writeIssueApproval(t, e, "approved.md", "DEC-STATE-FLAG-001", "STATE-FLAG-001", order, "zantianyou")
	e.setActor(t, "penny", "p1", testAgentCWD(testConfig(), e.root, "penny"))
	runTestCommand(t, e, "case", "create", "--id", "STATE-FLAG-001", "--title", "state flag", "--source", source)
	runTestCommand(t, e, "issue", "--case", "STATE-FLAG-001", "--to", "zantianyou", "--decision", decision, "--next", "work")
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	var sent Event
	for _, event := range events {
		if event.Type == "issue_sent" {
			sent = event
		}
	}
	beforeEvents, beforeDeliveries := len(events), len(e.transport.calls)
	e.setActor(t, "zantianyou", "p2", testAgentCWD(testConfig(), e.root, "zantianyou"))
	app := e.app(t)
	err = app.run([]string{"accept", "--event", sent.ID, "--state", "closed", "--next", "bad"})
	if err == nil || !strings.Contains(err.Error(), "unknown flag") || !strings.Contains(err.Error(), "--state") {
		t.Fatalf("removed state flag was not rejected as unknown: %v", err)
	}
	after, readErr := NewStore(e.data).ReadAll(testConfig())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(after) != beforeEvents || len(e.transport.calls) != beforeDeliveries {
		t.Fatalf("unknown state flag had side effects: events %d->%d deliveries %d->%d", beforeEvents, len(after), beforeDeliveries, len(e.transport.calls))
	}
}

func TestReportRequiresOwnerOrUnconsumedAssignment(t *testing.T) {
	t.Run("unrelated registered agent rejected", func(t *testing.T) {
		e := setupTestEnv(t)
		source := writeTestFile(t, filepath.Join(e.office, "source.md"), "# source\n")
		artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "artifact.md"), "# artifact\n")
		e.setActor(t, "zantianyou", "p1", testAgentCWD(testConfig(), e.root, "zantianyou"))
		runTestCommand(t, e, "case", "create", "--id", "REPORT-AUTH-001", "--title", "auth", "--source", source, "--owner", "eng-data-engineer")
		before, _ := NewStore(e.data).ReadAll(testConfig())
		e.setActor(t, "eng-developer", "p2", testAgentCWD(testConfig(), e.root, "eng-developer"))
		app := e.app(t)
		err := app.run([]string{"report", "--case", "REPORT-AUTH-001", "--result", "completed", "--artifact", artifact, "--next", "manager"})
		if err == nil || !strings.Contains(err.Error(), "不是 case") {
			t.Fatalf("unassigned report should fail, got %v", err)
		}
		after, _ := NewStore(e.data).ReadAll(testConfig())
		if len(after) != len(before) || len(e.transport.calls) != 0 {
			t.Fatalf("rejected report had side effects: events %d->%d delivery=%d", len(before), len(after), len(e.transport.calls))
		}
	})

	t.Run("assignment holder accepted", func(t *testing.T) {
		e := setupTestEnv(t)
		source := writeTestFile(t, filepath.Join(e.office, "source.md"), "# source\n")
		order := writeTestFile(t, filepath.Join(e.office, "order.md"), "# order\n")
		decision := writeIssueApproval(t, e, "approved.md", "DEC-REPORT-AUTH-002", "REPORT-AUTH-002", order, "zantianyou")
		artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "artifact.md"), "# artifact\n")
		e.setActor(t, "penny", "p1", testAgentCWD(testConfig(), e.root, "penny"))
		runTestCommand(t, e, "case", "create", "--id", "REPORT-AUTH-002", "--title", "assignment", "--source", source)
		runTestCommand(t, e, "issue", "--case", "REPORT-AUTH-002", "--to", "zantianyou", "--decision", decision, "--next", "work")
		e.setActor(t, "zantianyou", "p2", testAgentCWD(testConfig(), e.root, "zantianyou"))
		events, err := NewStore(e.data).ReadAll(testConfig())
		if err != nil {
			t.Fatal(err)
		}
		var issueSent Event
		for _, event := range events {
			if event.Type == "issue_sent" {
				issueSent = event
			}
		}
		runTestCommand(t, e, "accept", "--event", issueSent.ID, "--next", "work")
		runTestCommand(t, e, "report", "--case", "REPORT-AUTH-002", "--result", "completed", "--artifact", artifact, "--next", "review")
		events, err = NewStore(e.data).ReadAll(testConfig())
		if err != nil {
			t.Fatal(err)
		}
		var prepared Event
		for _, event := range events {
			if event.Type == "report_prepared" {
				prepared = event
			}
		}
		if prepared.AssignmentEventID == "" {
			t.Fatalf("assignment relation not recorded: %+v", prepared)
		}
	})
}

func createLegalClosedFlow(t *testing.T, e testEnv, caseID string) []Event {
	t.Helper()
	source := writeTestFile(t, filepath.Join(e.office, caseID+"-source.md"), "# source\n")
	order := writeTestFile(t, filepath.Join(e.office, caseID+"-order.md"), "# order\n")
	decision := writeIssueApproval(t, e, caseID+"-approved.md", "DEC-"+caseID, caseID, order, "zantianyou")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+"-artifact.md"), "# artifact\n")
	e.setActor(t, "penny", "p1", testAgentCWD(testConfig(), e.root, "penny"))
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "legal flow", "--source", source)
	runTestCommand(t, e, "issue", "--case", caseID, "--to", "zantianyou", "--decision", decision, "--next", "work")
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	var orderSent Event
	for _, event := range events {
		if event.Type == "issue_sent" {
			orderSent = event
		}
	}
	e.setActor(t, "zantianyou", "p2", testAgentCWD(testConfig(), e.root, "zantianyou"))
	runTestCommand(t, e, "accept", "--event", orderSent.ID, "--next", "implement")
	runTestCommand(t, e, "report", "--case", caseID, "--result", "completed", "--artifact", artifact, "--next", "review")
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	var reportSent Event
	for _, event := range events {
		if event.Type == "report_sent" {
			reportSent = event
		}
	}
	e.setActor(t, "penny", "p1", testAgentCWD(testConfig(), e.root, "penny"))
	runTestCommand(t, e, "accept", "--event", reportSent.ID, "--next", "close")
	runTestCommand(t, e, "close", "--case", caseID, "--reason", "verified", "--source", source)
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestLegalFiniteStateFlowAndCloseAuthority(t *testing.T) {
	e := setupTestEnv(t)
	events := createLegalClosedFlow(t, e, "LEGAL-FLOW-001")
	var transitions []string
	for _, event := range events {
		if event.ToState != "" {
			transitions = append(transitions, event.Type+":"+event.FromState+"->"+event.ToState)
		}
	}
	want := []string{
		"case_created:->open", "issue_sent:open->dispatched", "event_accepted:dispatched->in_progress",
		"report_sent:in_progress->reported", "event_accepted:reported->accepted", "case_closed:accepted->closed",
	}
	if strings.Join(transitions, "|") != strings.Join(want, "|") {
		t.Fatalf("unexpected legal transitions:\n got=%v\nwant=%v", transitions, want)
	}
	snapshot, err := NewStore(e.data).Snapshot(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if state := snapshot.Cases["LEGAL-FLOW-001"]; state == nil || state.Status != "closed" || state.Owner != "" {
		t.Fatalf("closed state invalid: %+v", state)
	}
}

func writeRawEvents(t *testing.T, data string, events []Event) string {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("writeRawEvents requires at least one event")
	}
	at, err := time.Parse(time.RFC3339, events[0].At)
	if err != nil {
		t.Fatalf("parse first event time %q: %v", events[0].At, err)
	}
	path := filepath.Join(data, "events", at.Format("2006-01")+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	for _, event := range events {
		eventAt, err := time.Parse(time.RFC3339, event.At)
		if err != nil {
			t.Fatalf("parse event time %q: %v", event.At, err)
		}
		if eventAt.Format("2006-01") != at.Format("2006-01") {
			t.Fatalf("writeRawEvents fixture spans multiple months: %s and %s", at.Format("2006-01"), eventAt.Format("2006-01"))
		}
		line, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(line)
		raw.WriteByte('\n')
	}
	if err := os.WriteFile(path, raw.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func indexOfEvent(events []Event, eventType string, occurrence int) int {
	for i, event := range events {
		if event.Type == eventType {
			if occurrence == 0 {
				return i
			}
			occurrence--
		}
	}
	return -1
}

func rehashEventChain(t *testing.T, events []Event) {
	t.Helper()
	previous := genesisEventHash
	for i := range events {
		events[i].PreviousEventHash = previous
		events[i].EventHash = ""
		hash, err := hashEvent(events[i])
		if err != nil {
			t.Fatal(err)
		}
		events[i].EventHash = hash
		previous = hash
	}
}

func TestRebuildRejectsSemanticTamperingWithFileAndLine(t *testing.T) {
	baseEnv := setupTestEnv(t)
	valid := createLegalClosedFlow(t, baseEnv, "REBUILD-GUARD-001")
	tests := []struct {
		name      string
		target    int
		mutate    func(*Event)
		wantError string
	}{
		{name: "unknown state", target: indexOfEvent(valid, "case_created", 0), mutate: func(e *Event) { e.ToState = "mystery" }, wantError: "未知后态"},
		{name: "accept to closed", target: indexOfEvent(valid, "event_accepted", 0), mutate: func(e *Event) { e.ToState = "closed" }, wantError: "非法状态转移"},
		{name: "wrong from state", target: indexOfEvent(valid, "issue_sent", 0), mutate: func(e *Event) { e.FromState = "accepted" }, wantError: "前态"},
		{name: "wrong recipient", target: indexOfEvent(valid, "issue_sent", 0), mutate: func(e *Event) { e.Recipient = "baogong" }, wantError: "不一致"},
		{name: "unauthorized close actor", target: indexOfEvent(valid, "case_closed", 0), mutate: func(e *Event) { e.Actor = "zantianyou"; e.ActorLabel = "工程部-詹天佑" }, wantError: "can_close"},
		{name: "jump transition", target: indexOfEvent(valid, "report_sent", 0), mutate: func(e *Event) { e.ToState = "accepted" }, wantError: "后态"},
		{name: "unknown event type", target: indexOfEvent(valid, "report_prepared", 0), mutate: func(e *Event) { e.Type = "unknown_event" }, wantError: "未知事件类型"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := append([]Event(nil), valid...)
			tc.mutate(&events[tc.target])
			rehashEventChain(t, events)
			data := filepath.Join(canonicalTestTempDir(t), "records")
			path := writeRawEvents(t, data, events)
			_, err := NewStore(data).Rebuild(testConfig())
			if err == nil {
				t.Fatal("tampered ledger unexpectedly rebuilt")
			}
			location := fmt.Sprintf("%s:%d", path, tc.target+1)
			if !strings.Contains(err.Error(), location) || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error lacks precise location/reason\nerror=%v\nwant location=%s reason=%s", err, location, tc.wantError)
			}
		})
	}
}

func TestNonCloserCannotClose(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "source.md"), "# source\n")
	e.setActor(t, "zantianyou", "p1", testAgentCWD(testConfig(), e.root, "zantianyou"))
	runTestCommand(t, e, "case", "create", "--id", "NO-CLOSE-001", "--title", "no close", "--source", source)
	before, _ := NewStore(e.data).ReadAll(testConfig())
	app := e.app(t)
	err := app.run([]string{"close", "--case", "NO-CLOSE-001", "--reason", "bad", "--source", source})
	if err == nil || !strings.Contains(err.Error(), "无权销账") {
		t.Fatalf("non-closer close should fail: %v", err)
	}
	after, _ := NewStore(e.data).ReadAll(testConfig())
	if len(after) != len(before) {
		t.Fatalf("unauthorized close wrote event: %d -> %d", len(before), len(after))
	}
}
