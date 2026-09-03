package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestIssueFreezesRoleCardSeatAndEnforcesWIP(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "role-card-source.md"), "# source\n")
	order := writeTestFile(t, filepath.Join(e.office, "role-card-order.md"), "# order\n")
	decision := writeIssueApproval(t, e, "role-card-decision.md", "DEC-ROLE-CARD-001", "ROLE-CARD-001", order, "zantianyou")

	e.setActor(t, "penny", "role-card:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", "ROLE-CARD-001", "--title", "Role card assignment", "--project", "roles", "--source", source)
	runTestCommand(t, e, "case", "create", "--id", "ROLE-CARD-002", "--parent", "ROLE-CARD-001", "--title", "WIP guard", "--source", source)
	runTestCommand(t, e, "issue", "--case", "ROLE-CARD-001", "--to", "zantianyou", "--decision", decision, "--next", "execute the frozen role")

	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	issue := latestCaseEvent(events, "ROLE-CARD-001", "issue_sent")
	rule, _ := testConfig().exactRule("zantianyou")
	if issue.AssigneeSeatVersion != rule.SeatVersion || issue.AssigneeSeatDigest != rule.SeatDigest ||
		issue.RoleCardID != rule.RoleCardID || issue.RoleCardVersion != rule.RoleCardVersion ||
		issue.RoleCardDigest != rule.RoleCardDigest || issue.RoleCardManualPath != rule.ManualPath {
		t.Fatalf("issue did not freeze the selected employee seat and role card: issue=%+v rule=%+v", issue, rule)
	}
	if issue.AssignmentDigest != assignmentContractDigest(issue) {
		t.Fatalf("assignment digest omitted frozen role binding: %+v", issue)
	}
	if len(e.transport.calls) != 1 {
		t.Fatalf("transport calls=%d, want one issue doorbell", len(e.transport.calls))
	}
	prompt := e.transport.calls[0].message
	for _, want := range []string{
		"ROLE_CARD=" + rule.RoleCardID + "@1",
		"ROLE_DIGEST=" + rule.RoleCardDigest,
		"MANUAL=" + rule.ManualPath,
		"ASSIGNMENT=" + issue.AssignmentID,
		"hq accept --event " + issue.RelatedEventID,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("issue prompt missing %q: %s", want, prompt)
		}
	}
	if strings.Contains(prompt, "固定测试角色卡") {
		t.Fatalf("issue prompt copied mutable role prose instead of a stable pointer: %s", prompt)
	}

	secondDecision := writeIssueApproval(t, e, "role-card-second-decision.md", "DEC-ROLE-CARD-002", "ROLE-CARD-002", order, "zantianyou")
	before, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	transportBefore := len(e.transport.calls)
	err = runAssignmentCommandError(t, e, "issue", "--case", "ROLE-CARD-002", "--to", "zantianyou", "--decision", secondDecision, "--next", "must be rejected")
	if err == nil || !strings.Contains(err.Error(), "max_wip=1") {
		t.Fatalf("second active assignment escaped seat WIP: %v", err)
	}
	after, readErr := NewStore(e.data).ReadAll(testConfig())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(after) != len(before) || len(e.transport.calls) != transportBefore {
		t.Fatalf("WIP rejection had side effects: events=%d->%d transport=%d->%d", len(before), len(after), transportBefore, len(e.transport.calls))
	}
}

