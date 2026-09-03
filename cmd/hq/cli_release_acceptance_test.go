package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func publicCLIPaths(t *testing.T) [][]string {
	t.Helper()
	root := newCobraRootCommand(globalOptions{}, io.Discard, io.Discard)
	paths := [][]string{{}}
	var walk func(*cobra.Command, []string)
	walk = func(parent *cobra.Command, prefix []string) {
		for _, command := range parent.Commands() {
			if !command.IsAvailableCommand() || command.Hidden || command.Name() == "help" || command.Name() == "completion" {
				continue
			}
			path := append(append([]string(nil), prefix...), command.Name())
			paths = append(paths, path)
			walk(command, path)
		}
	}
	walk(root, nil)
	return paths
}

func TestCLIReleaseCobraHelpMatrixNoPreflight(t *testing.T) {
	t.Setenv("HQ_OFFICE", "")
	t.Setenv("HQ_HERDR_BIN", filepath.Join(canonicalTestTempDir(t), "must-not-exist"))
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	arbitrary := canonicalTestTempDir(t)
	if err := os.Chdir(arbitrary); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })

	paths := publicCLIPaths(t)
	for _, helpFlag := range []string{"-h", "--help"} {
		for _, path := range paths {
			args := append(append([]string(nil), path...), helpFlag)
			var out, errOut bytes.Buffer
			if err := execute(args, &out, &errOut); err != nil {
				t.Fatalf("help failed path=%q flag=%s err=%v stderr=%s", strings.Join(path, " "), helpFlag, err, errOut.String())
			}
			if out.Len() == 0 {
				t.Fatalf("empty help path=%q flag=%s", strings.Join(path, " "), helpFlag)
			}
			if errOut.Len() != 0 {
				t.Fatalf("help wrote stderr path=%q flag=%s stderr=%s", strings.Join(path, " "), helpFlag, errOut.String())
			}
		}
	}
	if entries, err := os.ReadDir(arbitrary); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("help matrix wrote files in arbitrary cwd: %v", entries)
	}
}

func TestNudgeHelpMatchesTwoKiBMessageContract(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := execute([]string{"nudge", "enqueue", "--help"}, &out, &errOut); err != nil {
		t.Fatalf("nudge enqueue help failed: %v stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "2 KiB") || strings.Contains(out.String(), "200 rune") {
		t.Fatalf("nudge help does not match the 2 KiB runtime contract:\n%s", out.String())
	}
}

func TestCLIAllPublicCommandsAndFlagsAreSelfTeaching(t *testing.T) {
	t.Setenv("HQ_OFFICE", "")
	t.Setenv("HQ_HERDR_BIN", filepath.Join(canonicalTestTempDir(t), "must-not-exist"))
	paths := publicCLIPaths(t)
	if len(paths) < 60 {
		t.Fatalf("public CLI inventory unexpectedly small: %d", len(paths))
	}
	for _, path := range paths {
		label := strings.Join(path, " ")
		if label == "" {
			label = "root"
		}
		t.Run(strings.ReplaceAll(label, " ", "_"), func(t *testing.T) {
			var helpOut, helpErr bytes.Buffer
			helpArgs := append(append([]string(nil), path...), "--help")
			if err := execute(helpArgs, &helpOut, &helpErr); err != nil {
				t.Fatalf("help failed: %v stderr=%s", err, helpErr.String())
			}
			for _, want := range []string{"Usage:", "hq"} {
				if !strings.Contains(helpOut.String(), want) {
					t.Fatalf("help missing %q:\n%s", want, helpOut.String())
				}
			}

			// Root and command groups reject unknown tokens as subcommands; leaf
			// commands reject the same probe as an unknown flag. Both paths must
			// teach the agent how to recover without an external document.
			probe := append(append([]string(nil), path...), "--definitely-not-an-hq-flag")
			var out, errOut bytes.Buffer
			err := execute(probe, &out, &errOut)
			if err == nil {
				t.Fatalf("invalid probe passed: %v", probe)
			}
			if exitCodeForError(err) != exitUsage {
				t.Fatalf("invalid probe exit=%d err=%v", exitCodeForError(err), err)
			}
			for _, want := range []string{"命令 `hq", "用法：", "下一步：", "--help", "hq delivery status --id DELIVERY_ID", "hq flow show --case CASE_ID", "不要改用裸 herdr prompt"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("agent-facing error missing %q: %v", want, err)
				}
			}
		})
	}

	// Every required-flag set is rejected before dependency discovery and gives
	// the same local repair loop. This also guards future commands: adding a
	// requiredFlags entry automatically enrolls it in the audit.
	for _, path := range paths {
		if len(path) == 0 || len(requiredFlags(path...)) == 0 {
			continue
		}
		var out, errOut bytes.Buffer
		err := execute(path, &out, &errOut)
		if err == nil || exitCodeForError(err) != exitUsage {
			t.Fatalf("missing required flags path=%v err=%v code=%d", path, err, exitCodeForError(err))
		}
		for _, want := range []string{"缺少必填参数", "用法：", "下一步：", strings.Join(path, " ") + " --help"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("required-flag error path=%v missing %q: %v", path, want, err)
			}
		}
	}
}

