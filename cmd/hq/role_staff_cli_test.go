package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func setRegistryMutationActor(t *testing.T, e testEnv, pane string) {
	t.Helper()
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	rule, ok := cfg.exactRule("penny")
	if !ok {
		t.Fatal("test registry missing penny")
	}
	e.setActor(t, rule.Name, pane, filepath.Join(e.root, rule.WorkstationPath))
}

func TestStaffRemoveClearsBoundedKeepWarm(t *testing.T) {
	e := setupTestEnv(t)
	setRegistryMutationActor(t, e, "runtime-remove:penny")
	card := addTestRoleCard(t, e, "DEC-RUNTIME-REMOVE-ROLE", "runtime-remove-role", 1, "engineering/staff/runtime-remove-seat/v1", "review")
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	rule := AgentRule{
		Name: "runtime-remove-seat", Label: "工程部-休眠席", Nickname: "工程部-休眠席", DepartmentLabel: "engineering",
		Workspace: cfg.WorkspaceLabel, Responsibilities: []string{"staff:runtime-remove-seat"}, Department: "engineering",
		Kind: "codex", PermissionMode: "native", ReportsTo: "zantianyou", CanAccept: true,
		RoleCardID: card.ID, RoleCardVersion: card.Version, RoleCardDigest: card.Digest,
		ManualPath: card.ManualPath, WorkstationPath: filepath.Dir(card.ManualPath), ActivationPolicy: activationOnAssignment,
		KeepWarm: "45s", MaxWIP: 1, SeatVersion: 1,
	}
	rule.SeatDigest = employeeSeatDigest(rule)
	addDecision := writeApprovalDocument(t, filepath.Join(e.office, "decisions", "runtime-remove-add.md"), "DEC-RUNTIME-REMOVE-ADD", "effective", []ApprovalScope{{
		Action: "staff:add", Target: rule.Name, RequestDigest: staffScopeDigest("staff:add", rule),
	}})
	runTestCommand(t, e, "staff", "add", "--name", rule.Name, "--label", rule.Label, "--department", rule.Department,
		"--kind", rule.Kind, "--reports-to", rule.ReportsTo, "--role", roleCardKey(card.ID, card.Version),
		"--workstation", rule.WorkstationPath, "--activation", activationOnAssignment, "--keep-warm", "45s", "--max-wip", "1",
		"--grant", "accept", "--approval", addDecision)
	cfg, err = loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	added, ok := cfg.exactRule(rule.Name)
	if !ok || added.KeepWarm != "45s" {
		t.Fatalf("staff add lost keep_warm: %+v", added)
	}
	expected := added
	expected.Disabled, expected.ActivationPolicy, expected.KeepWarm = true, activationManual, ""
	finalizeTestSeatMutation(&expected)
	removeDecision := writeApprovalDocument(t, filepath.Join(e.office, "decisions", "runtime-remove.md"), "DEC-RUNTIME-REMOVE", "effective", []ApprovalScope{{
		Action: "staff:remove", Target: expected.Name, RequestDigest: staffScopeDigest("staff:remove", expected),
	}})
	runTestCommand(t, e, "staff", "remove", "--name", expected.Name, "--approval", removeDecision)
	cfg, err = loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	removed, ok := configRuleIncludingDisabled(cfg, expected.Name)
	if !ok || !removed.Disabled || removed.ActivationPolicy != activationManual || removed.KeepWarm != "" || removed.SeatDigest != expected.SeatDigest {
		t.Fatalf("staff remove did not clear keep_warm atomically: %+v", removed)
	}
}

