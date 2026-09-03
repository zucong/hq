package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

type testEnv struct {
	root        string
	office      string
	data        string
	config      string
	snapshot    string
	herdr       string
	herdrOutput string
	identity    *fakeIdentityProvider
	transport   *fakeTransport
}

func TestDiscoverOfficeUsesCanonicalConfigAsOnlyAnchor(t *testing.T) {
	root := canonicalTestTempDir(t)
	office := filepath.Join(root, "ceo-office")
	if err := os.MkdirAll(office, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyAnchor := filepath.Join(office, "ROSTER"+".md")
	if err := os.WriteFile(legacyAnchor, []byte("# obsolete\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := discoverOffice(office); err == nil || !strings.Contains(err.Error(), "tools/hq/config.yaml") {
		t.Fatalf("legacy Markdown must not identify an HQ office; err=%v", err)
	}
	writeConfigFixture(t, defaultConfigPath(office), testConfig())
	got, err := discoverOffice(office)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(office)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("office=%q want=%q", got, want)
	}
}

func canonicalTestTempDir(t *testing.T) string {
	t.Helper()
	raw, err := os.MkdirTemp(".", ".hq-test-")
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(abs) })
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

type fakeIdentityProvider struct {
	actors map[string]Actor
	calls  int
	mu     sync.Mutex
}

func (f *fakeIdentityProvider) Resolve(_ Config, _ string, paneID string) (Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	actor, ok := f.actors[paneID]
	if !ok {
		return Actor{}, fmt.Errorf("fake identity 未登记 pane %s", paneID)
	}
	return actor, nil
}

type fakeTransport struct {
	calls  []fakeDelivery
	err    error
	result TransportOutcome
	hook   func()
}

type fakeDelivery struct {
	target  string
	message string
}

func (f *fakeTransport) Deliver(target, message string) DeliveryAttempt {
	f.calls = append(f.calls, fakeDelivery{target: target, message: message})
	if f.hook != nil {
		f.hook()
	}
	outcome := f.result
	if outcome == "" {
		if f.err != nil {
			outcome = transportAmbiguous
		} else {
			outcome = transportSent
		}
	}
	return DeliveryAttempt{Outcome: outcome, Err: f.err}
}

func testConfig() Config {
	cfg := Config{
		Version: registrySchemaVersion, WorkspaceLabel: "hq-test", OwnerPrincipal: "ZC",
		Agents: []AgentRule{
			{Name: "penny", Label: "Penny通报", Nickname: "Penny", DepartmentLabel: "总裁办", Workspace: "hq-test", Responsibilities: []string{roleApprovalWitness, roleAccountCloser, "operations_manager"}, ManualPath: "ceo-office/AGENTS.md", Department: "ceo-office", Kind: "codex", CanCreate: true, CanIssue: true, CanAccept: true, CanClose: true, CanManageStaff: true, CanReceiveOrder: true},
			{Name: "zantianyou", Label: "工程部-詹天佑", Nickname: "詹天佑", DepartmentLabel: "工程部", Workspace: "hq-test", Responsibilities: []string{"manager:engineering"}, ManualPath: "engineering/AGENTS.md", Department: "engineering", ReportsTo: "penny", Kind: "codex", CanCreate: true, CanAccept: true, CanReceiveOrder: true},
			{Name: "baogong", Label: "质量与用户体验部-包公", Nickname: "包公", DepartmentLabel: "质量部", Workspace: "hq-test", Responsibilities: []string{"manager:qa-ux"}, ManualPath: "qa-ux/AGENTS.md", Department: "qa-ux", ReportsTo: "penny", Kind: "codex", CanCreate: true, CanAccept: true, CanReceiveOrder: true},
			{Name: "eng-data-engineer", Label: "工程部-郭守敬", Nickname: "郭守敬", DepartmentLabel: "工程部", Workspace: "hq-test", Responsibilities: []string{"data_engineer:engineering"}, ManualPath: "engineering/AGENTS.md", Department: "engineering", ReportsTo: "zantianyou", Kind: "codex", CanCreate: true, CanAccept: true, CanReceiveOrder: true},
			{Name: "eng-developer", Label: "工程部-李春", Nickname: "李春", DepartmentLabel: "工程部", Workspace: "hq-test", Responsibilities: []string{"developer:engineering"}, ManualPath: "engineering/AGENTS.md", Department: "engineering", ReportsTo: "zantianyou", Kind: "codex", CanCreate: true, CanAccept: true, CanReceiveOrder: true},
		},
	}
	return bindTestRoleContracts(cfg)
}