func TestAgentFacingGuidancePreservesEveryErrorCategory(t *testing.T) {
	root := newCobraRootCommand(globalOptions{}, io.Discard, io.Discard)
	command, _, err := root.Find([]string{"delivery", "retry"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		cause error
		code  int
	}{
		{usagef("bad parameter"), exitUsage},
		{permissionf("not authorized"), exitPermission},
		{conflictf("state conflict"), exitConflict},
		{unavailablef("runtime offline"), exitUnavailable},
		{internalf("ledger broken"), exitInternal},
		{errors.New("uncategorized failure"), exitInternal},
	} {
		guided := agentFacingCommandError(command, test.cause)
		if got := exitCodeForError(guided); got != test.code {
			t.Fatalf("guided category=%d want=%d err=%v", got, test.code, guided)
		}
		for _, want := range []string{"hq delivery retry", "用法：", "下一步：", "hq delivery retry --help"} {
			if !strings.Contains(guided.Error(), want) {
				t.Fatalf("guided error missing %q: %v", want, guided)
			}
		}
	}
}

func TestCLIAllPublicParametersDescribeTheirContract(t *testing.T) {
	root := newCobraRootCommand(globalOptions{}, io.Discard, io.Discard)
	var walk func(*cobra.Command)
	walk = func(command *cobra.Command) {
		command.LocalNonPersistentFlags().VisitAll(func(flag *pflag.Flag) {
			if !flag.Hidden && strings.TrimSpace(flag.Usage) == "" {
				t.Errorf("%s --%s has no agent-readable description", command.CommandPath(), flag.Name)
			}
		})
		path := strings.Fields(strings.TrimPrefix(command.CommandPath(), "hq "))
		if command == root {
			path = nil
		}
		for _, name := range requiredFlags(path...) {
			flag := command.Flags().Lookup(name)
			if flag == nil {
				t.Errorf("%s required flag --%s is not registered", command.CommandPath(), name)
				continue
			}
			if !strings.Contains(flag.Usage, "必填") {
				t.Errorf("%s --%s does not disclose that it is required: %q", command.CommandPath(), name, flag.Usage)
			}
		}
		for _, child := range command.Commands() {
			if child.IsAvailableCommand() && !child.Hidden && child.Name() != "help" && child.Name() != "completion" {
				walk(child)
			}
		}
	}
	walk(root)
}

func TestCLIPureParameterContractsFailBeforeOfficeDiscovery(t *testing.T) {
	t.Setenv("HQ_OFFICE", "")
	t.Setenv("HQ_HERDR_BIN", filepath.Join(canonicalTestTempDir(t), "must-not-exist"))
	caseSpec := []string{"--id", "CASE-PARAMETER-AUDIT", "--title", "audit", "--objective", "audit", "--acceptance", "audit", "--constraints", "audit", "--priority", "P1", "--source", "/tmp/audit.md"}
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "root project", args: append([]string{"case", "create"}, caseSpec...), want: []string{"唯一 root", "--project"}},
		{name: "child project", args: append(append([]string{"case", "create"}, caseSpec...), "--parent", "CASE-PARENT", "--project", "wrong"), want: []string{"child case", "删除 --project"}},
		{name: "revise lineage", args: append(append([]string{"case", "revise"}, caseSpec...), "--version", "2", "--project", "wrong"), want: []string{"case revise 不接受 --project"}},
		{name: "report blocked evidence", args: []string{"report", "--case", "CASE-PARAMETER-AUDIT", "--result", "blocked", "--next", "retry"}, want: []string{"--source", "--note", "可执行模板"}},
		{name: "message enum", args: []string{"message", "--to", "worker", "--kind", "urgent", "--text", "work"}, want: []string{"--kind 只能是 info|question|request|handoff"}},
		{name: "issue delivery", args: []string{"issue", "--case", "CASE-PARAMETER-AUDIT", "--to", "worker", "--next", "work", "--delivery", "quiet"}, want: []string{"固定使用 --delivery wakeup"}},
		{name: "project filter", args: []string{"project", "list", "--status", "done"}, want: []string{"--status 只能是 active|review|blocked|closed"}},
		{name: "assignment filter", args: []string{"assignment", "list", "--status", "working"}, want: []string{"--status 只能是 issued|accepted|submitted|rework|completed|reported|returned"}},
		{name: "delivery outcome", args: []string{"delivery", "resolve", "--id", "delivery:123", "--outcome", "maybe", "--reason", "checked", "--evidence", "/tmp/evidence.md"}, want: []string{"--outcome 只能是 delivered|not-delivered"}},
		{name: "nudge outcome", args: []string{"nudge", "reconcile", "--id", "NUDGE-1", "--resolution", "maybe", "--ref", "/tmp/evidence.md", "--note", "checked"}, want: []string{"--resolution 只能是 delivered|not-run"}},
		{name: "index entity", args: []string{"index", "query", "--entity", "unknown"}, want: []string{"--entity 只能是 flow_events|cases|deliveries|documents"}},
		{name: "staff add permission", args: []string{"staff", "add", "--name", "worker", "--label", "Worker", "--department", "engineering", "--role", "worker@1", "--workstation", "engineering/staff/worker/v1", "--approval", "/tmp/approval.md", "--permission-mode", "maybe"}, want: []string{"--permission-mode 只能是 native|yolo"}},
		{name: "staff update conflict", args: []string{"staff", "update", "--name", "worker", "--approval", "/tmp/approval.md", "--enable", "--disable"}, want: []string{"--enable 与 --disable 不能同时使用"}},
		{name: "role capability", args: []string{"role", "add", "--id", "worker", "--version", "1", "--label", "Worker", "--department", "engineering", "--manual", "engineering/staff/worker/v1/AGENTS.md", "--approval", "/tmp/approval.md"}, want: []string{"至少需要一个 --capability"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := execute(test.args, &out, &errOut)
			if err == nil || exitCodeForError(err) != exitUsage {
				t.Fatalf("pure parameter error=%v code=%d stdout=%s stderr=%s", err, exitCodeForError(err), out.String(), errOut.String())
			}
			if strings.Contains(err.Error(), "ceo-office") {
				t.Fatalf("dependency discovery hid parameter failure: %v", err)
			}
			for _, want := range append(test.want, "下一步：") {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("missing %q in %v", want, err)
				}
			}
		})
	}
}

