package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestManagerApprovalRequestRejectsUnusableScopeWithoutSideEffects(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "manager-approval.md"), "# manager approval\n")
	e.setActor(t, "zantianyou", "manager-approval:pane", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "case", "create", "--id", "MANAGER-APPROVAL-DIRECT", "--title", "Direct report", "--source", source)
	runTestCommand(t, e, "case", "create", "--id", "MANAGER-APPROVAL-CROSS", "--parent", "MANAGER-APPROVAL-DIRECT", "--title", "Cross boundary", "--source", source)

	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	tests := []struct {
		name, id, caseID, target string
		want                     []string
	}{
		{
			name: "direct report needs no approval", id: "APR-MANAGER-DIRECT", caseID: "MANAGER-APPROVAL-DIRECT", target: "eng-developer",
			want: []string{"直属下属无需 approval", "hq issue", "--case MANAGER-APPROVAL-DIRECT", "--to eng-developer", "不要传 --approval/--decision"},
		},
		{
			name: "approval cannot cross manager boundary", id: "APR-MANAGER-CROSS", caseID: "MANAGER-APPROVAL-CROSS", target: "baogong",
			want: []string{"只能委派自己的直属下属", "target=baogong", "reports_to=penny", "不得通过 approval 跨越管理边界", "工作路由给对应经理 penny", "hq case escalate", "--parent MANAGER-APPROVAL-CROSS"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := snapshotTree(t, e.data)
			transportBefore := len(e.transport.calls)
			err := e.app(t).run([]string{"approval", "request", "--id", test.id, "--case", test.caseID, "--target", test.target, "--expires", expires})
			if err == nil || exitCodeForError(err) != exitPermission {
				t.Fatalf("manager approval request was not rejected as a permission error: %v", err)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("manager approval rejection missing %q: %v", want, err)
				}
			}
			if after := snapshotTree(t, e.data); !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected approval request changed ledger\nbefore=%v\nafter=%v", before, after)
			}
			if len(e.transport.calls) != transportBefore {
				t.Fatalf("rejected approval request reached transport: %d -> %d", transportBefore, len(e.transport.calls))
			}
		})
	}
}

func TestApprovalRequestRequiresWitnessOwnedCaseWithoutSideEffects(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "witness-owned-approval.md"), "# witness-owned approval\n")
	e.setActor(t, "penny", "witness-owner:pane", e.office)
	runTestCommand(t, e, "case", "create", "--id", "APPROVAL-WRONG-OWNER", "--title", "Wrong owner", "--owner", "zantianyou", "--source", source)

	before := snapshotTree(t, e.data)
	err := e.app(t).run([]string{"approval", "request", "--id", "APR-WRONG-OWNER", "--case", "APPROVAL-WRONG-OWNER", "--target", "zantianyou", "--expires", time.Now().UTC().Add(time.Hour).Format(time.RFC3339)})
	if err == nil || exitCodeForError(err) != exitPermission {
		t.Fatalf("approval for a non-witness-owned case was not rejected: %v", err)
	}
	for _, want := range []string{"approval_witness", "penny", "case=APPROVAL-WRONG-OWNER", "owner=zantianyou", "正式接收 case", "hq approval request"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("wrong-owner rejection missing %q: %v", want, err)
		}
	}
	if after := snapshotTree(t, e.data); !reflect.DeepEqual(after, before) || len(e.transport.calls) != 0 {
		t.Fatalf("wrong-owner approval rejection had side effects: ledger_equal=%t transport=%d", reflect.DeepEqual(after, before), len(e.transport.calls))
	}
}

