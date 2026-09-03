package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeDoctorRunner struct {
	calls int
	err   error
}

type fakeDoctorPatrol struct {
	report PatrolReport
	err    error
}

func (f fakeDoctorPatrol) Run(context.Context, Config, string, time.Duration) (PatrolReport, error) {
	return f.report, f.err
}

type fakeDoctorGateway struct{ health GatewayHealth }

func (f fakeDoctorGateway) Ping(context.Context, string, string) GatewayHealth { return f.health }

type fakeDoctorLedger struct {
	summary LedgerHealthSummary
	err     error
}

func (f fakeDoctorLedger) Read(Config) (LedgerHealthSummary, error) { return f.summary, f.err }

func (r *fakeDoctorRunner) WorkspaceList(string, string) error {
	r.calls++
	return r.err
}

func prepareDoctorEnv(t *testing.T) testEnv {
	t.Helper()
	root := canonicalTestTempDir(t)
	office := filepath.Join(root, "ceo-office")
	e := testEnv{
		root:        root,
		office:      office,
		data:        filepath.Join(office, "records"),
		config:      filepath.Join(office, "hq-config.yaml"),
		snapshot:    filepath.Join(root, "snapshot.json"),
		herdr:       filepath.Join(root, "fake-herdr"),
		herdrOutput: filepath.Join(root, "herdr-calls.log"),
	}
	for _, directory := range []string{
		office,
		filepath.Join(office, "decisions"),
		filepath.Join(root, "engineering"),
		filepath.Join(root, "product"),
		filepath.Join(root, "qa-ux"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig()
	cfg.Agents = append(cfg.Agents,
		AgentRule{Name: "zhangqian", Label: "test-product-manager", Nickname: "张骞", DepartmentLabel: "产品部", Workspace: cfg.WorkspaceLabel, Responsibilities: []string{"manager:product"}, ManualPath: "product/AGENTS.md", Department: "product", ReportsTo: "penny", Kind: "codex", CanCreate: true, CanAccept: true, CanReceiveOrder: true},
		AgentRule{Name: "researcher", Label: "test-researcher", Nickname: "研究员", DepartmentLabel: "产品部", Workspace: cfg.WorkspaceLabel, Responsibilities: []string{"researcher:product"}, ManualPath: "product/AGENTS.md", Department: "product", ReportsTo: "zhangqian", Kind: "codex", CanCreate: true, CanAccept: true, CanReceiveOrder: true},
	)
	cfg = bindTestRoleContracts(cfg)
	for _, rule := range cfg.Agents {
		writeTestFile(t, filepath.Join(e.root, rule.ManualPath), string(testRoleManual(rule.Name)))
	}
	writeConfigFixture(t, e.config, cfg)
	writeApprovalDocument(t, filepath.Join(e.office, "decisions", "approved.md"), "DEC-DOCTOR-001", "effective", []ApprovalScope{{
		Action: "issue", CaseID: "DOCTOR-001", SourceRef: "/synthetic/source.md", Target: "penny",
	}})
	binary := writeTestFile(t, filepath.Join(e.office, "tools", "hq", "bin", "hq"), "test binary\n")
	if err := os.Chmod(binary, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, e.herdr, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HQ_HERDR_CAPTURE\"\n")
	if err := os.Chmod(e.herdr, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HQ_HERDR_CAPTURE", e.herdrOutput)
	example, err := os.ReadFile(repositoryPath("examples", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(e.office, "tools", "hq", "config.example.yaml"), string(example))
	return e
}

func newTestDoctor(t *testing.T, e testEnv, runner DoctorRunner, jsonOutput bool) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errOut bytes.Buffer
	app, err := newDoctorApp(globalOptions{
		Office: e.office,
		Data:   e.data,
		Config: e.config,
		Herdr:  e.herdr,
		JSON:   jsonOutput,
	}, &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	app.DoctorRunner = runner
	app.PatrolRunner = fakeDoctorPatrol{report: PatrolReport{Version: patrolReportVersion, WorkspaceID: "w-test", WorkspaceLabel: "hq-test"}}
	app.GatewayHealth = fakeDoctorGateway{health: GatewayHealth{OK: true, Connected: true, Version: gatewayProtocolVersion, Workspace: "w-test", ServerID: "gateway-test"}}
	app.LedgerHealth = fakeDoctorLedger{summary: LedgerHealthSummary{StatusCounts: map[string]int{}}}
	return app, &out, &errOut
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := fmt.Sprintf("%s:%04o", info.Mode().Type(), info.Mode().Perm())
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			hash := sha256.Sum256(raw)
			value += ":" + hex.EncodeToString(hash[:])
		}
		snapshot[rel] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func decodeDoctorReport(t *testing.T, raw []byte) DoctorReport {
	t.Helper()
	var report DoctorReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("invalid doctor JSON: %v\n%s", err, raw)
	}
	return report
}

func checksByName(report DoctorReport) map[string]DoctorCheck {
	checks := make(map[string]DoctorCheck, len(report.Checks))
	for _, check := range report.Checks {
		checks[check.Name] = check
	}
	return checks
}

func TestDoctorPassesAndDoesNotMutateInspectedTree(t *testing.T) {
	e := prepareDoctorEnv(t)
	runner := &fakeDoctorRunner{}
	app, out, errOut := newTestDoctor(t, e, runner, true)
	before := snapshotTree(t, e.root)

	if err := app.cmdDoctor(nil); err != nil {
		t.Fatalf("doctor failed: %v\nstderr=%s\nstdout=%s", err, errOut.String(), out.String())
	}
	after := snapshotTree(t, e.root)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("doctor mutated fixture\nbefore=%v\nafter=%v", before, after)
	}
	if _, err := os.Stat(e.data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing records must remain missing, stat err=%v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("workspace list calls=%d, want 1", runner.calls)
	}

	report := decodeDoctorReport(t, out.Bytes())
	if report.Version != doctorReportVersion || !report.OK {
		t.Fatalf("unexpected report envelope: %+v", report)
	}
	for _, check := range report.Checks {
		if check.Status != doctorStatusPass {
			t.Fatalf("check did not pass: %+v", check)
		}
	}
	records := checksByName(report)["records"]
	if !strings.Contains(records.Message, "不存在") || !strings.Contains(records.Remediation, "首次写入") {
		t.Fatalf("missing records guidance absent: %+v", records)
	}
}

func TestDoctorReportsHardFailuresAndBlocksConfigDependents(t *testing.T) {
	e := prepareDoctorEnv(t)
	if err := os.WriteFile(e.config, []byte(`{"version":999,"workspace_label":"secret=leak","agents":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.office, "decisions", "approved.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.office, "tools", "hq", "bin", "hq")); err != nil {
		t.Fatal(err)
	}
	runner := &fakeDoctorRunner{err: errors.New("fake unavailable")}
	app, out, _ := newTestDoctor(t, e, runner, true)

	err := app.cmdDoctor(nil)
	var failed DoctorFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("hard failures must return DoctorFailedError, got %v", err)
	}
	report := decodeDoctorReport(t, out.Bytes())
	if report.OK {
		t.Fatal("failed report must set ok=false")
	}
	checks := checksByName(report)
	if checks["office_directory"].Status != doctorStatusPass {
		t.Fatalf("existing office directory must pass independently of config: %+v", checks["office_directory"])
	}
	for _, name := range []string{"config", "decisions", "binary"} {
		if checks[name].Status != doctorStatusFail {
			t.Fatalf("%s status=%q, want FAIL; check=%+v", name, checks[name].Status, checks[name])
		}
	}
	for _, name := range []string{"agent_workstations", "herdr_workspace"} {
		if checks[name].Status != doctorStatusBlocked {
			t.Fatalf("config-dependent %s check must be BLOCKED: %+v", name, checks[name])
		}
	}
	if strings.Contains(out.String(), "secret=leak") {
		t.Fatalf("doctor echoed sensitive config content: %s", out.String())
	}
}

func TestDoctorChecksEveryActiveAgentWorkstation(t *testing.T) {
	e := prepareDoctorEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"eng-developer", "eng-data-engineer", "zantianyou"} {
		rule, _ := cfg.exactRule(name)
		if err := os.Remove(filepath.Join(e.root, rule.ManualPath)); err != nil {
			t.Fatal(err)
		}
	}
	app, out, _ := newTestDoctor(t, e, &fakeDoctorRunner{}, true)
	if err := app.cmdDoctor(nil); err == nil {
		t.Fatal("missing personal AGENTS.md must fail")
	}
	report := decodeDoctorReport(t, out.Bytes())
	checks := checksByName(report)
	for _, name := range []string{"eng-developer", "eng-data-engineer", "zantianyou"} {
		check, ok := checks["agent_workstation/"+name]
		if !ok || check.Status != doctorStatusFail || !strings.Contains(check.Remediation, "AGENTS.md") {
			t.Fatalf("missing workstation failure for %s: %+v", name, check)
		}
	}
	if checks["agent_workstation/penny"].Status != doctorStatusPass {
		t.Fatalf("unaffected active agent should pass: %+v", checks["agent_workstation/penny"])
	}
}

func TestDoctorJSONFlagIsCobraRecognizedAndFailureIsStructured(t *testing.T) {
	e := prepareDoctorEnv(t)
	if err := os.Remove(filepath.Join(e.office, "tools", "hq", "bin", "hq")); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	args := []string{
		"--office", e.office,
		"--data", e.data,
		"--config", e.config,
		"--herdr", e.herdr,
		"doctor", "--json",
	}
	err := execute(args, &out, &errOut)
	var failed DoctorFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("doctor should return structured failure exit, got %v", err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("JSON mode must not mix human prefixes into stderr: %s", errOut.String())
	}
	report := decodeDoctorReport(t, out.Bytes())
	if report.OK || checksByName(report)["binary"].Status != doctorStatusFail {
		t.Fatalf("unexpected failure report: %+v", report)
	}
	calls, err := os.ReadFile(e.herdrOutput)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(calls)) != "api snapshot\napi snapshot" {
		t.Fatalf("doctor invoked unexpected herdr operation: %s", calls)
	}
}

func TestDoctorRunnerMissingFailsClosed(t *testing.T) {
	e := prepareDoctorEnv(t)
	app, out, _ := newTestDoctor(t, e, nil, true)
	if err := app.cmdDoctor(nil); err == nil {
		t.Fatal("missing runner must fail closed")
	}
	check := checksByName(decodeDoctorReport(t, out.Bytes()))["herdr_workspace"]
	if check.Status != doctorStatusFail {
		t.Fatalf("missing runner did not fail herdr check: %+v", check)
	}
}

func TestDoctorRecordsPermissionCheckIsReadOnly(t *testing.T) {
	e := prepareDoctorEnv(t)
	if err := os.MkdirAll(e.data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.data, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.data, 0o755) })
	app, out, _ := newTestDoctor(t, e, &fakeDoctorRunner{}, true)
	before := snapshotTree(t, e.root)
	if err := app.cmdDoctor(nil); err == nil {
		t.Fatal("read-only records directory must be a hard failure")
	}
	after := snapshotTree(t, e.root)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("records check changed fixture\nbefore=%v\nafter=%v", before, after)
	}
	check := checksByName(decodeDoctorReport(t, out.Bytes()))["records"]
	if check.Status != doctorStatusFail || !strings.Contains(check.Remediation, "不会 chmod") {
		t.Fatalf("unexpected records permission result: %+v", check)
	}
}

func TestDoctorReportsMissingConfigDecisionDirectoryAndWorkstation(t *testing.T) {
	tests := []struct {
		name     string
		remove   func(testEnv) string
		expected map[string]string
	}{
		{
			name:   "config",
			remove: func(e testEnv) string { return e.config },
			expected: map[string]string{
				"config":             doctorStatusFail,
				"agent_workstations": doctorStatusBlocked,
			},
		},
		{
			name:   "decisions directory",
			remove: func(e testEnv) string { return filepath.Join(e.office, "decisions") },
			expected: map[string]string{
				"decisions": doctorStatusFail,
			},
		},
		{
			name:   "department workstation",
			remove: func(e testEnv) string { return filepath.Join(e.root, "product") },
			expected: map[string]string{
				"agent_workstation/researcher": doctorStatusFail,
				"agent_workstation/zhangqian":  doctorStatusFail,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := prepareDoctorEnv(t)
			if err := os.RemoveAll(test.remove(e)); err != nil {
				t.Fatal(err)
			}
			app, out, _ := newTestDoctor(t, e, &fakeDoctorRunner{}, true)
			if err := app.cmdDoctor(nil); err == nil {
				t.Fatal("missing required path must fail")
			}
			checks := checksByName(decodeDoctorReport(t, out.Bytes()))
			for name, status := range test.expected {
				if checks[name].Status != status {
					t.Fatalf("%s status=%q, want %q; check=%+v", name, checks[name].Status, status, checks[name])
				}
			}
		})
	}
}

func TestDoctorConfigFailuresProvideSafeUserActions(t *testing.T) {
	tests := []struct {
		name            string
		breakConfig     func(*testing.T, testEnv)
		message         string
		actionFragments []string
	}{
		{
			name: "missing",
			breakConfig: func(t *testing.T, e testEnv) {
				t.Helper()
				if err := os.Remove(e.config); err != nil {
					t.Fatal(err)
				}
			},
			message:         "HQ config 缺失",
			actionFragments: []string{"已批准", "hq init", "config.example.yaml", "不能直接"},
		},
		{
			name: "YAML parse failure",
			breakConfig: func(t *testing.T, e testEnv) {
				t.Helper()
				if err := os.WriteFile(e.config, []byte("{\n  \"version\": 1,\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			message:         "HQ config YAML 解析失败",
			actionFragments: []string{"YAML", "不会覆盖"},
		},
		{
			name: "semantic validation failure",
			breakConfig: func(t *testing.T, e testEnv) {
				t.Helper()
				if err := os.WriteFile(e.config, []byte("version: 999\nworkspace_label: hq\nowner_principal: ZC\nagents: []\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			message:         "HQ config 已解析，但未通过语义校验",
			actionFragments: []string{"cp -np", ".bak", "config.example.yaml", "README"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := prepareDoctorEnv(t)
			test.breakConfig(t, e)
			before := snapshotTree(t, e.root)
			app, out, _ := newTestDoctor(t, e, &fakeDoctorRunner{}, true)
			if err := app.cmdDoctor(nil); err == nil {
				t.Fatal("broken config must fail")
			}
			after := snapshotTree(t, e.root)
			if fmt.Sprint(before) != fmt.Sprint(after) {
				t.Fatalf("doctor changed broken config fixture\nbefore=%v\nafter=%v", before, after)
			}
			check := checksByName(decodeDoctorReport(t, out.Bytes()))["config"]
			if check.Message != test.message {
				t.Fatalf("message=%q, want %q; check=%+v", check.Message, test.message, check)
			}
			for _, fragment := range test.actionFragments {
				if !strings.Contains(check.Remediation, fragment) {
					t.Fatalf("remediation missing %q: %+v", fragment, check)
				}
			}
			if strings.Contains(check.Remediation, "loadConfig") || strings.Contains(check.Remediation, "validateConfig") {
				t.Fatalf("remediation exposed internal function names: %+v", check)
			}
		})
	}
}

func TestDoctorConfigExampleIsSchemaReferenceNotInstanceRecovery(t *testing.T) {
	e := prepareDoctorEnv(t)
	if err := os.Remove(e.config); err != nil {
		t.Fatal(err)
	}
	beforeMissing := snapshotTree(t, e.root)
	app, out, _ := newTestDoctor(t, e, &fakeDoctorRunner{}, true)
	if err := app.cmdDoctor(nil); err == nil {
		t.Fatal("missing config must fail before recovery")
	}
	afterMissing := snapshotTree(t, e.root)
	if fmt.Sprint(beforeMissing) != fmt.Sprint(afterMissing) {
		t.Fatalf("doctor changed missing-config fixture\nbefore=%v\nafter=%v", beforeMissing, afterMissing)
	}
	missingCheck := checksByName(decodeDoctorReport(t, out.Bytes()))["config"]
	if !strings.Contains(missingCheck.Remediation, "已批准") || !strings.Contains(missingCheck.Remediation, "不能直接") {
		t.Fatalf("missing config did not explain instance-bound digest recovery: %+v", missingCheck)
	}

	examplePath := filepath.Join(e.office, "tools", "hq", "config.example.yaml")
	example, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.config, example, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatalf("config.example.yaml failed the production loader: %v", err)
	}
	app, out, _ = newTestDoctor(t, e, &fakeDoctorRunner{}, true)
	beforeRecovered := snapshotTree(t, e.root)
	if err := app.cmdDoctor(nil); err == nil {
		t.Fatal("foreign example digests/artifacts unexpectedly recovered this company")
	}
	afterRecovered := snapshotTree(t, e.root)
	if fmt.Sprint(beforeRecovered) != fmt.Sprint(afterRecovered) {
		t.Fatalf("doctor changed recovered fixture\nbefore=%v\nafter=%v", beforeRecovered, afterRecovered)
	}
	checks := checksByName(decodeDoctorReport(t, out.Bytes()))
	if checks["config"].Status != doctorStatusPass {
		t.Fatalf("schema-valid example did not pass config parsing: %+v", checks["config"])
	}
	failedArtifact := false
	for _, rule := range cfg.Agents {
		if check := checks["agent_workstation/"+rule.Name]; check.Status == doctorStatusFail {
			failedArtifact = true
		}
	}
	if !failedArtifact {
		t.Fatalf("foreign example did not fail its instance-bound workstation/manual checks: %+v", checks)
	}
}

func TestDoctorTextOutputIncludesStatusesAndRemediation(t *testing.T) {
	e := prepareDoctorEnv(t)
	if err := os.Remove(filepath.Join(e.office, "tools", "hq", "bin", "hq")); err != nil {
		t.Fatal(err)
	}
	app, out, _ := newTestDoctor(t, e, &fakeDoctorRunner{}, false)
	if err := app.cmdDoctor(nil); err == nil {
		t.Fatal("missing binary must return a non-zero contract")
	}
	text := out.String()
	for _, expected := range []string{"PASS", "FAIL", "binary", "修复：", "HQ doctor：ok=false"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("text output missing %q:\n%s", expected, text)
		}
	}
}