func TestCLIReleaseCobraMissingAndUnknownSubcommandsAreUsage(t *testing.T) {
	t.Setenv("HQ_OFFICE", "")
	t.Setenv("HQ_HERDR_BIN", filepath.Join(canonicalTestTempDir(t), "must-not-exist"))

	type testCase struct {
		name string
		args []string
		want string
	}
	tests := []testCase{
		{name: "root missing", args: nil, want: "缺少子命令"},
		{name: "root unknown", args: []string{"foobar"}, want: "未知命令 \"foobar\""},
		{name: "group missing", args: []string{"case"}, want: "缺少子命令"},
		{name: "group unknown case", args: []string{"case", "foobar"}, want: "未知子命令 \"foobar\""},
		{name: "group unknown nudge", args: []string{"nudge", "nope"}, want: "未知子命令 \"nope\""},
	}
	for _, group := range []string{"nudge", "reminder", "estop", "session", "flow", "index", "case", "project", "approval", "delivery", "delivery budget", "staff"} {
		path := strings.Fields(group)
		tests = append(tests,
			testCase{name: group + " missing", args: append([]string(nil), path...), want: "缺少子命令"},
			testCase{name: group + " unknown", args: append(append([]string(nil), path...), "not-a-command"), want: "未知子命令 \"not-a-command\""},
		)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := execute(test.args, &out, &errOut)
			if err == nil {
				t.Fatalf("invalid invocation passed: %v", test.args)
			}
			if got := exitCodeForError(err); got != exitUsage {
				t.Fatalf("usage exit mismatch args=%v code=%d err=%v", test.args, got, err)
			}
			for _, want := range []string{test.want, "用法："} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error missing %q args=%v err=%v", want, test.args, err)
				}
			}
		})
	}
}