func TestManagerIssueRejectionsProvideExecutableCorrection(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "manager-issue-correction.md"), "# manager issue correction\n")
	e.setActor(t, "zantianyou", "manager-issue-correction:pane", filepath.Join(e.root, "engineering"))
	for index, id := range []string{"MANAGER-ISSUE-APPROVAL", "MANAGER-ISSUE-DECISION", "MANAGER-ISSUE-CROSS"} {
		args := []string{"case", "create", "--id", id, "--title", id, "--source", source}
		if index != 0 {
			args = append(args, "--parent", "MANAGER-ISSUE-APPROVAL")
		}
		runTestCommand(t, e, args...)
	}
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "remove approval", args: []string{"issue", "--case", "MANAGER-ISSUE-APPROVAL", "--to", "eng-developer", "--approval", "APR-UNUSABLE", "--next", "work"},
			want: []string{"删除 --approval/--decision 后重试", "hq issue --case MANAGER-ISSUE-APPROVAL --to eng-developer --next TEXT"},
		},
		{
			name: "remove decision", args: []string{"issue", "--case", "MANAGER-ISSUE-DECISION", "--to", "eng-developer", "--decision", "unused.md", "--next", "work"},
			want: []string{"删除 --approval/--decision 后重试", "hq issue --case MANAGER-ISSUE-DECISION --to eng-developer --next TEXT"},
		},
		{
			name: "route through target manager", args: []string{"issue", "--case", "MANAGER-ISSUE-CROSS", "--to", "baogong", "--next", "work"},
			want: []string{"actor=zantianyou", "target=baogong", "target_reports_to=penny", "工作路由给对应经理 penny", "hq case escalate", "--parent MANAGER-ISSUE-CROSS", "固定上交直属上级 penny"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := snapshotTree(t, e.data)
			err := e.app(t).run(test.args)
			if err == nil || exitCodeForError(err) != exitPermission {
				t.Fatalf("manager issue misuse was not rejected: %v", err)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("manager issue correction missing %q: %v", want, err)
				}
			}
			if test.name == "route through target manager" && strings.Contains(err.Error(), "hq message") {
				t.Fatalf("manager issue correction fell back to projection-neutral message: %v", err)
			}
			if after := snapshotTree(t, e.data); !reflect.DeepEqual(after, before) || len(e.transport.calls) != 0 {
				t.Fatalf("rejected manager issue had side effects: ledger_equal=%t transport=%d", reflect.DeepEqual(after, before), len(e.transport.calls))
			}
		})
	}
}

func forgeApprovalRequest(t *testing.T, e testEnv, actorName, caseID, targetName, approvalID string) error {
	t.Helper()
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(e.data)
	events, err := store.ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := validateLedger(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	state, err := ledger.currentCase(caseID)
	if err != nil {
		t.Fatal(err)
	}
	target, ok := cfg.exactRule(targetName)
	if !ok {
		t.Fatalf("missing target %s", targetName)
	}
	actor := actorFor(cfg, actorName, "forged-approval:"+actorName, e.office)
	event := testLedgerEvent(t, store, actor, "approval_requested", caseID)
	event.ApprovalID, event.ApprovalAction, event.ApprovalStatus, event.ApprovalMode = approvalID, "issue", "requested", "one_time"
	event.Recipient, event.RecipientLabel = target.Name, target.Label
	event.CaseVersion, event.CaseDigest, event.Title = state.Version, state.Digest, state.Title
	event.BasisEventID = ledger.caseGeneration(caseID)
	event.AssigneeSeatVersion, event.AssigneeSeatDigest = target.SeatVersion, target.SeatDigest
	lastAt, err := time.Parse(time.RFC3339, events[len(events)-1].At)
	if err != nil {
		t.Fatal(err)
	}
	event.ExpiresAt = lastAt.Add(time.Hour).Format(time.RFC3339)
	events = appendForgedFenceEvent(t, events, event, approvalID)
	writeRawEvents(t, e.data, events)
	_, err = store.ReadAll(cfg)
	return err
}

func TestApprovalRequestedStrictReplayEnforcesWitnessOwnerAndDirectManager(t *testing.T) {
	tests := []struct {
		name, actor, owner, target, id string
		want                           []string
	}{
		{
			name: "actor must be witness", actor: "zantianyou", owner: "zantianyou", target: "eng-developer", id: "APR-FORGED-ACTOR",
			want: []string{"总裁秘书", "approval_witness", "agent=penny", "actor=zantianyou"},
		},
		{
			name: "owner must be witness", actor: "penny", owner: "zantianyou", target: "zantianyou", id: "APR-FORGED-OWNER",
			want: []string{"当时 case owner", "agent=penny", "owner=zantianyou"},
		},
		{
			name: "target must be direct manager", actor: "penny", owner: "penny", target: "eng-developer", id: "APR-FORGED-SPECIALIST",
			want: []string{"直属部门经理", "agent=penny", "target=eng-developer", "target_reports_to=zantianyou", "target=zantianyou（直属经理，不是 specialist eng-developer）", "拆分子 case"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := setupTestEnv(t)
			source := writeTestFile(t, filepath.Join(e.office, test.id+".md"), "# strict approval replay\n")
			e.setActor(t, test.actor, "strict-approval:"+test.actor, testAgentCWD(testConfig(), e.root, test.actor))
			args := []string{"case", "create", "--id", "CASE-" + test.id, "--title", test.id, "--source", source}
			if test.owner != test.actor {
				args = append(args, "--owner", test.owner)
			}
			runTestCommand(t, e, args...)
			err := forgeApprovalRequest(t, e, test.actor, "CASE-"+test.id, test.target, test.id)
			if err == nil {
				t.Fatal("strict replay accepted forged approval request")
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("strict approval replay error missing %q: %v", want, err)
				}
			}
		})
	}
}

