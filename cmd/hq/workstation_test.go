package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndependentEmployeeWorkstationsStartAndBindExactly(t *testing.T) {
	e := setupTestEnv(t)
	cfg := testConfig()
	var rules []AgentRule
	for index := range cfg.Agents {
		if cfg.Agents[index].Name != "eng-data-engineer" && cfg.Agents[index].Name != "eng-developer" {
			continue
		}
		cfg.Agents[index].ActivationPolicy = activationAlways
		cfg.Agents[index].SeatDigest = employeeSeatDigest(cfg.Agents[index])
		rules = append(rules, cfg.Agents[index])
	}
	if len(rules) != 2 {
		t.Fatalf("fixture rules=%d", len(rules))
	}

	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	app := &App{
		Office: e.office, HQRoot: e.root, DataDir: e.data, Config: cfg, Herdr: control,
		Sessions: &FileSessionStore{Root: filepath.Join(e.data, "independent-workstation-sessions")},
		Out:      io.Discard, Err: io.Discard,
	}
	for _, rule := range rules {
		if err := app.startHQAgentAdmitted(context.Background(), "w-test", rule); err != nil {
			t.Fatalf("start %s: %v", rule.Name, err)
		}
	}

	snapshot, err := app.herdrSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range rules {
		want := filepath.Join(e.root, rule.WorkstationPath)
		binding, err := ResolveLiveBinding(snapshot, cfg, e.root, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
		if err != nil {
			t.Fatalf("bind %s: %v", rule.Name, err)
		}
		if binding.CWD != want {
			t.Fatalf("%s cwd=%s want=%s", rule.Name, binding.CWD, want)
		}
	}
	healthyPatrol := analyzePatrolSnapshot(snapshot, cfg, e.root).report
	if healthyPatrol.Drift != 0 || healthyPatrol.Orphan != 0 {
		t.Fatalf("independent workstations rejected by patrol: %+v", healthyPatrol.Findings)
	}
	if items, err := planEstopItems(snapshot, cfg, e.root); err != nil || len(items) != len(rules) {
		t.Fatalf("independent workstations rejected by ESTOP plan: items=%+v err=%v", items, err)
	}

	control.mu.Lock()
	calls := strings.Join(control.calls, "\n")
	control.mu.Unlock()
	for _, rule := range rules {
		if want := "WORKSTATION=" + filepath.Join(e.root, rule.WorkstationPath); !strings.Contains(calls, want) {
			t.Fatalf("startup prompt missing %q: %s", want, calls)
		}
	}

	swapped := cloneHerdrSnapshot(snapshot)
	firstCWD := filepath.Join(e.root, rules[0].WorkstationPath)
	secondCWD := filepath.Join(e.root, rules[1].WorkstationPath)
	for index := range swapped.Agents {
		if swapped.Agents[index].Name == rules[0].Name {
			swapped.Agents[index].CWD = secondCWD
		} else if swapped.Agents[index].Name == rules[1].Name {
			swapped.Agents[index].CWD = firstCWD
		}
	}
	for index := range swapped.Tabs {
		if swapped.Tabs[index].Label == rosterTabLabel(rules[0]) {
			swapped.Tabs[index].CWD = secondCWD
		} else if swapped.Tabs[index].Label == rosterTabLabel(rules[1]) {
			swapped.Tabs[index].CWD = firstCWD
		}
	}
	for index := range swapped.Panes {
		for _, agent := range swapped.Agents {
			if agent.PaneID == swapped.Panes[index].ID {
				swapped.Panes[index].CWD = agent.CWD
			}
		}
	}
	for _, rule := range rules {
		if _, err := ResolveLiveBinding(swapped, cfg, e.root, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true}); err == nil {
			t.Fatalf("%s accepted another employee's workstation", rule.Name)
		}
	}
	wrongPatrol := analyzePatrolSnapshot(swapped, cfg, e.root).report
	if wrongPatrol.Drift == 0 && wrongPatrol.Orphan == 0 {
		t.Fatalf("patrol accepted mutually swapped workstations: %+v", wrongPatrol)
	}
	if _, err := planEstopItems(swapped, cfg, e.root); err == nil {
		t.Fatal("ESTOP plan accepted mutually swapped workstations")
	}
}

func testRuleCWD(root string, rule AgentRule) string {
	return filepath.Join(root, rule.WorkstationPath)
}

func testAgentCWD(cfg Config, root, name string) string {
	rule, ok := cfg.ruleFor(name)
	if !ok {
		return ""
	}
	return testRuleCWD(root, rule)
}