func testRoleManual(name string) []byte {
	return []byte("# Test role: " + name + "\n\n固定测试角色卡；行为由本文件定义。\n")
}

func bindTestRoleContracts(cfg Config) Config {
	cfg.RoleCards = nil
	for index := range cfg.Agents {
		rule := &cfg.Agents[index]
		if rule.PermissionMode == "" {
			rule.PermissionMode = "native"
		}
		rule.WorkstationPath = filepath.Join(rule.Department, "staff", rule.Name, "v1")
		rule.ManualPath = filepath.Join(rule.WorkstationPath, "AGENTS.md")
		if rule.ActivationPolicy == "" {
			rule.ActivationPolicy = activationManual
		}
		rule.MaxWIP = 1
		rule.SeatVersion = 1
		capabilities, err := canonicalStringSet(rule.Responsibilities)
		if err != nil {
			panic(err)
		}
		card := RoleCard{
			ID: rule.Name, Version: 1, Label: rule.Label, Department: rule.Department,
			Capabilities: capabilities, ManualPath: rule.ManualPath,
			ManualDigest: roleCardFileDigest(testRoleManual(rule.Name)), Status: roleCardApproved,
		}
		card.Digest = roleCardDigest(card)
		cfg.RoleCards = append(cfg.RoleCards, card)
		rule.RoleCardID, rule.RoleCardVersion, rule.RoleCardDigest = card.ID, card.Version, card.Digest
		rule.SeatDigest = employeeSeatDigest(*rule)
	}
	return cfg
}

func finalizeTestSeatMutation(rule *AgentRule) {
	rule.SeatVersion++
	rule.SeatDigest = employeeSeatDigest(*rule)
}