func TestPendingIssueDeliveryReservesSeatWIP(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "pending-wip-source.md"), "# source\n")
	order := writeTestFile(t, filepath.Join(e.office, "pending-wip-order.md"), "# order\n")
	firstDecision := writeIssueApproval(t, e, "pending-wip-first.md", "DEC-PENDING-WIP-001", "PENDING-WIP-001", order, "zantianyou")
	secondDecision := writeIssueApproval(t, e, "pending-wip-second.md", "DEC-PENDING-WIP-002", "PENDING-WIP-002", order, "zantianyou")

	e.setActor(t, "penny", "pending-wip:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", "PENDING-WIP-001", "--title", "First pending assignment", "--source", source)
	runTestCommand(t, e, "case", "create", "--id", "PENDING-WIP-002", "--parent", "PENDING-WIP-001", "--title", "Second pending assignment", "--source", source)
	e.transport.result, e.transport.err = transportDefinitelyNotSent, errors.New("synthetic definitely-not-sent")
	firstErr := runAssignmentCommandError(t, e, "issue", "--case", "PENDING-WIP-001", "--to", "zantianyou", "--decision", firstDecision, "--next", "reserve the seat")
	if firstErr == nil || !strings.Contains(firstErr.Error(), "synthetic definitely-not-sent") {
		t.Fatalf("first failed delivery did not persist the expected retryable intent: %v", firstErr)
	}

	eventsBefore, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	transportBefore := len(e.transport.calls)
	secondErr := runAssignmentCommandError(t, e, "issue", "--case", "PENDING-WIP-002", "--to", "zantianyou", "--decision", secondDecision, "--next", "must not overbook")
	if secondErr == nil || !strings.Contains(secondErr.Error(), "max_wip=1") {
		t.Fatalf("pending issue did not reserve seat WIP: %v", secondErr)
	}
	eventsAfter, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfter) != len(eventsBefore) || len(e.transport.calls) != transportBefore {
		t.Fatalf("rejected overbooking had side effects: events=%d->%d transport=%d->%d", len(eventsBefore), len(eventsAfter), transportBefore, len(e.transport.calls))
	}
}

func TestRoleCardManualDriftFailsBeforeAnyBusinessWrite(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	rule, _ := cfg.exactRule("zantianyou")
	if err := os.WriteFile(filepath.Join(e.root, rule.ManualPath), []byte("# unapproved drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = newAppWithDependencies(runtimePaths{
		Office: e.office, HQRoot: e.root, DataDir: e.data, ConfigPath: e.config, HerdrBin: e.herdr,
	}, cfg, globalOptions{}, AppDependencies{
		Store: NewStore(e.data), Identity: e.identity, Transport: e.transport,
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "manual digest 漂移") {
		t.Fatalf("drifted role manual was accepted: %v", err)
	}
	if len(e.transport.calls) != 0 {
		t.Fatalf("manual drift reached transport: %+v", e.transport.calls)
	}
	if _, statErr := os.Lstat(e.data); !os.IsNotExist(statErr) {
		t.Fatalf("manual drift touched ledger path: %v", statErr)
	}
}

func TestFormalIssueActivatesOnAssignmentSeat(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	for index := range cfg.Agents {
		if cfg.Agents[index].Name == "zantianyou" {
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

	source := writeTestFile(t, filepath.Join(e.office, "activate-source.md"), "# source\n")
	order := writeTestFile(t, filepath.Join(e.office, "activate-order.md"), "# order\n")
	decision := writeIssueApproval(t, e, "activate-decision.md", "DEC-ACTIVATE-001", "ACTIVATE-001", order, "zantianyou")
	e.setActor(t, "penny", "activate:penny", e.office)
	app := e.app(t)
	state := deliveryRuntimeOffline
	resumeCalls := 0
	app.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return state, nil }
	app.DeliveryColdResume = func(target string) error {
		if target != "zantianyou" {
			t.Fatalf("activated wrong seat: %s", target)
		}
		resumeCalls++
		state = deliveryRuntimeIdle
		return nil
	}
	if err := app.run([]string{"case", "create", "--id", "ACTIVATE-001", "--title", "Activate dormant role", "--project", "test-project", "--source", source}); err != nil {
		t.Fatal(err)
	}
	if err := app.run([]string{"issue", "--case", "ACTIVATE-001", "--to", "zantianyou", "--decision", decision, "--next", "start only from durable assignment"}); err != nil {
		t.Fatal(err)
	}
	if resumeCalls != 1 || state != deliveryRuntimeIdle || len(e.transport.calls) != 1 {
		t.Fatalf("formal issue did not activate exactly one dormant seat: resumes=%d state=%s transport=%d", resumeCalls, state, len(e.transport.calls))
	}
}