func TestCLIReleaseUsageCategoriesAndZeroWrite(t *testing.T) {
	root := canonicalTestTempDir(t)
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	cases := [][]string{{"staff", "get"}, {"staff", "get", "--unknown"}, {"case", "show"}, {"project", "show"}, {"init"}, {"version", "extra"}}
	for _, args := range cases {
		var out, errOut bytes.Buffer
		err := execute(args, &out, &errOut)
		if err == nil {
			t.Fatalf("invalid invocation passed: %v", args)
		}
		if exitCodeForError(err) != exitUsage {
			t.Fatalf("usage exit mismatch args=%v code=%d err=%v", args, exitCodeForError(err), err)
		}
		if !strings.Contains(err.Error(), "用法：") {
			t.Fatalf("usage missing args=%v err=%v", args, err)
		}
	}
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("usage path wrote files: before=%d after=%d", len(before), len(after))
	}
	for _, tc := range []struct {
		err  error
		code int
	}{
		{permissionf("denied"), exitPermission}, {conflictf("conflict"), exitConflict}, {unavailablef("offline"), exitUnavailable}, {internalf("broken"), exitInternal}, {errors.New("uncategorized"), exitInternal},
	} {
		if got := exitCodeForError(tc.err); got != tc.code {
			t.Fatalf("category code=%d want=%d err=%v", got, tc.code, tc.err)
		}
	}
}