func materializeTestWorkstations(t *testing.T, root string, cfg Config) {
	t.Helper()
	for _, rule := range cfg.Agents {
		if err := os.MkdirAll(testRuleCWD(root, rule), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rule.ManualPath), testRoleManual(rule.Name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolveAgentWorkstationRequiresCanonicalWorkspaceRelativePath(t *testing.T) {
	e := setupTestEnv(t)
	rule, _ := testConfig().exactRule("eng-developer")
	rule.WorkstationPath = ""
	if _, err := resolveAgentWorkstation(e.root, rule); err == nil {
		t.Fatal("accepted empty workstation_path")
	}

	for _, invalid := range []string{"../outside", filepath.Join(e.root, "engineering")} {
		rule.WorkstationPath = invalid
		if _, err := resolveAgentWorkstation(e.root, rule); err == nil {
			t.Fatalf("accepted invalid workstation_path=%q", invalid)
		}
	}

	target := filepath.Join(e.root, "engineering", "staff", "real-seat", "v1")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(e.root, "engineering", "staff", "alias-seat", "v1")
	if err := os.MkdirAll(filepath.Dir(alias), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	rule.WorkstationPath = "engineering/staff/alias-seat/v1"
	if _, err := resolveAgentWorkstation(e.root, rule); err == nil {
		t.Fatal("accepted symlink workstation alias")
	}
}

func TestPersonalWorkstationLayoutRejectsDepartmentRootsAndNestedSharing(t *testing.T) {
	valid := "engineering/staff/reviewer-seat/v2"
	if err := validatePersonalWorkstationPath("engineering", valid, 2); err != nil {
		t.Fatalf("valid personal workstation rejected: %v", err)
	}
	if err := validatePersonalManualPath("engineering", filepath.Join(valid, "AGENTS.md"), 2); err != nil {
		t.Fatalf("valid personal manual rejected: %v", err)
	}
	for _, invalid := range []string{
		"engineering",
		"engineering/staff",
		"engineering/staff/reviewer-seat",
		"engineering/staff/reviewer-seat/v1",
		"engineering/staff/reviewer-seat/v2/nested",
		"engineering/staff/reviewer-seat/v2/AGENTS.md",
	} {
		if err := validatePersonalWorkstationPath("engineering", invalid, 2); err == nil {
			t.Fatalf("accepted department root, ancestor, descendant, or wrong version: %s", invalid)
		}
	}
	for _, invalid := range []string{
		"engineering/AGENTS.md",
		"engineering/staff/reviewer-seat/v2/manual.md",
		"engineering/staff/reviewer-seat/v2/nested/AGENTS.md",
	} {
		if err := validatePersonalManualPath("engineering", invalid, 2); err == nil {
			t.Fatalf("accepted manual outside exact workstation root: %s", invalid)
		}
	}
}

func TestRegistryRejectsSharedManualBetweenUnboundRoleCards(t *testing.T) {
	cfg := testConfig()
	shared := cfg.RoleCards[0]
	shared.ID = "another-unbound-role"
	shared.Digest = roleCardDigest(shared)
	cfg.RoleCards = append(cfg.RoleCards, shared)
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "role card manual 必须独立") {
		t.Fatalf("registry accepted two role cards sharing one AGENTS.md: %v", err)
	}
}

func TestRoleManualSecureOpenRejectsSymlinkAndReplacement(t *testing.T) {
	e := setupTestEnv(t)
	rule, _ := testConfig().exactRule("eng-developer")
	manual := filepath.Join(e.root, rule.ManualPath)
	target := writeTestFile(t, filepath.Join(e.root, "replacement-role-manual.md"), "# replacement\n")

	t.Run("initial symlink", func(t *testing.T) {
		if err := os.Remove(manual); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, manual); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readRoleManualFile(e.root, manual, "test role manual", nil); err == nil {
			t.Fatal("secure role manual read accepted symlink")
		}
		if err := os.Remove(manual); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, manual, string(testRoleManual(rule.Name)))
	})

	t.Run("replacement between check and open", func(t *testing.T) {
		beforeOpen := func(path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Symlink(target, path)
		}
		if _, _, err := readRoleManualFile(e.root, manual, "test role manual", beforeOpen); err == nil {
			t.Fatal("secure role manual read accepted symlink replacement")
		}
	})
}

func TestRoleManualReadRejectsOversizeArtifact(t *testing.T) {
	root := canonicalTestTempDir(t)
	manual := filepath.Join(root, "engineering", "staff", "oversize", "v1", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(manual), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manual, []byte(strings.Repeat("x", int(maxRoleManualBytes)+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readRoleManualFile(root, manual, "oversize role manual", nil); err == nil || !strings.Contains(err.Error(), "超过最大") {
		t.Fatalf("oversize role manual was accepted: %v", err)
	}
}

func TestStartAndColdResumeRecheckRoleManualBeforeMutation(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	rule, _ := cfg.exactRule("eng-developer")
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	app := &App{
		Office: e.office, HQRoot: e.root, DataDir: e.data, Config: cfg, Herdr: control,
		Sessions: &FileSessionStore{Root: filepath.Join(e.data, "digest-recheck-sessions")},
		Out:      io.Discard, Err: io.Discard,
	}
	if err := os.WriteFile(filepath.Join(e.root, rule.ManualPath), []byte("# unapproved runtime drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.startHQAgentAdmitted(context.Background(), "w-test", rule); err == nil || !strings.Contains(err.Error(), "manual digest 漂移") {
		t.Fatalf("start accepted drifted role manual: %v", err)
	}
	control.mu.Lock()
	calls := append([]string(nil), control.calls...)
	control.mu.Unlock()
	for _, call := range calls {
		if strings.HasPrefix(call, "tab create") {
			t.Fatalf("drifted start reached CreateTab: %v", calls)
		}
	}

	resumeCalls := 0
	app.DeliveryColdResume = func(string) error {
		resumeCalls++
		return nil
	}
	if err := app.coldResumeDeliveryTargetAdmitted(rule.Name); err == nil || !strings.Contains(err.Error(), "manual digest 漂移") {
		t.Fatalf("cold resume accepted drifted role manual: %v", err)
	}
	if resumeCalls != 0 {
		t.Fatalf("cold resume hook ran before role manual verification: %d", resumeCalls)
	}
}
