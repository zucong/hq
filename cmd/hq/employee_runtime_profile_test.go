package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestEmployeeRuntimeProfileStartupAndInspectionAgree(t *testing.T) {
	_, cfg, rule, _, reader, _ := profileRepairFixture(t, "idle")
	policy := cfg.RuntimeProfiles["codex"]
	policy.Employees = map[string]EmployeeRuntimeProfile{rule.Name: {Model: "gpt-6", ReasoningEffort: "high"}}
	cfg.RuntimeProfiles["codex"] = policy
	if err := validateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	before := append([]string(nil), rule.AgentArgs...)
	args, err := nativeAgentArgsForConfig(cfg, rule)
	if err != nil {
		t.Fatal(err)
	}
	model, effort, err := codexRuntimeProfileOverrides(args)
	if err != nil || model != "gpt-6" || effort != "high" {
		t.Fatalf("args=%v err=%v", args, err)
	}
	if !reflect.DeepEqual(before, rule.AgentArgs) {
		t.Fatal("mutated seat args")
	}
	expected, _, mismatch, err := inspectLiveRuntimeProfile(context.Background(), reader, cfg, LiveBinding{Kind: "codex", Seat: rule.Name})
	if err != nil || expected.Model != "gpt-6" || !strings.Contains(mismatch, "gpt-6") {
		t.Fatalf("expected=%+v mismatch=%s err=%v", expected, mismatch, err)
	}
	other, _ := cfg.exactRule("penny")
	defaultProfile, _ := runtimeProfileForEmployee(cfg, other.Kind, other.Name)
	if defaultProfile.Model != "gpt-5.6-sol" {
		t.Fatalf("other seat changed: %+v", defaultProfile)
	}
}

