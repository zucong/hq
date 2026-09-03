package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
)

func directoryEntryNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func approvalDocumentBytes(t *testing.T, metadata ApprovalMetadata) []byte {
	t.Helper()
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(approvalHeaderMarker + string(raw) + metadataHeaderEnd + "# synthetic decision\n")
}

func baseIssueApproval(t *testing.T, e testEnv, caseID, source, target string) ApprovalMetadata {
	t.Helper()
	cleanSource, err := normalizeRef(source, e.root, true)
	if err != nil {
		t.Fatal(err)
	}
	return ApprovalMetadata{
		Version: 1, DecisionID: "DEC-" + caseID, Status: "effective",
		ConfirmedBy: testConfig().OwnerPrincipal, ConfirmedAt: "2026-08-28T00:00:00Z",
		Scopes: []ApprovalScope{{Action: "issue", CaseID: caseID, SourceRef: cleanSource, Target: target}},
	}
}

func TestApprovalReferenceApprovalMetadataScopeAndZeroSideEffects(t *testing.T) {
	negative := []struct {
		name string
		make func(*testing.T, testEnv, ApprovalMetadata) string
	}{
		{name: "body keyword only", make: func(t *testing.T, e testEnv, _ ApprovalMetadata) string {
			return writeTestFile(t, filepath.Join(e.office, "decisions", "bad.md"), "状态：未生效\n正文讨论生效\n")
		}},
		{name: "draft", make: func(t *testing.T, e testEnv, metadata ApprovalMetadata) string {
			metadata.Status = "draft"
			return writeTestFile(t, filepath.Join(e.office, "decisions", "bad.md"), string(approvalDocumentBytes(t, metadata)))
		}},
		{name: "unrelated case", make: func(t *testing.T, e testEnv, metadata ApprovalMetadata) string {
			metadata.Scopes[0].CaseID = "OTHER-CASE-001"
			return writeTestFile(t, filepath.Join(e.office, "decisions", "bad.md"), string(approvalDocumentBytes(t, metadata)))
		}},
		{name: "unrelated source", make: func(t *testing.T, e testEnv, metadata ApprovalMetadata) string {
			metadata.Scopes[0].SourceRef = filepath.Join(e.office, "other.md")
			return writeTestFile(t, filepath.Join(e.office, "decisions", "bad.md"), string(approvalDocumentBytes(t, metadata)))
		}},
		{name: "unrelated target", make: func(t *testing.T, e testEnv, metadata ApprovalMetadata) string {
			metadata.Scopes[0].Target = "baogong"
			return writeTestFile(t, filepath.Join(e.office, "decisions", "bad.md"), string(approvalDocumentBytes(t, metadata)))
		}},
		{name: "unknown field", make: func(t *testing.T, e testEnv, metadata ApprovalMetadata) string {
			raw, _ := json.Marshal(metadata)
			value := strings.TrimSuffix(string(raw), "}") + `,"unknown":true}`
			return writeTestFile(t, filepath.Join(e.office, "decisions", "bad.md"), approvalHeaderMarker+value+metadataHeaderEnd)
		}},
		{name: "duplicate field", make: func(t *testing.T, e testEnv, metadata ApprovalMetadata) string {
			raw, _ := json.Marshal(metadata)
			value := strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1)
			return writeTestFile(t, filepath.Join(e.office, "decisions", "bad.md"), approvalHeaderMarker+value+metadataHeaderEnd)
		}},
		{name: "future version", make: func(t *testing.T, e testEnv, metadata ApprovalMetadata) string {
			metadata.Version = 2
			return writeTestFile(t, filepath.Join(e.office, "decisions", "bad.md"), string(approvalDocumentBytes(t, metadata)))
		}},
		{name: "bad time", make: func(t *testing.T, e testEnv, metadata ApprovalMetadata) string {
			metadata.ConfirmedAt = "not-rfc3339"
			return writeTestFile(t, filepath.Join(e.office, "decisions", "bad.md"), string(approvalDocumentBytes(t, metadata)))
		}},
		{name: "multi json", make: func(t *testing.T, e testEnv, metadata ApprovalMetadata) string {
			raw, _ := json.Marshal(metadata)
			return writeTestFile(t, filepath.Join(e.office, "decisions", "bad.md"), approvalHeaderMarker+string(raw)+" {}"+metadataHeaderEnd)
		}},
		{name: "bom prefix", make: func(t *testing.T, e testEnv, metadata ApprovalMetadata) string {
			return writeTestFile(t, filepath.Join(e.office, "decisions", "bad.md"), "\ufeff"+string(approvalDocumentBytes(t, metadata)))
		}},
		{name: "duplicate block", make: func(t *testing.T, e testEnv, metadata ApprovalMetadata) string {
			doc := string(approvalDocumentBytes(t, metadata))
			return writeTestFile(t, filepath.Join(e.office, "decisions", "bad.md"), doc+doc)
		}},
		{name: "symlink", make: func(t *testing.T, e testEnv, metadata ApprovalMetadata) string {
			target := writeTestFile(t, filepath.Join(e.office, "decisions", "target.md"), string(approvalDocumentBytes(t, metadata)))
			link := filepath.Join(e.office, "decisions", "bad.md")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
		{name: "non regular", make: func(t *testing.T, e testEnv, _ ApprovalMetadata) string {
			path := filepath.Join(e.office, "decisions", "bad-dir")
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{name: "outside decisions", make: func(t *testing.T, e testEnv, metadata ApprovalMetadata) string {
			return writeTestFile(t, filepath.Join(e.root, "outside-decision.md"), string(approvalDocumentBytes(t, metadata)))
		}},
	}
	for _, test := range negative {
		t.Run(test.name, func(t *testing.T) {
			e := setupTestEnv(t)
			caseSource := writeTestFile(t, filepath.Join(e.office, "case.md"), "# case\n")
			order := writeTestFile(t, filepath.Join(e.office, "order.md"), "# order\n")
			metadata := baseIssueApproval(t, e, "APPROVAL-001", order, "zantianyou")
			decision := test.make(t, e, metadata)
			e.setActor(t, "penny", "approval:p", e.office)
			runTestCommand(t, e, "case", "create", "--id", "APPROVAL-001", "--title", "approval", "--source", caseSource)
			before, err := NewStore(e.data).ReadAll(testConfig())
			if err != nil {
				t.Fatal(err)
			}
			calls := len(e.transport.calls)
			app := e.app(t)
			err = app.run([]string{"issue", "--case", "APPROVAL-001", "--to", "zantianyou", "--decision", decision, "--next", "work"})
			if err == nil {
				t.Fatal("invalid approval was accepted")
			}
			after, readErr := NewStore(e.data).ReadAll(testConfig())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(after) != len(before) || len(e.transport.calls) != calls {
				t.Fatalf("rejected approval had side effects: events %d->%d transport %d->%d", len(before), len(after), calls, len(e.transport.calls))
			}
		})
	}

	t.Run("valid exact issue scope", func(t *testing.T) {
		e := setupTestEnv(t)
		caseSource := writeTestFile(t, filepath.Join(e.office, "case.md"), "# case\n")
		order := writeTestFile(t, filepath.Join(e.office, "order.md"), "# order\n")
		decision := writeIssueApproval(t, e, "approved.md", "DEC-APPROVAL-POSITIVE", "APPROVAL-POSITIVE-001", order, "zantianyou")
		e.setActor(t, "penny", "approval:p", e.office)
		runTestCommand(t, e, "case", "create", "--id", "APPROVAL-POSITIVE-001", "--title", "approval", "--source", caseSource)
		runTestCommand(t, e, "issue", "--case", "APPROVAL-POSITIVE-001", "--to", "zantianyou", "--decision", decision, "--next", "work")
		if len(e.transport.calls) != 1 {
			t.Fatalf("transport calls=%d, want 1", len(e.transport.calls))
		}
	})

	t.Run("duplicate decision id", func(t *testing.T) {
		e := setupTestEnv(t)
		order := writeTestFile(t, filepath.Join(e.office, "order.md"), "# order\n")
		metadata := baseIssueApproval(t, e, "APPROVAL-DUP-001", order, "zantianyou")
		first := writeTestFile(t, filepath.Join(e.office, "decisions", "first.md"), string(approvalDocumentBytes(t, metadata)))
		writeTestFile(t, filepath.Join(e.office, "decisions", "second.md"), string(approvalDocumentBytes(t, metadata)))
		if _, err := validateApproval(first, e.office, e.root, testConfig().ownerPrincipal()); err == nil || !strings.Contains(err.Error(), "重复") {
			t.Fatalf("duplicate decision_id was not rejected: %v", err)
		}
	})
}

func TestApprovalReferenceStaffDigestMismatchPrecedesConfigWrite(t *testing.T) {
	e := setupTestEnv(t)
	setRegistryMutationActor(t, e, "staff:p")
	role := addTestRoleCard(t, e, "DEC-APPROVAL-ROLE", "eng-approval-role", 1, "engineering/staff/eng-approval/v1", "synthesis")
	if err := os.Remove(e.config + ".lock"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	rule := AgentRule{
		Name: "eng-approval", Label: "工程部-合成员工", Nickname: "工程部-合成员工", DepartmentLabel: "engineering", Workspace: "hq-test", Responsibilities: []string{"staff:eng-approval"},
		ManualPath: role.ManualPath, RoleCardID: role.ID, RoleCardVersion: role.Version, RoleCardDigest: role.Digest,
		WorkstationPath: filepath.Dir(role.ManualPath), ActivationPolicy: activationOnAssignment, MaxWIP: 1, SeatVersion: 1, Department: "engineering",
		Kind: "codex", PermissionMode: "native", ReportsTo: "zantianyou", CanCreate: true,
	}
	rule.SeatDigest = employeeSeatDigest(rule)
	baseArgs := []string{"staff", "add", "--name", rule.Name, "--label", rule.Label, "--department", rule.Department,
		"--kind", rule.Kind, "--reports-to", rule.ReportsTo, "--role", roleCardKey(role.ID, role.Version),
		"--workstation", rule.WorkstationPath, "--activation", activationOnAssignment, "--max-wip", "1", "--grant", "create"}
	wrongDecision := writeApprovalDocument(t, filepath.Join(e.office, "decisions", "wrong.md"), "DEC-STAFF-WRONG", "effective", []ApprovalScope{{
		Action: "staff:add", Target: rule.Name, RequestDigest: strings.Repeat("0", 64),
	}})
	before, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	beforeEntries := directoryEntryNames(t, filepath.Dir(e.config))
	beforeData := snapshotTree(t, e.data)
	assertNoPersistentSideEffects := func(label string) {
		t.Helper()
		after, readErr := os.ReadFile(e.config)
		if readErr != nil {
			t.Fatal(readErr)
		}
		afterEntries := directoryEntryNames(t, filepath.Dir(e.config))
		if !bytes.Equal(before, after) || !equalStringSlices(beforeEntries, afterEntries) || len(e.transport.calls) != 0 {
			t.Fatalf("%s wrote persistent state: config_equal=%t entries=%v->%v calls=%d", label, bytes.Equal(before, after), beforeEntries, afterEntries, len(e.transport.calls))
		}
		if _, statErr := os.Lstat(e.config + ".lock"); !os.IsNotExist(statErr) {
			t.Fatalf("%s left config lock sidecar: %v", label, statErr)
		}
		if afterData := snapshotTree(t, e.data); !reflect.DeepEqual(afterData, beforeData) {
			t.Fatalf("%s changed ledger/records tree: before=%v after=%v", label, beforeData, afterData)
		}
	}
	app := e.app(t)
	err = app.run(append(append([]string(nil), baseArgs...), "--approval", wrongDecision))
	if err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("digest mismatch was not rejected: %v", err)
	}
	assertNoPersistentSideEffects("wrong approval")

	app = e.app(t)
	err = app.run(append([]string(nil), baseArgs...))
	if err == nil || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("missing approval was not rejected: %v", err)
	}
	assertNoPersistentSideEffects("missing approval")

	validDecision := writeApprovalDocument(t, filepath.Join(e.office, "decisions", "valid.md"), "DEC-STAFF-VALID", "effective", []ApprovalScope{{
		Action: "staff:add", Target: rule.Name, RequestDigest: staffScopeDigest("staff:add", rule),
	}})
	before, err = os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	beforeEntries = directoryEntryNames(t, filepath.Dir(e.config))
	app = e.app(t)
	app.DryRun = true
	err = app.run(append(append([]string(nil), baseArgs...), "--approval", validDecision))
	if err != nil {
		t.Fatalf("valid dry-run failed: %v", err)
	}
	assertNoPersistentSideEffects("dry-run")

	runTestCommand(t, e, append(append([]string(nil), baseArgs...), "--approval", validDecision)...)
	loaded, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.exactRule(rule.Name); !ok {
		t.Fatal("valid staff scope did not add employee")
	}
}

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-C", directory}, args...)...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestApprovalReferenceReferenceAllowlistAndGitCommit(t *testing.T) {
	base := shortSocketTempDir(t)
	hqRoot := filepath.Join(base, "headquarters")
	projectRoot := filepath.Join(base, "project")
	outsideRoot := filepath.Join(base, "outside")
	for _, directory := range []string{filepath.Join(hqRoot, "_archives"), filepath.Join(hqRoot, "engineering"), filepath.Join(hqRoot, "qa-ux"), projectRoot, outsideRoot} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	metadata, _ := json.Marshal(archiveReferenceMetadata{Version: 1, Root: projectRoot})
	writeTestFile(t, filepath.Join(hqRoot, "_archives", "project.md"), referenceHeaderMarker+string(metadata)+metadataHeaderEnd+"# project\n")
	hqFile := writeTestFile(t, filepath.Join(hqRoot, "README.md"), "# hq\n")
	engineeringFile := writeTestFile(t, filepath.Join(hqRoot, "engineering", "handoff.md"), "# handoff\n")
	qaFile := writeTestFile(t, filepath.Join(hqRoot, "qa-ux", "finding.md"), "# finding\n")
	projectFile := writeTestFile(t, filepath.Join(projectRoot, "artifact.md"), "# artifact\n")
	for _, value := range []string{hqFile, engineeringFile + "#line 7", qaFile, projectFile} {
		if _, err := normalizeRef(value, hqRoot, true); err != nil {
			t.Fatalf("valid reference %s: %v", value, err)
		}
	}

	outsideFile := writeTestFile(t, filepath.Join(outsideRoot, "outside.md"), "# outside\n")
	symlink := filepath.Join(hqRoot, "engineering", "symlink.md")
	if err := os.Symlink(projectFile, symlink); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(hqRoot, "engineering", "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(hqRoot, "engineering", "ref.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	sensitive := writeTestFile(t, filepath.Join(hqRoot, "engineering", "apikey-empty.txt"), "")
	invalid := []string{outsideFile, symlink, filepath.Join(hqRoot, "engineering"), fifo, socket, hqFile + "#", hqFile + "#one#two", hqFile + "#line\nnext", sensitive}
	for _, value := range invalid {
		if _, err := normalizeRef(value, hqRoot, true); err == nil {
			t.Fatalf("invalid reference accepted: %s", value)
		}
	}
	for name, mode := range map[string]os.FileMode{
		"device":      os.ModeDevice,
		"char-device": os.ModeDevice | os.ModeCharDevice,
	} {
		if isRegularReferenceMode(mode) {
			t.Fatalf("synthetic %s mode accepted as a regular reference", name)
		}
	}

	t.Run("path replacement is revalidated", func(t *testing.T) {
		victim := writeTestFile(t, filepath.Join(hqRoot, "engineering", "victim.md"), "# victim\n")
		policy := referencePolicy{HQRoot: hqRoot, BeforeOpen: func(path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Symlink(outsideFile, path)
		}}
		if _, err := normalizeRefWithPolicy(victim, policy, true); err == nil {
			t.Fatal("replaced path was accepted")
		}
	})

	repo := filepath.Join(projectRoot, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.name", "Synthetic")
	runGit(t, repo, "config", "user.email", "synthetic@example.invalid")
	writeTestFile(t, filepath.Join(repo, "file.txt"), "synthetic\n")
	runGit(t, repo, "add", "file.txt")
	runGit(t, repo, "commit", "-q", "-m", "synthetic")
	commit := runGit(t, repo, "rev-parse", "HEAD")
	blob := runGit(t, repo, "rev-parse", "HEAD:file.txt")
	tree := runGit(t, repo, "rev-parse", "HEAD^{tree}")
	if got, err := normalizeRef("git:"+repo+"@"+commit, hqRoot, true); err != nil || !strings.HasSuffix(got, "@"+commit) {
		t.Fatalf("valid commit rejected: got=%s err=%v", got, err)
	}
	fakeRepo := filepath.Join(projectRoot, "fake-repo")
	if err := os.Mkdir(fakeRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkRepo := filepath.Join(projectRoot, "repo-link")
	if err := os.Symlink(repo, symlinkRepo); err != nil {
		t.Fatal(err)
	}
	outsideRepo := filepath.Join(outsideRoot, "repo")
	if err := os.Mkdir(outsideRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, outsideRepo, "init", "-q")
	for _, value := range []string{
		"git:" + repo + "@" + strings.Repeat("f", 40),
		"git:" + repo + "@" + blob,
		"git:" + repo + "@" + tree,
		"git:" + fakeRepo + "@" + commit,
		"git:" + symlinkRepo + "@" + commit,
		"git:" + outsideRepo + "@" + commit,
	} {
		if _, err := normalizeRef(value, hqRoot, true); err == nil {
			t.Fatalf("invalid git reference accepted: %s", value)
		}
	}
}