func setupTestEnv(t *testing.T) testEnv {
	t.Helper()
	root := canonicalTestTempDir(t)
	office := filepath.Join(root, "ceo-office")
	for _, dir := range []string{
		office,
		filepath.Join(office, "decisions"),
		filepath.Join(root, "engineering"),
		filepath.Join(root, "product"),
		filepath.Join(root, "qa-ux"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig()
	for _, rule := range cfg.Agents {
		workstation := filepath.Join(root, rule.WorkstationPath)
		if err := os.MkdirAll(workstation, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rule.ManualPath), testRoleManual(rule.Name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rawConfig, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(office, "hq-config.yaml")
	if err := os.WriteFile(config, rawConfig, 0o644); err != nil {
		t.Fatal(err)
	}
	herdrOutput := filepath.Join(root, "herdr-calls.log")
	herdr := filepath.Join(root, "fake-herdr")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HQ_HERDR_CAPTURE\"\n"
	if err := os.WriteFile(herdr, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HQ_HERDR_CAPTURE", herdrOutput)
	return testEnv{
		root: root, office: office, data: filepath.Join(office, "records"),
		config: config, herdr: herdr, herdrOutput: herdrOutput,
		identity: &fakeIdentityProvider{actors: map[string]Actor{}}, transport: &fakeTransport{},
	}
}

func (e testEnv) setActor(t *testing.T, name, pane, cwd string) {
	t.Helper()
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	rule, ok := configRuleIncludingDisabled(cfg, name)
	if !ok {
		t.Fatalf("fake actor 未登记：%s", name)
	}
	e.identity.actors[pane] = Actor{Name: name, Label: rule.Label, Department: rule.Department, PaneID: pane, CWD: cwd, Rule: rule}
	t.Setenv("HERDR_PANE_ID", pane)
}

func (e testEnv) app(t *testing.T) *App {
	t.Helper()
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newAppWithDependencies(runtimePaths{
		Office: e.office, HQRoot: e.root, DataDir: e.data, ConfigPath: e.config, HerdrBin: e.herdr,
	}, cfg, globalOptions{}, AppDependencies{
		Store: NewStore(e.data), Identity: e.identity, Transport: e.transport,
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	app.FromGateway = true
	return app
}

func runTestCommand(t *testing.T, e testEnv, command ...string) string {
	t.Helper()
	// Most tests exercise a command other than project admission. Keep their
	// independent root fixtures explicit under the single-space contract while
	// dedicated project-contract tests continue to call App.run directly.
	if len(command) >= 2 && command[0] == "case" && command[1] == "create" {
		hasParent, hasProject := false, false
		for index := 2; index < len(command); index++ {
			switch command[index] {
			case "--parent":
				hasParent = true
			case "--project":
				hasProject = true
			}
		}
		if !hasParent && !hasProject {
			command = append(command, "--project", "test-project")
		}
	}
	var out, errOut bytes.Buffer
	app := e.app(t)
	app.Out, app.Err = &out, &errOut
	if err := app.run(command); err != nil {
		t.Fatalf("command %v failed: %v\nstderr=%s\nstdout=%s", command, err, errOut.String(), out.String())
	}
	return out.String()
}

func writeTestFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeApprovalDocument(t *testing.T, path, decisionID, status string, scopes []ApprovalScope) string {
	t.Helper()
	metadata := ApprovalMetadata{
		Version: 1, DecisionID: decisionID, Status: status,
		ConfirmedBy: testConfig().OwnerPrincipal, ConfirmedAt: "2026-08-28T00:00:00Z", Scopes: scopes,
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	return writeTestFile(t, path, approvalHeaderMarker+string(raw)+metadataHeaderEnd+"# synthetic decision\n")
}

func writeIssueApproval(t *testing.T, e testEnv, filename, decisionID, caseID, source, target string) string {
	t.Helper()
	_ = source
	metadata := standingAuthorizationMetadata{
		Version: 1, DecisionID: decisionID, Status: "effective",
		ConfirmedBy: testConfig().OwnerPrincipal, ConfirmedAt: "2026-08-28T00:00:00Z",
		Scopes: []standingScope{{Action: "issue", Actor: "penny", Target: target, CasePrefix: caseID}},
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	return writeTestFile(t, filepath.Join(e.office, "decisions", filename), standingHeaderMarker+string(raw)+metadataHeaderEnd+"# synthetic standing decision\n")
}

func TestFullIssueReportAcceptAndRebuild(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "source.md"), "# source\n")
	order := writeTestFile(t, filepath.Join(e.office, "order.md"), "# order\n")
	decision := writeIssueApproval(t, e, "approved.md", "DEC-TEST-CASE-001", "TEST-CASE-001", order, "zantianyou")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "result.md"), "# result\n")

	e.setActor(t, "penny", "w1:p1", e.office)
	runTestCommand(t, e, "case", "create", "--id", "TEST-CASE-001", "--title", "测试事项", "--project", "test", "--source", source)
	runTestCommand(t, e, "issue", "--case", "TEST-CASE-001", "--to", "zantianyou", "--decision", decision, "--next", "工程部接令")

	store := NewStore(e.data)
	events, err := store.ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	var orderPrepared Event
	for _, event := range events {
		if event.Type == "issue_prepared" {
			orderPrepared = event
		}
	}
	if orderPrepared.ID == "" {
		t.Fatal("missing issue_prepared event")
	}

	e.setActor(t, "zantianyou", "w1:p2", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", orderPrepared.ID, "--next", "开始实施")
	runTestCommand(t, e, "report", "--case", "TEST-CASE-001", "--result", "completed", "--artifact", artifact, "--next", "Penny核验后转QA")

	events, err = store.ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	var reportSent Event
	for _, event := range events {
		if event.Type == "report_sent" {
			reportSent = event
		}
	}
	if reportSent.ID == "" || reportSent.Recipient != "penny" {
		t.Fatalf("bad report routing: %+v", reportSent)
	}

	e.setActor(t, "penny", "w1:p1", e.office)
	inbox := runTestCommand(t, e, "inbox")
	if !strings.Contains(inbox, reportSent.ID) {
		t.Fatalf("Penny inbox missing report %s:\n%s", reportSent.ID, inbox)
	}
	runTestCommand(t, e, "accept", "--event", reportSent.ID, "--next", "转qa-ux复验")

	snapshot, err := store.Snapshot(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	state := snapshot.Cases["TEST-CASE-001"]
	if state == nil || state.Status != "accepted" || state.Owner != "penny" {
		t.Fatalf("unexpected case state: %+v", state)
	}
	if state.SourceRef != source {
		t.Fatalf("canonical source changed: got %s want %s", state.SourceRef, source)
	}

	if err := os.Remove(filepath.Join(e.data, "state.json")); err != nil {
		t.Fatal(err)
	}
	finalEvents, err := store.ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := store.Rebuild(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Cases["TEST-CASE-001"].Status != "accepted" || rebuilt.EventCount != len(finalEvents) ||
		rebuilt.LastSequence != finalEvents[len(finalEvents)-1].Sequence || rebuilt.LastEventHash != finalEvents[len(finalEvents)-1].EventHash {
		t.Fatalf("bad rebuild: %+v, events=%d", rebuilt.Cases["TEST-CASE-001"], rebuilt.EventCount)
	}

	var delivered strings.Builder
	for _, call := range e.transport.calls {
		fmt.Fprintf(&delivered, "%s %s\n", call.target, call.message)
	}
	for _, expected := range []string{"zantianyou [HQ notification][Penny通报]", "penny [HQ notification][工程部-詹天佑]"} {
		if !strings.Contains(delivered.String(), expected) {
			t.Fatalf("missing fake delivery %q:\n%s", expected, delivered.String())
		}
	}
}

func TestChildReportRoutesToManagerAndLabelsSender(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "source.md"), "# source\n")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "child-result.md"), "# result\n")

	e.setActor(t, "zantianyou", "w1:p2", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "case", "create", "--id", "TEST-CHILD-001", "--title", "子角色任务", "--source", source, "--owner", "eng-data-engineer")

	e.setActor(t, "eng-data-engineer", "w1:p3", filepath.Join(e.root, "engineering"))
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	runTestCommand(t, e, "report", "--case", "TEST-CHILD-001", "--result", "completed", "--artifact", artifact, "--next", "经理验收")

	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	var sent Event
	for _, event := range events {
		if event.Type == "report_sent" {
			sent = event
		}
	}
	if sent.Recipient != "zantianyou" || sent.ActorLabel != "工程部-郭守敬" {
		t.Fatalf("unexpected child route: %+v", sent)
	}
	if len(e.transport.calls) == 0 || e.transport.calls[len(e.transport.calls)-1].target != "zantianyou" ||
		!strings.HasPrefix(e.transport.calls[len(e.transport.calls)-1].message, "[HQ notification][工程部-郭守敬]") {
		t.Fatalf("sender label not generated at sentence head: %+v", e.transport.calls)
	}
}

func TestPermissionApprovalAndSensitiveValidation(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "source.md"), "# source\n")
	draft := writeApprovalDocument(t, filepath.Join(e.office, "decisions", "draft.md"), "DEC-TEST-GUARD-001", "draft", []ApprovalScope{{
		Action: "issue", CaseID: "TEST-GUARD-001", SourceRef: source, Target: "baogong",
	}})

	e.setActor(t, "penny", "w1:p1", e.office)
	runTestCommand(t, e, "case", "create", "--id", "TEST-GUARD-001", "--title", "守卫测试", "--source", source)

	e.setActor(t, "zantianyou", "w1:p2", filepath.Join(e.root, "engineering"))
	var out, errOut bytes.Buffer
	app := e.app(t)
	app.Out, app.Err = &out, &errOut
	err := app.run([]string{"issue", "--case", "TEST-GUARD-001", "--to", "baogong", "--decision", draft, "--next", "测试"})
	if err == nil || !strings.Contains(err.Error(), "直属") {
		t.Fatalf("manager issue should fail, got %v", err)
	}

	e.setActor(t, "penny", "w1:p1", e.office)
	out.Reset()
	errOut.Reset()
	app = e.app(t)
	app.Out, app.Err = &out, &errOut
	err = app.run([]string{"issue", "--case", "TEST-GUARD-001", "--to", "baogong", "--decision", draft, "--next", "测试"})
	if err == nil || !strings.Contains(err.Error(), "hq-standing-authorization:v1") {
		t.Fatalf("draft approval should fail, got %v", err)
	}

	if _, err := validateShortText("note", "价格为￥123", false); err == nil {
		t.Fatal("money-like note should be rejected")
	}
}

func TestUnixGatewayMediatesDepartmentWrite(t *testing.T) {
	e := setupTestEnv(t)
	shortData, err := os.MkdirTemp(os.TempDir(), "hqgw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortData) })
	e.data = shortData
	source := writeTestFile(t, filepath.Join(e.office, "source.md"), "# source\n")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "gateway-result.md"), "# result\n")

	e.setActor(t, "zantianyou", "w1:p2", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "case", "create", "--id", "TEST-GATEWAY-001", "--title", "网关测试", "--source", source, "--owner", "eng-data-engineer")

	server := e.app(t)
	server.FromGateway = false
	server.GatewayWorkspaceID, server.GatewayServerID = "w1", "gateway-test"
	server.Out, server.Err = io.Discard, io.Discard
	socket := filepath.Join(e.data, "hq.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	defer os.Remove(socket)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		server.handleGatewayConn(conn)
	}()

	e.setActor(t, "eng-data-engineer", "w1:p3", filepath.Join(e.root, "engineering"))
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	var out, errOut bytes.Buffer
	if err := forwardToGateway(socket, []string{"report", "--case", "TEST-GATEWAY-001", "--result", "completed", "--artifact", artifact, "--next", "经理验收"}, false, false, &out, &errOut); err != nil {
		t.Fatalf("gateway report failed: %v\nstderr=%s\nstdout=%s", err, errOut.String(), out.String())
	}
	<-done

	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	var prepared, attempted, sent Event
	for _, event := range events {
		switch event.Type {
		case "report_prepared":
			prepared = event
		case "delivery_attempted":
			if event.DeliveryID == prepared.DeliveryID {
				attempted = event
			}
		case "report_sent":
			sent = event
		}
	}
	if prepared.Actor != "eng-data-engineer" || prepared.ActorLabel != "工程部-郭守敬" || prepared.ActorPaneID != "w1:p3" ||
		attempted.Actor != prepared.Actor || attempted.ActorLabel != prepared.ActorLabel || attempted.ActorPaneID != prepared.ActorPaneID ||
		sent.Actor != prepared.Actor || sent.ActorLabel != prepared.ActorLabel || sent.ActorPaneID != prepared.ActorPaneID || sent.Recipient != "zantianyou" {
		t.Fatalf("gateway did not preserve verified origin actor across outbox lifecycle: prepared=%+v attempted=%+v terminal=%+v", prepared, attempted, sent)
	}
}

func TestStaffCRUDPreflightAllowsEmptyLedger(t *testing.T) {
	e := setupTestEnv(t)
	if events, err := NewStore(e.data).ReadAll(testConfig()); err != nil || len(events) != 0 {
		t.Fatalf("staff empty-ledger fixture is not empty: events=%d err=%v", len(events), err)
	}
	setRegistryMutationActor(t, e, "w1:p1")
	role := addTestRoleCard(t, e, "DEC-STAFF-CRUD-ROLE", "eng-new-reviewer-role", 1, "engineering/staff/eng-new-reviewer/v1", "review")
	addRule := AgentRule{
		Name: "eng-new-reviewer", Label: "工程部-新审", Nickname: "工程部-新审", DepartmentLabel: "engineering", Workspace: "hq-test", Responsibilities: []string{"staff:eng-new-reviewer"},
		ManualPath: role.ManualPath, RoleCardID: role.ID, RoleCardVersion: role.Version, RoleCardDigest: role.Digest,
		WorkstationPath: filepath.Dir(role.ManualPath), ActivationPolicy: activationOnAssignment, MaxWIP: 1, SeatVersion: 1, Department: "engineering",
		Kind: "codex", PermissionMode: "native", ReportsTo: "zantianyou", CanCreate: true, CanAccept: true,
	}
	addRule.SeatDigest = employeeSeatDigest(addRule)
	addDecision := writeApprovalDocument(t, filepath.Join(e.office, "decisions", "staff-add-approved.md"), "DEC-STAFF-ADD-001", "effective", []ApprovalScope{{
		Action: "staff:add", Target: addRule.Name, RequestDigest: staffScopeDigest("staff:add", addRule),
	}})

	runTestCommand(t, e, "staff", "add",
		"--name", "eng-new-reviewer", "--label", "工程部-新审",
		"--department", "engineering", "--kind", "codex", "--reports-to", "zantianyou",
		"--role", roleCardKey(role.ID, role.Version), "--workstation", addRule.WorkstationPath,
		"--activation", activationOnAssignment, "--max-wip", "1",
		"--grant", "create,accept", "--approval", addDecision)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	added, ok := cfg.exactRule("eng-new-reviewer")
	if !ok || added.Label != "工程部-新审" || added.ReportsTo != "zantianyou" || !added.CanCreate {
		t.Fatalf("staff add not reflected in config: %+v", added)
	}

	expectedUpdate := added
	expectedUpdate.Label, expectedUpdate.ActivationPolicy = "工程部-新审二", activationAlways
	finalizeTestSeatMutation(&expectedUpdate)
	updateDecision := writeApprovalDocument(t, filepath.Join(e.office, "decisions", "staff-update-approved.md"), "DEC-STAFF-UPDATE-001", "effective", []ApprovalScope{{
		Action: "staff:update", Target: expectedUpdate.Name, RequestDigest: staffScopeDigest("staff:update", expectedUpdate),
	}})
	runTestCommand(t, e, "staff", "update", "--name", "eng-new-reviewer",
		"--label", "工程部-新审二", "--activation", activationAlways, "--approval", updateDecision)
	cfg, err = loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	updated, ok := cfg.exactRule("eng-new-reviewer")
	if !ok || updated.Label != "工程部-新审二" || updated.ActivationPolicy != activationAlways {
		t.Fatalf("staff update not reflected in config: %+v", updated)
	}

	expectedRemove := updated
	expectedRemove.Disabled, expectedRemove.ActivationPolicy = true, activationManual
	finalizeTestSeatMutation(&expectedRemove)
	removeDecision := writeApprovalDocument(t, filepath.Join(e.office, "decisions", "staff-remove-approved.md"), "DEC-STAFF-REMOVE-001", "effective", []ApprovalScope{{
		Action: "staff:remove", Target: expectedRemove.Name, RequestDigest: staffScopeDigest("staff:remove", expectedRemove),
	}})
	runTestCommand(t, e, "staff", "remove", "--name", "eng-new-reviewer", "--approval", removeDecision)
	cfg, err = loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	removed, ok := configRuleIncludingDisabled(cfg, "eng-new-reviewer")
	if !ok || !removed.Disabled || removed.ActivationPolicy != activationManual {
		t.Fatalf("staff remove should disable without erasing: %+v", removed)
	}
	if _, active := cfg.exactRule("eng-new-reviewer"); active {
		t.Fatal("disabled employee must not resolve as an active identity")
	}
}

func TestUpStartsEmployeeFromConfigWithoutCodeChange(t *testing.T) {
	e := setupTestEnv(t)
	configured := addRuntimeTestAgent(t, e, AgentRule{
		Name: "eng-configured", Label: "工程部-配置员工", Nickname: "配置员工", DepartmentLabel: "工程部", Workspace: "hq-test", Responsibilities: []string{"configured:engineering"}, ManualPath: "engineering/AGENTS.md", Department: "engineering",
		Kind: "kimi", PermissionMode: "yolo", ReportsTo: "zantianyou", ActivationPolicy: activationAlways,
	})
	e.setActor(t, "penny", "w1:p1", testAgentCWD(testConfig(), e.root, "penny"))
	app := e.app(t)
	control := newFakeHerdrControl(e.root, "hq-test")
	app.Herdr = control
	app.Sessions = &FileSessionStore{Root: filepath.Join(e.root, "up-session")}
	app.FromGateway, app.Direct = false, true
	app.Out, app.Err = io.Discard, io.Discard
	if err := app.run([]string{"up", "--no-gateway", configured.Name}); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(control.calls, "\n")
	for _, expected := range []string{
		"tab create 工程部-配置员工", "agent start eng-configured kimi --auto",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("config-driven start missing %q:\n%s", expected, text)
		}
	}
	if !strings.Contains(text, "prompt eng-configured [HQ notification]") {
		t.Fatalf("missing injected control prompt: %s", text)
	}
}