func TestEmployeeRuntimeProfileManagerAuthorityAndNoSeatMutation(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := mutateConfig(e.config, func(cfg *Config) error { cfg.RuntimeProfiles = configuredCodexProfile(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	target, _ := cfg.exactRule("zantianyou")
	e.setActor(t, "penny", "runtime-update:admin", testAgentCWD(cfg, e.root, "penny"))
	runTestCommand(t, e, "staff", "update", "--name", target.Name, "--model", "gpt-6", "--effort", "medium")
	after, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := after.exactRule(target.Name)
	if !reflect.DeepEqual(target, updated) {
		t.Fatal("runtime update changed seat")
	}
	p, _ := runtimeProfileForEmployee(after, "codex", target.Name)
	if p.Model != "gpt-6" {
		t.Fatalf("profile=%+v", p)
	}
	e.setActor(t, "zantianyou", "runtime-update:manager", testAgentCWD(after, e.root, "zantianyou"))
	app := e.app(t)
	err = app.run([]string{"staff", "update", "--name", "penny", "--model", "gpt-6", "--effort", "medium"})
	if err == nil || !strings.Contains(err.Error(), "直属经理") {
		t.Fatalf("upward mutation allowed: %v", err)
	}
}

func TestEmployeeRuntimeProfileStripsOnlyModelOptions(t *testing.T) {
	args := []string{"--model=old", "-c", `model_reasoning_effort="low"`, "--config=feature=true", "--sandbox", "danger-full-access"}
	got, err := withoutCodexModelArgs(args)
	want := []string{"--config=feature=true", "--sandbox", "danger-full-access"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

func TestEmployeeRuntimeProfileDirectManagerAndActiveAssignment(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := mutateConfig(e.config, func(c *Config) error { c.RuntimeProfiles = configuredCodexProfile(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	e.setActor(t, "zantianyou", "model-manager", testAgentCWD(cfg, e.root, "zantianyou"))
	runTestCommand(t, e, "staff", "update", "--name", "eng-developer", "--model", "gpt-6", "--effort", "medium")
	after, err := loadConfig(e.config)
	if err != nil || after.RuntimeProfiles["codex"].Employees["eng-developer"].Model != "gpt-6" {
		t.Fatalf("manager update failed: %v", err)
	}

	busy, busyCfg, _, _ := assignmentProgressFixture(t, "MODEL-BUSY")
	_, err = mutateConfig(busy.config, func(c *Config) error { c.RuntimeProfiles = configuredCodexProfile(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	busy.setActor(t, "zantianyou", "model-manager-busy", testAgentCWD(busyCfg, busy.root, "zantianyou"))
	err = busy.app(t).run([]string{"staff", "update", "--name", "eng-developer", "--model", "gpt-6", "--effort", "medium"})
	if err == nil || !strings.Contains(err.Error(), "在途 assignment") {
		t.Fatalf("active change allowed: %v", err)
	}
	unchanged, _ := loadConfig(busy.config)
	if len(unchanged.RuntimeProfiles["codex"].Employees) != 0 {
		t.Fatal("rejected change wrote config")
	}
}

func TestEmployeeRuntimeProfileRecoveryUsesOverride(t *testing.T) {
	e, _, rule, app, reader, _ := profileRepairFixture(t, "idle")
	cfg, err := mutateConfig(e.config, func(c *Config) error {
		p := c.RuntimeProfiles["codex"]
		p.Employees = map[string]EmployeeRuntimeProfile{rule.Name: {Model: "gpt-6", ReasoningEffort: "high"}}
		c.RuntimeProfiles["codex"] = p
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	app.Config = cfg
	reader.afterStart = []byte("gpt-6 high · 100% left · ~/work\n› Ask Codex to do anything\n")
	if err := app.recoverRuntimeProfileDriftsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	reader.mu.Lock()
	profiles := reader.startProfiles
	reader.mu.Unlock()
	if len(profiles) != 1 {
		t.Fatalf("start profiles=%v", profiles)
	}
	m, effort, err := codexRuntimeProfileOverrides(profiles[0])
	if err != nil || m != "gpt-6" || effort != "high" {
		t.Fatalf("wrong recovered profile: %v", profiles)
	}
	if err := app.recoverRuntimeProfileDriftsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.startProfiles) != 1 {
		t.Fatal("repeated replacement")
	}
}

func TestEmployeeRuntimeProfileAdmissionWaitsForActualModel(t *testing.T) {
	_, cfg, rule, app, reader, _ := profileRepairFixture(t, "idle")
	p := cfg.RuntimeProfiles["codex"]
	p.Employees = map[string]EmployeeRuntimeProfile{rule.Name: {Model: "gpt-6", ReasoningEffort: "medium"}}
	cfg.RuntimeProfiles["codex"] = p
	app.Config = cfg
	if err := app.ensureEmployeeModelReady(rule); err == nil || !strings.Contains(err.Error(), "尚未创建 assignment") {
		t.Fatalf("old live model admitted: %v", err)
	}
	reader.mu.Lock()
	reader.terminal = []byte("gpt-6 medium · 100% left · ~/work\n› Ask Codex to do anything\n")
	reader.mu.Unlock()
	if err := app.ensureEmployeeModelReady(rule); err != nil {
		t.Fatalf("matching model rejected: %v", err)
	}
}

func TestEmployeeRuntimeProfileCLIAndDryRun(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := mutateConfig(e.config, func(c *Config) error { c.RuntimeProfiles = configuredCodexProfile(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	e.setActor(t, "zantianyou", "model-dryrun", testAgentCWD(cfg, e.root, "zantianyou"))
	for _, flags := range [][]string{
		{"--model", "gpt-6"},
		{"--effort", "medium"},
		{"--model", "gpt-6", "--effort", "invalid"},
		{"--model", "gpt-6", "--effort", "medium", "--grant", "can_manage_staff"},
	} {
		args := append([]string{"staff", "update", "--name", "eng-developer"}, flags...)
		if err := e.app(t).run(args); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
	app := e.app(t)
	app.DryRun = true
	if err := app.run([]string{"staff", "update", "--name", "eng-developer", "--model", "gpt-6", "--effort", "medium"}); err != nil {
		t.Fatal(err)
	}
	after, err := loadConfig(e.config)
	if err != nil || !reflect.DeepEqual(cfg, after) {
		t.Fatalf("dry-run changed config: %v", err)
	}
}

func TestEmployeeRuntimeProfileDisabledSeatRetainsOverride(t *testing.T) {
	_, cfg, rule, _, _, _ := profileRepairFixture(t, "idle")
	p := cfg.RuntimeProfiles[rule.Kind]
	p.Employees = map[string]EmployeeRuntimeProfile{rule.Name: {Model: "gpt-6", ReasoningEffort: "medium"}}
	cfg.RuntimeProfiles[rule.Kind] = p
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == rule.Name {
			cfg.Agents[i].Disabled = true
		}
	}
	if err := validateRuntimeProfiles(cfg); err != nil {
		t.Fatal(err)
	}
}