func TestCLIReleaseReadmeFirstUseContinuityWithoutHerdr(t *testing.T) {
	root := canonicalTestTempDir(t)
	companyRoot := filepath.Join(root, "headquarters")
	office := filepath.Join(companyRoot, "ceo-office")
	data := filepath.Join(office, "records")
	emptyPath := filepath.Join(root, "empty-path")
	if err := os.Mkdir(emptyPath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", emptyPath)
	t.Setenv("HQ_HERDR_BIN", "")

	var first bytes.Buffer
	initArgs := []string{"init", companyRoot, "--silent", "--company-name", "Continuity Co", "--owner", "ZC", "--workspace", "continuity-hq", "--template", "minimal", "--prepare-only"}
	if err := execute(initArgs, &first, &first); err != nil {
		t.Fatalf("first init: %v\n%s", err, first.String())
	}
	if !strings.Contains(first.String(), "created=") || !strings.Contains(first.String(), "通过静态校验") {
		t.Fatalf("first init count: %s", first.String())
	}
	manifestBefore := initTreeManifest(t, root)
	var second bytes.Buffer
	if err := execute(initArgs, &second, &second); err != nil {
		t.Fatalf("second init: %v", err)
	}
	if !strings.Contains(second.String(), "created=0") {
		t.Fatalf("second init count: %s", second.String())
	}
	if got := initTreeManifest(t, root); !reflect.DeepEqual(got, manifestBefore) {
		t.Fatalf("second init changed manifest\nbefore=%v\nafter=%v", manifestBefore, got)
	}

	var staffOut bytes.Buffer
	if err := execute([]string{"--office", office, "staff", "list"}, &staffOut, &staffOut); err != nil {
		t.Fatalf("staff list: %v\n%s", err, staffOut.String())
	}
	if !strings.Contains(staffOut.String(), "continuity-secretary") || !strings.Contains(staffOut.String(), "continuity-delivery-manager") {
		t.Fatalf("staff output: %s", staffOut.String())
	}
	var boardOut bytes.Buffer
	if err := execute([]string{"--office", office, "board", "--cases-only"}, &boardOut, &boardOut); err != nil {
		t.Fatalf("board: %v\n%s", err, boardOut.String())
	}
	if !strings.Contains(boardOut.String(), "0 cases / 0 events") {
		t.Fatalf("board output: %s", boardOut.String())
	}
	var projectOut bytes.Buffer
	if err := execute([]string{"--office", office, "--json", "project", "list"}, &projectOut, &projectOut); err != nil {
		t.Fatalf("project list: %v\n%s", err, projectOut.String())
	}
	var projects ProjectListView
	if err := json.Unmarshal(projectOut.Bytes(), &projects); err != nil || projects.ProjectCount != 0 || projects.Projects == nil {
		t.Fatalf("empty project output: %+v err=%v raw=%s", projects, err, projectOut.String())
	}
	if got := initTreeManifest(t, root); !reflect.DeepEqual(got, manifestBefore) {
		t.Fatalf("read-only first use changed manifest\nbefore=%v\nafter=%v", manifestBefore, got)
	}
	for _, path := range []string{
		filepath.Join(data, "events"), filepath.Join(data, ".hq.lock"),
		filepath.Join(data, "state.json"), filepath.Join(data, "hq.sock"),
		filepath.Join(office, "tools", "index.db"),
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("read-only first use created %s: %v", path, err)
		}
	}

	var writeOut bytes.Buffer
	err := execute([]string{"--office", office, "case", "create", "--id", "CASE-READONLY-REGRESSION", "--title", "must fail closed", "--project", "test-project", "--objective", "verify gateway", "--acceptance", "write rejected", "--constraints", "read-only fixture", "--priority", "P1", "--source", filepath.Join(root, "source.md")}, &writeOut, &writeOut)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "herdr") {
		t.Fatalf("Herdr-dependent write did not fail closed: err=%v output=%s", err, writeOut.String())
	}
}

type countingDoctorGateway struct {
	calls  int
	health GatewayHealth
}

func (g *countingDoctorGateway) Ping(context.Context, string, string) GatewayHealth {
	g.calls++
	return g.health
}