func TestUpStartsGrokWithAlwaysApprove(t *testing.T) {
	e := setupTestEnv(t)
	configured := addRuntimeTestAgent(t, e, AgentRule{
		Name: "eng-grok", Label: "工程部-Grok岗", Nickname: "Grok岗", DepartmentLabel: "工程部", Workspace: "hq-test", Responsibilities: []string{"grok:engineering"}, ManualPath: "engineering/AGENTS.md", Department: "engineering",
		Kind: "grok", PermissionMode: "yolo", ReportsTo: "zantianyou", ActivationPolicy: activationAlways,
	})
	e.setActor(t, "penny", "w1:p1", testAgentCWD(testConfig(), e.root, "penny"))
	app := e.app(t)
	control := newFakeHerdrControl(e.root, "hq-test")
	app.Herdr = control
	app.Sessions = &FileSessionStore{Root: filepath.Join(e.root, "up-grok-session")}
	app.FromGateway, app.Direct = false, true
	app.Out, app.Err = io.Discard, io.Discard
	if err := app.run([]string{"up", "--no-gateway", configured.Name}); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(control.calls, "\n")
	want := "agent start eng-grok grok --always-approve"
	if !strings.Contains(text, want) {
		t.Fatalf("grok start missing always-approve:\n%s", text)
	}
}