func addTestRoleCard(t *testing.T, e testEnv, decisionID, id string, version int, workstation string, capabilities ...string) RoleCard {
	t.Helper()
	manual := filepath.Join(workstation, "AGENTS.md")
	raw := []byte("# " + roleCardKey(id, version) + "\n\nIndependent immutable role manual.\n")
	writeTestFile(t, filepath.Join(e.root, manual), string(raw))
	canonicalCapabilities, err := canonicalStringSet(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	card := RoleCard{
		ID: id, Version: version, Label: "Engineering Reviewer", Department: "engineering",
		Capabilities: canonicalCapabilities, ManualPath: manual,
		ManualDigest: roleCardFileDigest(raw), Status: roleCardApproved,
	}
	card.Digest = roleCardDigest(card)
	decision := writeApprovalDocument(t, filepath.Join(e.office, "decisions", strings.ToLower(decisionID)+".md"), decisionID, "effective", []ApprovalScope{{
		Action: "role:add", Target: roleCardKey(id, version), RequestDigest: roleScopeDigest("role:add", card),
	}})
	args := []string{
		"role", "add", "--id", id, "--version", strconv.Itoa(version), "--label", card.Label,
		"--department", card.Department, "--manual", card.ManualPath, "--approval", decision,
	}
	for _, capability := range card.Capabilities {
		args = append(args, "--capability", capability)
	}
	runTestCommand(t, e, args...)
	return card
}

func TestRoleAndStaffSeatLifecycle(t *testing.T) {
	e := setupTestEnv(t)
	setRegistryMutationActor(t, e, "role-seat:penny")

	roleV1 := addTestRoleCard(t, e, "DEC-ROLE-ADD-ENG-1", "engineering-reviewer", 1, "engineering/staff/reviewer-seat/v1", "accept", "manage_staff")
	loaded, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	storedV1, ok := loaded.roleCard(roleV1.ID, roleV1.Version)
	if !ok || storedV1.Digest != roleV1.Digest || storedV1.ManualDigest != roleV1.ManualDigest || storedV1.Status != roleCardApproved {
		t.Fatalf("role add did not freeze the expected card: %+v", storedV1)
	}
	if output := runTestCommand(t, e, "role", "list"); !strings.Contains(output, "engineering-reviewer@1") || !strings.Contains(output, "accept,manage_staff") {
		t.Fatalf("role list omitted added card capabilities: %s", output)
	}
	if output := runTestCommand(t, e, "role", "show", "--role", "engineering-reviewer@1"); !strings.Contains(output, roleV1.Digest) || !strings.Contains(output, "manage_staff") {
		t.Fatalf("role show omitted frozen digest or capabilities: %s", output)
	}

	// The same id@version is immutable. Reusing the original approval and
	// manual must fail before any candidate config replacement.
	beforeDuplicate, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	app := e.app(t)
	err = app.run([]string{
		"role", "add", "--id", roleV1.ID, "--version", "1", "--label", roleV1.Label,
		"--department", roleV1.Department, "--manual", roleV1.ManualPath,
		"--capability", "accept", "--capability", "manage_staff", "--approval", roleV1.ApprovalRef,
	})
	if err == nil || !strings.Contains(err.Error(), "不可变") {
		t.Fatalf("duplicate immutable role version was not rejected: %v", err)
	}
	afterDuplicate, readErr := os.ReadFile(e.config)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(beforeDuplicate, afterDuplicate) {
		t.Fatal("rejected immutable role add changed config bytes")
	}

	roleV2 := addTestRoleCard(t, e, "DEC-ROLE-ADD-ENG-2", "engineering-reviewer", 2, "engineering/staff/reviewer-seat/v2", "accept", "manage_staff")
	loaded, err = loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	newSeat := AgentRule{
		Name: "eng-reviewer-seat", Label: "工程部-独立审查席", Nickname: "工程部-独立审查席", DepartmentLabel: "engineering",
		Workspace: loaded.WorkspaceLabel, Responsibilities: []string{"staff:eng-reviewer-seat"},
		ManualPath: roleV1.ManualPath, RoleCardID: roleV1.ID, RoleCardVersion: roleV1.Version, RoleCardDigest: roleV1.Digest,
		WorkstationPath: filepath.Dir(roleV1.ManualPath), ActivationPolicy: activationOnAssignment, MaxWIP: 2, SeatVersion: 1,
		Department: "engineering", Kind: "codex", PermissionMode: "native", ReportsTo: "zantianyou", CanAccept: true,
	}
	newSeat.SeatDigest = employeeSeatDigest(newSeat)
	staffAddDecision := writeApprovalDocument(t, filepath.Join(e.office, "decisions", "staff-seat-add.md"), "DEC-STAFF-SEAT-ADD", "effective", []ApprovalScope{{
		Action: "staff:add", Target: newSeat.Name, RequestDigest: staffScopeDigest("staff:add", newSeat),
	}})
	runTestCommand(t, e, "staff", "add",
		"--name", newSeat.Name, "--label", newSeat.Label, "--department", newSeat.Department,
		"--kind", newSeat.Kind, "--reports-to", newSeat.ReportsTo, "--role", roleCardKey(roleV1.ID, roleV1.Version),
		"--workstation", newSeat.WorkstationPath, "--activation", activationOnAssignment, "--max-wip", "2",
		"--grant", "accept", "--approval", staffAddDecision)
	loaded, err = loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	added, ok := loaded.exactRule(newSeat.Name)
	if !ok || added.SeatVersion != 1 || added.SeatDigest != newSeat.SeatDigest || added.WorkstationPath != newSeat.WorkstationPath {
		t.Fatalf("staff add did not bind the expected seat contract: %+v", added)
	}
	if added.CanManageStaff {
		t.Fatal("role prose/capability manage_staff incorrectly granted the seat can_manage_staff permission")
	}

	retiredV1 := storedV1
	retiredV1.Status = roleCardRetired
	retireV1Decision := writeApprovalDocument(t, filepath.Join(e.office, "decisions", "role-v1-retire.md"), "DEC-ROLE-RETIRE-ENG-1", "effective", []ApprovalScope{{
		Action: "role:retire", Target: roleCardKey(roleV1.ID, roleV1.Version), RequestDigest: roleScopeDigest("role:retire", retiredV1),
	}})
	beforeBoundRetire, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	app = e.app(t)
	err = app.run([]string{"role", "retire", "--role", roleCardKey(roleV1.ID, roleV1.Version), "--approval", retireV1Decision})
	if err == nil || !strings.Contains(err.Error(), "仍被员工") {
		t.Fatalf("bound role card retirement was not rejected: %v", err)
	}
	afterBoundRetire, readErr := os.ReadFile(e.config)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(beforeBoundRetire, afterBoundRetire) {
		t.Fatal("rejected bound role retirement changed config bytes")
	}

	updatedSeat := added
	updatedSeat.RoleCardID, updatedSeat.RoleCardVersion, updatedSeat.RoleCardDigest = roleV2.ID, roleV2.Version, roleV2.Digest
	updatedSeat.ManualPath, updatedSeat.WorkstationPath = roleV2.ManualPath, filepath.Dir(roleV2.ManualPath)
	updatedSeat.ActivationPolicy, updatedSeat.MaxWIP = activationAlways, 3
	updatedSeat.SeatVersion++
	updatedSeat.SeatDigest = employeeSeatDigest(updatedSeat)
	staffUpdateDecision := writeApprovalDocument(t, filepath.Join(e.office, "decisions", "staff-seat-update.md"), "DEC-STAFF-SEAT-UPDATE", "effective", []ApprovalScope{{
		Action: "staff:update", Target: updatedSeat.Name, RequestDigest: staffScopeDigest("staff:update", updatedSeat),
	}})
	runTestCommand(t, e, "staff", "update", "--name", updatedSeat.Name,
		"--role", roleCardKey(roleV2.ID, roleV2.Version), "--workstation", updatedSeat.WorkstationPath,
		"--activation", activationAlways, "--max-wip", "3", "--approval", staffUpdateDecision)
	loaded, err = loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	updated, ok := loaded.exactRule(updatedSeat.Name)
	if !ok || updated.RoleCardVersion != 2 || updated.SeatVersion != 2 || updated.SeatDigest != updatedSeat.SeatDigest || updated.ActivationPolicy != activationAlways {
		t.Fatalf("staff update did not atomically advance the seat contract: %+v", updated)
	}

	// Once no seat references v1 it may be retired, without changing the
	// immutable contract digest.
	runTestCommand(t, e, "role", "retire", "--role", roleCardKey(roleV1.ID, roleV1.Version), "--approval", retireV1Decision)
	loaded, err = loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	retiredStored, ok := loaded.roleCard(roleV1.ID, roleV1.Version)
	if !ok || retiredStored.Status != roleCardRetired || retiredStored.Digest != roleV1.Digest {
		t.Fatalf("role retirement changed immutable fields: %+v", retiredStored)
	}

	removedSeat := updated
	removedSeat.Disabled, removedSeat.ActivationPolicy = true, activationManual
	removedSeat.SeatVersion++
	removedSeat.SeatDigest = employeeSeatDigest(removedSeat)
	removeDecision := writeApprovalDocument(t, filepath.Join(e.office, "decisions", "staff-seat-remove.md"), "DEC-STAFF-SEAT-REMOVE", "effective", []ApprovalScope{{
		Action: "staff:remove", Target: removedSeat.Name, RequestDigest: staffScopeDigest("staff:remove", removedSeat),
	}})
	runTestCommand(t, e, "staff", "remove", "--name", removedSeat.Name, "--approval", removeDecision)

	retiredV2 := roleV2
	retiredV2.Status = roleCardRetired
	retireV2Decision := writeApprovalDocument(t, filepath.Join(e.office, "decisions", "role-v2-retire.md"), "DEC-ROLE-RETIRE-ENG-2", "effective", []ApprovalScope{{
		Action: "role:retire", Target: roleCardKey(roleV2.ID, roleV2.Version), RequestDigest: roleScopeDigest("role:retire", retiredV2),
	}})
	beforeDisabledBoundRetire, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	app = e.app(t)
	err = app.run([]string{"role", "retire", "--role", roleCardKey(roleV2.ID, roleV2.Version), "--approval", retireV2Decision})
	if err == nil || !strings.Contains(err.Error(), "仍被员工") {
		t.Fatalf("disabled but still-bound role card retirement was not rejected: %v", err)
	}
	afterDisabledBoundRetire, readErr := os.ReadFile(e.config)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(beforeDisabledBoundRetire, afterDisabledBoundRetire) {
		t.Fatal("rejected disabled-seat role retirement changed config bytes")
	}
}

func TestRoleCommandsAreRegisteredInCobra(t *testing.T) {
	root := newCobraRootCommand(globalOptions{}, io.Discard, io.Discard)
	for _, path := range [][]string{{"role", "list"}, {"role", "show"}, {"role", "add"}, {"role", "retire"}} {
		command, remaining, err := root.Find(path)
		if err != nil || len(remaining) != 0 || command.CommandPath() != "hq "+strings.Join(path, " ") {
			t.Fatalf("role command %v is not registered: command=%v remaining=%v err=%v", path, command, remaining, err)
		}
	}
	add, _, _ := root.Find([]string{"role", "add"})
	for _, flag := range []string{"id", "version", "label", "department", "capability", "manual", "approval"} {
		if add.Flags().Lookup(flag) == nil {
			t.Fatalf("role add missing --%s", flag)
		}
	}
	staffAdd, _, _ := root.Find([]string{"staff", "add"})
	for _, flag := range []string{"role", "workstation", "activation", "max-wip", "permission-mode"} {
		if staffAdd.Flags().Lookup(flag) == nil {
			t.Fatalf("staff add missing --%s", flag)
		}
	}
	staffUpdate, _, _ := root.Find([]string{"staff", "update"})
	if staffAdd.Flags().Lookup("auto-start") != nil || staffUpdate.Flags().Lookup("auto-start") != nil {
		t.Fatal("undeclared --auto-start flag must not be registered")
	}
}

func TestRoleApprovalScopeRequiresExactVersionedTarget(t *testing.T) {
	validDigest := strings.Repeat("a", 64)
	for _, action := range []string{"role:add", "role:retire"} {
		if err := validateApprovalScopeShape(ApprovalScope{Action: action, Target: "engineering-reviewer@2", RequestDigest: validDigest}); err != nil {
			t.Fatalf("valid %s scope rejected: %v", action, err)
		}
	}
	for _, scope := range []ApprovalScope{
		{Action: "role:add", Target: "engineering-reviewer", RequestDigest: validDigest},
		{Action: "role:add", Target: "engineering-reviewer@0", RequestDigest: validDigest},
		{Action: "role:retire", Target: "engineering-reviewer@2", RequestDigest: "not-a-digest"},
		{Action: "role:retire", Target: "engineering-reviewer@2", RequestDigest: validDigest, CaseID: "CASE-001"},
	} {
		if err := validateApprovalScopeShape(scope); err == nil {
			t.Fatalf("invalid role approval scope accepted: %+v", scope)
		}
	}
}