func TestApprovalRequestCommandAndReplayRejectTargetWithoutReceiveAuthority(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	for index := range cfg.Agents {
		if cfg.Agents[index].Name == "zantianyou" {
			cfg.Agents[index].CanReceiveOrder = false
			cfg.Agents[index].SeatVersion++
			cfg.Agents[index].SeatDigest = employeeSeatDigest(cfg.Agents[index])
		}
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.config, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	caseID := "APPROVAL-NO-RECEIVE"
	source := writeTestFile(t, filepath.Join(e.office, "approval-no-receive.md"), "# no receive\n")
	e.setActor(t, "penny", "approval-no-receive:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "No receive target", "--source", source)
	before := snapshotTree(t, e.data)
	err = e.app(t).run([]string{"approval", "request", "--id", "APR-NO-RECEIVE", "--case", caseID, "--target", "zantianyou", "--expires", time.Now().UTC().Add(time.Hour).Format(time.RFC3339)})
	if err == nil || !strings.Contains(err.Error(), "can_receive_order") {
		t.Fatalf("approval request accepted target without receive authority: %v", err)
	}
	if after := snapshotTree(t, e.data); !reflect.DeepEqual(after, before) || len(e.transport.calls) != 0 {
		t.Fatalf("no-receive command rejection had side effects: ledger_equal=%t transport=%d", reflect.DeepEqual(after, before), len(e.transport.calls))
	}
	err = forgeApprovalRequest(t, e, "penny", caseID, "zantianyou", "APR-FORGED-NO-RECEIVE")
	if err == nil || !strings.Contains(err.Error(), "can_receive_order") {
		t.Fatalf("strict replay accepted approval target without receive authority: %v", err)
	}
}

func TestIssueStrictReplayRejectsSecretarySkippingManager(t *testing.T) {
	e := setupTestEnv(t)
	caseID := "ISSUE-FORGED-SKIP-MANAGER"
	source := writeTestFile(t, filepath.Join(e.office, "issue-forged-skip-manager.md"), "# forged issue\n")
	e.setActor(t, "penny", "strict-issue:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "Forged skip-level issue", "--source", source)
	cfg := testConfig()
	store := NewStore(e.data)
	events, err := store.ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := validateLedger(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := testBusinessIssueEvent(e.app(t), ledger, "penny", "eng-developer", caseID, "skip-manager")
	if err != nil {
		t.Fatal(err)
	}
	events = appendForgedFenceEvent(t, events, forged, "skip-manager")
	writeRawEvents(t, e.data, events)
	_, err = store.ReadAll(cfg)
	if err == nil {
		t.Fatal("strict replay accepted secretary skip-level issue")
	}
	for _, want := range []string{"target.ReportsTo==actor.Name", "actor=penny", "target=eng-developer", "target_reports_to=zantianyou"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("strict issue replay error missing %q: %v", want, err)
		}
	}
}

func TestPortableWitnessSkipLevelErrorsUseDynamicSlug(t *testing.T) {
	cfgDoc := portableRegistry("portable-hq", "executive", "delivery", "relay", "bridge", "maker")
	root, office, config := writePortableRegistryFixture(t, cfgDoc, "executive")
	cfg, err := loadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	identity := &fakeIdentityProvider{actors: map[string]Actor{}}
	transport := &fakeTransport{}
	data := filepath.Join(office, "records")
	app, err := newAppWithDependencies(runtimePaths{Office: office, HQRoot: root, DataDir: data, ConfigPath: config}, cfg, globalOptions{}, AppDependencies{
		Store: NewStore(data), Identity: identity, Transport: transport,
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	app.FromGateway = true
	rule, _ := cfg.exactRule("relay")
	identity.actors["portable:relay"] = Actor{Name: "relay", Label: rule.Label, Department: rule.Department, PaneID: "portable:relay", CWD: filepath.Join(root, rule.WorkstationPath), Rule: rule}
	app.CallerPane = "portable:relay"
	source := writeTestFile(t, filepath.Join(office, "portable-skip.md"), "# portable skip\n")
	if err := app.run([]string{"case", "create", "--id", "PORTABLE-SKIP", "--title", "Portable skip", "--project", "test-project", "--source", source}); err != nil {
		t.Fatal(err)
	}

	before := snapshotTree(t, data)
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	commands := [][]string{
		{"approval", "request", "--id", "APR-PORTABLE-SKIP", "--case", "PORTABLE-SKIP", "--target", "maker", "--expires", expires},
		{"issue", "--case", "PORTABLE-SKIP", "--to", "maker", "--decision", "unused.md", "--next", "work"},
	}
	for _, command := range commands {
		err := app.run(command)
		if err == nil || exitCodeForError(err) != exitPermission {
			t.Fatalf("portable skip-level command was not rejected: command=%v err=%v", command, err)
		}
		for _, want := range []string{"总裁秘书", "approval_witness", "agent=relay", "target=maker", "bridge"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("portable error missing %q: %v", want, err)
			}
		}
		if strings.Contains(strings.ToLower(err.Error()), "penny") {
			t.Fatalf("portable error hard-coded Penny: %v", err)
		}
	}
	if after := snapshotTree(t, data); !reflect.DeepEqual(after, before) || len(transport.calls) != 0 {
		t.Fatalf("portable skip-level rejections had side effects: ledger_equal=%t transport=%d", reflect.DeepEqual(after, before), len(transport.calls))
	}
}

func prepareStaleApprovalGrant(t *testing.T, mutation string) (testEnv, string, string) {
	t.Helper()
	e := setupTestEnv(t)
	caseID := "STALE-GRANT-" + strings.ToUpper(mutation)
	approvalID := "APR-" + caseID
	source := writeTestFile(t, filepath.Join(e.office, strings.ToLower(caseID)+".md"), "# stale grant\n")
	e.setActor(t, "penny", "stale-grant:penny:"+mutation, e.office)
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "Stale grant", "--source", source)
	runTestCommand(t, e, "approval", "request", "--id", approvalID, "--case", caseID, "--target", "zantianyou", "--expires", time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
	switch mutation {
	case "version":
		runTestCommand(t, e, "case", "revise", "--id", caseID, "--title", "Stale grant revised", "--version", "2", "--source", source)
	case "owner":
		decision := writeIssueApproval(t, e, "stale-grant-owner.md", "DEC-STALE-GRANT-OWNER", caseID, source, "zantianyou")
		runTestCommand(t, e, "issue", "--case", caseID, "--to", "zantianyou", "--decision", decision, "--next", "Take ownership")
	default:
		t.Fatalf("unknown stale grant mutation %q", mutation)
	}
	return e, caseID, approvalID
}

func forgeApprovalGrant(t *testing.T, e testEnv, approvalID string) error {
	t.Helper()
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(e.data)
	events, err := store.ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := validateLedger(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	record := ledger.approvals[approvalID]
	if record == nil || record.Status != "requested" {
		t.Fatalf("missing requested approval %s: %+v", approvalID, record)
	}
	witness, err := cfg.approvalWitness()
	if err != nil {
		t.Fatal(err)
	}
	actor := actorFor(cfg, witness.Name, "forged-grant:"+witness.Name, e.office)
	event := testLedgerEvent(t, store, actor, "approval_granted", record.Request.CaseID)
	copyApprovalScope(&event, record.Request)
	event.RelatedEventID = record.Request.ID
	event.ApprovalStatus, event.Issuer, event.CapturedBy = "granted", cfg.ownerPrincipal(), witness.Name
	events = appendForgedFenceEvent(t, events, event, approvalID+"-grant")
	writeRawEvents(t, e.data, events)
	_, err = store.ReadAll(cfg)
	return err
}

func TestApprovalGrantRejectsStaleOwnerOrCaseGenerationWithoutSideEffects(t *testing.T) {
	tests := []struct {
		mutation string
		want     []string
	}{
		{mutation: "version", want: []string{"已过期于 case 规格", "request=STALE-GRANT-VERSION@v1", "current=STALE-GRANT-VERSION@v2", "不要重试旧 grant", "新 approval_id", "agent=penny"}},
		{mutation: "owner", want: []string{"已失效", "case=STALE-GRANT-OWNER", "当前 owner=zantianyou", "approval_witness", "agent=penny", "不要重试旧 grant", "新 approval_id"}},
	}
	for _, test := range tests {
		t.Run(test.mutation, func(t *testing.T) {
			e, _, approvalID := prepareStaleApprovalGrant(t, test.mutation)
			before := snapshotTree(t, e.data)
			transportBefore := len(e.transport.calls)
			err := e.app(t).run([]string{"approval", "grant", "--id", approvalID, "--issuer", testConfig().ownerPrincipal()})
			if err == nil || exitCodeForError(err) != exitConflict {
				t.Fatalf("stale approval grant was not rejected as conflict: %v", err)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("stale grant error missing %q: %v", want, err)
				}
			}
			if after := snapshotTree(t, e.data); !reflect.DeepEqual(after, before) || len(e.transport.calls) != transportBefore {
				t.Fatalf("stale grant rejection had side effects: ledger_equal=%t transport=%d->%d", reflect.DeepEqual(after, before), transportBefore, len(e.transport.calls))
			}
		})
	}
}

func TestApprovalGrantedStrictReplayRejectsStaleOwnerOrCaseGeneration(t *testing.T) {
	for _, mutation := range []string{"version", "owner"} {
		t.Run(mutation, func(t *testing.T) {
			e, _, approvalID := prepareStaleApprovalGrant(t, mutation)
			err := forgeApprovalGrant(t, e, approvalID)
			if err == nil {
				t.Fatal("strict replay accepted stale approval grant")
			}
			for _, want := range []string{approvalID, "不要重试旧 grant", "新 approval_id", "agent=penny"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("strict stale grant error missing %q: %v", want, err)
				}
			}
		})
	}
}

func TestManagerDirectIssueWithoutApprovalActivatesDirectReport(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	for index := range cfg.Agents {
		if cfg.Agents[index].Name == "eng-developer" {
			cfg.Agents[index].ActivationPolicy = activationOnAssignment
			cfg.Agents[index].SeatVersion++
			cfg.Agents[index].SeatDigest = employeeSeatDigest(cfg.Agents[index])
		}
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.config, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	source := writeTestFile(t, filepath.Join(e.root, "engineering", "manager-direct-issue.md"), "# direct issue\n")
	e.setActor(t, "zantianyou", "manager-direct:pane", filepath.Join(e.root, "engineering"))
	app := e.app(t)
	runtimeState := deliveryRuntimeOffline
	resumeCalls := 0
	app.DeliveryTargetState = func(target string) (deliveryRuntimeState, error) {
		if target != "eng-developer" {
			t.Fatalf("inspected wrong seat: %s", target)
		}
		return runtimeState, nil
	}
	app.DeliveryColdResume = func(target string) error {
		if target != "eng-developer" {
			t.Fatalf("activated wrong seat: %s", target)
		}
		resumeCalls++
		runtimeState = deliveryRuntimeIdle
		return nil
	}
	if err := app.run([]string{"case", "create", "--id", "MANAGER-DIRECT-ISSUE", "--title", "Direct manager issue", "--project", "test-project", "--source", source}); err != nil {
		t.Fatal(err)
	}
	if err := app.run([]string{"issue", "--case", "MANAGER-DIRECT-ISSUE", "--to", "eng-developer", "--next", "Implement and report"}); err != nil {
		t.Fatal(err)
	}
	if resumeCalls != 1 || runtimeState != deliveryRuntimeIdle || len(e.transport.calls) != 1 {
		t.Fatalf("direct manager issue did not activate exactly one direct report: resumes=%d state=%s transport=%d", resumeCalls, runtimeState, len(e.transport.calls))
	}
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	prepared := latestCaseEvent(events, "MANAGER-DIRECT-ISSUE", "issue_prepared")
	if prepared.AuthorizationType != "manager" || prepared.ApprovalID != "" || prepared.DecisionRef != "" || prepared.Recipient != "eng-developer" {
		t.Fatalf("direct manager issue used the wrong authorization path: %+v", prepared)
	}
}