func TestCLIReleaseDoctorColdStartTreatsMissingGatewayAsExpected(t *testing.T) {
	e := prepareDoctorEnv(t)
	app, out, _ := newTestDoctor(t, e, &fakeDoctorRunner{}, false)
	gateway := &countingDoctorGateway{health: GatewayHealth{Connected: true, Error: "must not be observed"}}
	app.GatewayHealth = gateway
	before := snapshotTree(t, e.root)

	if err := app.cmdDoctor(nil); err != nil {
		t.Fatalf("cold-start doctor failed: %v\n%s", err, out.String())
	}
	if gateway.calls != 0 {
		t.Fatalf("cold-start doctor pinged absent gateway %d times", gateway.calls)
	}
	after := snapshotTree(t, e.root)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("cold-start doctor mutated fixture\nbefore=%v\nafter=%v", before, after)
	}
	if _, err := os.Lstat(e.data); !os.IsNotExist(err) {
		t.Fatalf("cold-start doctor created records: %v", err)
	}
	text := out.String()
	for _, want := range []string{"PASS", "company_health", "gateway=not-started", "records", "提示：", "ok=true"} {
		if !strings.Contains(text, want) {
			t.Fatalf("cold-start text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "修复：无需预创建") {
		t.Fatalf("cold-start records advisory used a hard-failure label:\n%s", text)
	}
}

func TestCLIReleaseDoctorColdStartCLIUsesFakeHerdrAndKeepsRecordsAbsent(t *testing.T) {
	e := newInitTestEnv(t)
	runInitForTest(t, e, false)
	if err := os.RemoveAll(e.data); err != nil {
		t.Fatal(err)
	}
	writeApprovalDocument(t, filepath.Join(e.office, "decisions", "approved.md"), "DEC-CLI-COLD-001", "effective", []ApprovalScope{{
		Action: "issue", CaseID: "CLI-COLD", SourceRef: "/synthetic/source.md", Target: "test-company-delivery-manager",
	}})
	binary := filepath.Join(e.office, "tools", "hq", "bin", "hq")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("isolated build placeholder\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeRoot := canonicalTestTempDir(t)
	callLog := filepath.Join(fakeRoot, "calls.log")
	fakeHerdr := filepath.Join(fakeRoot, "fake-herdr")
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	officeWorkstation := testAgentCWD(cfg, e.root, "test-company-secretary")
	managerWorkstation := testAgentCWD(cfg, e.root, "test-company-delivery-manager")
	specialistWorkstation := testAgentCWD(cfg, e.root, "test-company-delivery-specialist")
	snapshot := fmt.Sprintf(`{"result":{"snapshot":{"workspaces":[{"workspace_id":"w-test","label":"test-company-hq"}],"tabs":[{"tab_id":"w-test:t1","workspace_id":"w-test","label":"总裁办-总裁秘书","cwd":%q},{"tab_id":"w-test:t2","workspace_id":"w-test","label":"交付部-交付负责人","cwd":%q},{"tab_id":"w-test:t3","workspace_id":"w-test","label":"交付部-交付专员","cwd":%q}],"panes":[{"pane_id":"w-test:p1","workspace_id":"w-test","tab_id":"w-test:t1","cwd":%q},{"pane_id":"w-test:p2","workspace_id":"w-test","tab_id":"w-test:t2","cwd":%q},{"pane_id":"w-test:p3","workspace_id":"w-test","tab_id":"w-test:t3","cwd":%q}],"agents":[{"name":"test-company-secretary","agent":"codex","agent_status":"idle","workspace_id":"w-test","tab_id":"w-test:t1","pane_id":"w-test:p1","cwd":%q,"interactive_ready":true},{"name":"test-company-delivery-manager","agent":"codex","agent_status":"idle","workspace_id":"w-test","tab_id":"w-test:t2","pane_id":"w-test:p2","cwd":%q,"interactive_ready":true},{"name":"test-company-delivery-specialist","agent":"codex","agent_status":"idle","workspace_id":"w-test","tab_id":"w-test:t3","pane_id":"w-test:p3","cwd":%q,"interactive_ready":true}]}}}`,
		officeWorkstation, managerWorkstation, specialistWorkstation, officeWorkstation, managerWorkstation, specialistWorkstation, officeWorkstation, managerWorkstation, specialistWorkstation)
	script := "#!/bin/sh\nif [ \"$1 $2\" != \"api snapshot\" ]; then exit 91; fi\nprintf '%s\\n' \"$1 $2\" >> \"$HQ_CLI_HERDR_LOG\"\nprintf '%s\\n' " + shellQuote(snapshot) + "\n"
	if err := os.WriteFile(fakeHerdr, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HQ_CLI_HERDR_LOG", callLog)
	before := initTreeManifest(t, e.root)

	var out, errOut bytes.Buffer
	if err := execute([]string{
		"--office", e.office, "--config", e.config, "--data", e.data,
		"--herdr", fakeHerdr, "doctor", "--json",
	}, &out, &errOut); err != nil {
		t.Fatalf("cold-start CLI doctor failed: %v\nstderr=%s\nstdout=%s", err, errOut.String(), out.String())
	}
	after := initTreeManifest(t, e.root)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("cold-start CLI doctor mutated fixture\nbefore=%v\nafter=%v", before, after)
	}
	if _, err := os.Lstat(e.data); !os.IsNotExist(err) {
		t.Fatalf("cold-start CLI doctor created records: %v", err)
	}
	report := decodeDoctorReport(t, out.Bytes())
	if !report.OK || report.CompanyHealth == nil || !report.CompanyHealth.Gateway.NotStarted {
		t.Fatalf("cold-start CLI report=%+v", report)
	}
	checks := checksByName(report)
	if checks["records"].Status != doctorStatusPass || checks["records"].Severity != "advisory" {
		t.Fatalf("cold-start records check=%+v", checks["records"])
	}
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "api snapshot\napi snapshot\n" {
		t.Fatalf("doctor invoked unexpected fake Herdr operations: %q", calls)
	}
}

func TestCLIReleaseDoctorExistingGatewayProtocolFailureRemainsHard(t *testing.T) {
	e := prepareDoctorEnv(t)
	if err := os.MkdirAll(e.data, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(e.data, "hq.sock"), "started-but-invalid\n")
	app, out, _ := newTestDoctor(t, e, &fakeDoctorRunner{}, true)
	gateway := &countingDoctorGateway{health: GatewayHealth{Connected: true, Error: "bad handshake"}}
	app.GatewayHealth = gateway
	before := snapshotTree(t, e.root)

	err := app.cmdDoctor(nil)
	var failed DoctorFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("existing abnormal gateway must fail hard: %v", err)
	}
	if gateway.calls != 1 {
		t.Fatalf("existing gateway ping calls=%d, want 1", gateway.calls)
	}
	after := snapshotTree(t, e.root)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("abnormal-gateway doctor mutated fixture\nbefore=%v\nafter=%v", before, after)
	}
	report := decodeDoctorReport(t, out.Bytes())
	check := checksByName(report)["company_health"]
	if report.OK || check.Status != doctorStatusFail || report.CompanyHealth == nil || report.CompanyHealth.Gateway.NotStarted || len(report.CompanyHealth.Errors) != 1 || !strings.Contains(report.CompanyHealth.Errors[0], "bad handshake") {
		t.Fatalf("existing abnormal gateway did not remain hard: report=%+v check=%+v", report, check)
	}
}

