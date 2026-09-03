package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func assertResolvedMessageHasNoPhantomCase(t *testing.T, e testEnv) {
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
	if len(events) == 0 {
		t.Fatal("resolved message fixture has no events")
	}
	wantTail := events[len(events)-1]
	for _, snapshot := range []Snapshot{
		func() Snapshot {
			value, snapshotErr := store.Snapshot(cfg)
			if snapshotErr != nil {
				t.Fatal(snapshotErr)
			}
			return value
		}(),
		func() Snapshot {
			value, rebuildErr := store.Rebuild(cfg)
			if rebuildErr != nil {
				t.Fatal(rebuildErr)
			}
			return value
		}(),
	} {
		if _, exists := snapshot.Cases[""]; exists || len(snapshot.Cases) != 0 {
			t.Fatalf("case-less message resolution polluted cases: %+v", snapshot.Cases)
		}
		if snapshot.EventCount != len(events) || snapshot.LastSequence != wantTail.Sequence ||
			snapshot.LastEventHash != wantTail.EventHash || snapshot.GeneratedAt != wantTail.At {
			t.Fatalf("neutral resolution lost snapshot tail metadata: %+v tail=%+v", snapshot, wantTail)
		}
	}

	e.setActor(t, "penny", "snapshot-resolution:project", e.office)
	output, err := runProjectTestCommand(t, e, true, "project", "list")
	if err != nil {
		t.Fatal(err)
	}
	var projects ProjectListView
	if err := json.Unmarshal([]byte(output), &projects); err != nil {
		t.Fatal(err)
	}
	if projects.ProjectCount != 0 || projects.AssignedCaseCount != 0 {
		t.Fatalf("case-less resolution polluted Project View: %+v", projects)
	}
}

func TestMessageDeliveryResolutionIsSnapshotNeutralWithoutCase(t *testing.T) {
	for _, outcome := range []string{"delivered", "not-delivered"} {
		t.Run(outcome, func(t *testing.T) {
			e := setupTestEnv(t)
			setDeliveryPolicyWithBundle(t, e, deliveryModeAuto, 10, 2, 64*1024)
			sender := deliveryPolicyTestApp(t, e, "eng-developer", "snapshot-resolution:sender")
			sender.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeIdle, nil }
			e.transport.result = transportAmbiguous
			e.transport.err = errors.New("synthetic ambiguous message delivery")
			if _, err := runDeliveryPolicyTest(sender, "message", "--to", "zantianyou", "--kind", "request",
				"--text", "case-less resolution", "--delivery", "wakeup"); err == nil {
				t.Fatal("ambiguous message unexpectedly succeeded")
			}
			messages := deliveryPolicyMessages(t, e)
			origin := messages[len(messages)-1]
			if origin.CaseID != "" {
				t.Fatalf("message fixture unexpectedly has case_id=%q", origin.CaseID)
			}
			evidence := writeTestFile(t, filepath.Join(e.office, "message-resolution-"+outcome+".md"), "# delivery evidence\n")
			resolver := deliveryPolicyTestApp(t, e, "penny", "snapshot-resolution:resolver")
			if _, err := runDeliveryPolicyTest(resolver, "delivery", "resolve", "--id", origin.DeliveryID,
				"--outcome", outcome, "--reason", "operator verified message outcome", "--evidence", evidence); err != nil {
				t.Fatal(err)
			}
			assertResolvedMessageHasNoPhantomCase(t, e)
		})
	}
}

func TestResolvedSentIssueAndReportStillAdvanceCaseProjection(t *testing.T) {
	e := setupTestEnv(t)
	caseID := "RESOLUTION-PROJECTION"
	source := writeTestFile(t, filepath.Join(e.office, "resolution-projection-source.md"), "# source\n")
	order := writeTestFile(t, filepath.Join(e.office, "resolution-projection-order.md"), "# order\n")
	decision := writeIssueApproval(t, e, "resolution-projection-decision.md", "DEC-RESOLUTION-PROJECTION", caseID, order, "zantianyou")
	evidence := writeTestFile(t, filepath.Join(e.office, "resolution-projection-evidence.md"), "# receiver evidence\n")

	e.setActor(t, "penny", "resolution-projection:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "resolution projection", "--project", "projection", "--source", source)
	e.transport.result, e.transport.err = transportAmbiguous, errors.New("synthetic ambiguous issue")
	if err := runAssignmentCommandError(t, e, "issue", "--case", caseID, "--to", "zantianyou", "--decision", decision, "--next", "work"); err == nil {
		t.Fatal("ambiguous issue unexpectedly succeeded")
	}
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	issueOrigin := latestEventOfType(events, "issue_prepared")
	runTestCommand(t, e, "delivery", "resolve", "--id", issueOrigin.DeliveryID, "--outcome", "delivered",
		"--reason", "receiver confirmed issue", "--evidence", evidence)
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	resolvedIssue := latestEventOfType(events, "delivery_resolved_sent")
	snapshot, err := NewStore(e.data).Snapshot(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if state := snapshot.Cases[caseID]; state == nil || state.Status != string(statusDispatched) || state.Owner != "zantianyou" || state.LastEventID != resolvedIssue.ID {
		t.Fatalf("resolved-sent issue did not advance case projection: %+v", state)
	}

	e.setActor(t, "zantianyou", "resolution-projection:manager", filepath.Join(e.root, "engineering"))
	e.transport.result, e.transport.err = transportSent, nil
	runTestCommand(t, e, "accept", "--event", resolvedIssue.ID, "--next", "work")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "resolution-projection-result.md"), "# result\n")
	e.transport.result, e.transport.err = transportAmbiguous, errors.New("synthetic ambiguous report")
	if err := runAssignmentCommandError(t, e, "report", "--case", caseID, "--result", "completed", "--artifact", artifact, "--verify", "checked", "--next", "review"); err == nil {
		t.Fatal("ambiguous report unexpectedly succeeded")
	}
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	reportOrigin := latestEventOfType(events, "report_prepared")
	e.setActor(t, "penny", "resolution-projection:penny", e.office)
	runTestCommand(t, e, "delivery", "resolve", "--id", reportOrigin.DeliveryID, "--outcome", "delivered",
		"--reason", "receiver confirmed report", "--evidence", evidence)
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	resolvedReport := latestEventOfType(events, "delivery_resolved_sent")
	snapshot, err = NewStore(e.data).Rebuild(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if state := snapshot.Cases[caseID]; state == nil || state.Status != string(statusReported) || state.Owner != "penny" || state.LastEventID != resolvedReport.ID {
		t.Fatalf("resolved-sent report did not advance case projection: %+v", state)
	}
}