func TestCLIReleaseDoctorLabels(t *testing.T) {
	report := DoctorReport{Checks: []DoctorCheck{
		{Name: "advisory", Status: doctorStatusPass, Severity: "advisory", Message: "ok", Remediation: "no action"},
		{Name: "hard", Status: doctorStatusFail, Severity: "hard", Message: "bad", Remediation: "repair"},
	}}
	var out bytes.Buffer
	app := &App{Out: &out}
	if err := app.writeDoctorReport(report); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "提示：no action") || strings.Contains(text, "修复：no action") {
		t.Fatalf("PASS label regressed:\n%s", text)
	}
	if !strings.Contains(text, "修复：repair") {
		t.Fatalf("FAIL label regressed:\n%s", text)
	}
}

func TestCLIReleaseInitHelpAndVersion(t *testing.T) {
	var help bytes.Buffer
	if err := execute([]string{"init", "--help"}, &help, &help); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"<company-directory>", "--silent", "--company-name", "--owner", "--template", "--organization-spec", "--secretary-name", "--secretary-nickname", "--prepare-only", "LLM API"} {
		if !strings.Contains(help.String(), required) {
			t.Fatalf("init help missing %q:\n%s", required, help.String())
		}
	}

	oldVersion, oldCommit := buildVersion, buildCommit
	t.Cleanup(func() { buildVersion, buildCommit = oldVersion, oldCommit })
	buildVersion, buildCommit = "v9.8.7", "0123456789abcdef"
	var textOut bytes.Buffer
	if err := execute([]string{"version"}, &textOut, &textOut); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"v9.8.7", "0123456789abcdef", "platform", "go"} {
		if !strings.Contains(textOut.String(), want) {
			t.Fatalf("version text missing %q: %s", want, textOut.String())
		}
	}
	var jsonOut bytes.Buffer
	if err := execute([]string{"version", "--json"}, &jsonOut, &jsonOut); err != nil {
		t.Fatal(err)
	}
	var info versionInfo
	if err := json.Unmarshal(jsonOut.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Version != buildVersion || info.Commit != buildCommit || info.Go == "" || info.Platform == "" {
		t.Fatalf("version json=%+v", info)
	}
}

func TestCLIReleaseREADMEConventionAndReleaseBaseline(t *testing.T) {
	readme, err := os.ReadFile(repositoryPath("README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)
	for _, want := range []string{"if command -v hq", "${TMPDIR:-/tmp}", "pwd -P", "trap cleanup_hq_smoke", "./bin/hq help", "board --cases-only"} {
		if !strings.Contains(text, want) {
			t.Fatalf("README missing %q", want)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "hq ") || strings.HasPrefix(trimmed, "ceo-office/tools/hq/bin/hq") {
			t.Fatalf("README mixed executable convention: %q", line)
		}
	}
	for _, path := range []string{repositoryPath("scripts", "release.sh"), repositoryPath("docs", "RELEASE.md")} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("release baseline missing %s: %v", path, err)
		}
	}
	script, err := os.ReadFile(repositoryPath("scripts", "release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"darwin/arm64", "linux/amd64", "linux/arm64", "-trimpath", "SHA256SUMS", "main.buildVersion", "main.buildCommit"} {
		if !strings.Contains(string(script), want) {
			t.Fatalf("release entry missing %q", want)
		}
	}
}

func TestCLIReleaseAcceptance(t *testing.T) {
	checks := []struct {
		name string
		run  func(*testing.T)
	}{
		{"AC01_full_case_flow", TestLedgerIndexCompleteFlowAndDeliveryAckProjection},
		{"AC02_payload_and_sources", TestLedgerIndexPayloadFieldsUseSeparatedHardLimits},
		{"AC03_index_delete_rebuild_consistency", TestLedgerIndexSQLiteRebuildQueryAndNoBodyCopy},
		{"AC04_delivery_pending_failed_sent_acked", TestLedgerIndexDeliveryProjectionMatrix},
		{"AC05_patrol_blocked_drift_orphan_two_signals", TestHerdrRuntimePatrolBlockedDriftOrphanAndGraceMatrix},
		{"AC06_doctor_company_health", func(t *testing.T) {
			TestHerdrRuntimeDoctorCompanyHealthFailsClosedAndNeverWrites(t)
			TestHerdrRuntimeDoctorRemediationPrefixesRemainSemantic(t)
		}},
		{"AC07_nudge_boundary_claim_dedupe", TestOperationsNudgeAtomicClaimDedupeTTLAndPromptRecovery},
		{"AC08_spawn_identity_environment", TestHerdrRuntimeUpInjectsIdentityRecordsSessionAndHandlesFailures},
		{"AC09_reminder_once_and_resolve", TestOperationsReminderOncePerCaseAndResolvedWithoutAuthorityMutation},
		{"AC10_estop_exempt_freeze_release", TestOperationsEstopManagersStayChildrenStopPatrolSuppressesAndReleaseRestores},
		{"AC11_session_started_stopped_jsonl", TestHerdrRuntimeSessionStrictLifecycleAndPhysicalDiagnostics},
		{"AC12_no_third_ledger_and_fake_fail_closed", func(t *testing.T) {
			assertNoThirdLedgerDependencies(t)
			TestOperationsPermissionsAndNoRealHerdrFallback(t)
		}},
		{"AC13_sequence_int64_gap_overflow_js", TestLedgerIndexSequenceInt64GapsOverflowAndJSSafety},
	}
	for index, check := range checks {
		t.Run(check.name, func(t *testing.T) { check.run(t); t.Logf("PASS acceptance=%02d name=%s", index+1, check.name) })
	}
}

func assertNoThirdLedgerDependencies(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(repositoryPath("go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(raw))
	for _, banned := range []string{"beads", "dolt"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("third ledger dependency found: %s", banned)
		}
	}
}

func shortSocketTempDir(t *testing.T) string {
	t.Helper()
	for _, base := range []string{"/private/tmp", "/tmp"} {
		info, err := os.Stat(base)
		if err != nil || !info.IsDir() {
			continue
		}
		root, err := os.MkdirTemp(base, "h-")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		return root
	}
	t.Fatal("no short temporary directory available for Unix socket fixture")
	return ""
}
