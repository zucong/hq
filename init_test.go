package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

type initTestEnv struct{ root, office, config, data string }

type initManifestEntry struct {
	Type   fs.FileMode
	Mode   fs.FileMode
	Size   int64
	SHA256 string
}

func newInitTestEnv(t *testing.T) initTestEnv {
	t.Helper()
	parent := canonicalTestTempDir(t)
	root := filepath.Join(parent, "test-company")
	office := filepath.Join(root, "ceo-office")
	return initTestEnv{root: root, office: office, config: defaultConfigPath(office), data: filepath.Join(office, "records")}
}

func (e initTestEnv) args(flagsFirst bool) []string {
	flags := []string{"--silent", "--company-name", "Test Company", "--owner", "ZC", "--workspace", "test-company-hq", "--template", "minimal", "--prepare-only"}
	_ = flagsFirst
	return append([]string{"init", e.root}, flags...)
}

func runInitForTest(t *testing.T, e initTestEnv, flagsFirst bool) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if err := execute(e.args(flagsFirst), &out, &errOut); err != nil {
		t.Fatalf("init failed: %v\nstderr=%s\nstdout=%s", err, errOut.String(), out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("init wrote stderr: %s", errOut.String())
	}
	return out.String()
}

func initTreeManifest(t *testing.T, root string) map[string]initManifestEntry {
	t.Helper()
	manifest := map[string]initManifestEntry{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		item := initManifestEntry{Type: info.Mode().Type(), Mode: info.Mode().Perm(), Size: info.Size()}
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(raw)
			item.SHA256 = hex.EncodeToString(sum[:])
		}
		manifest[rel] = item
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func manifestPaths(manifest map[string]initManifestEntry) []string {
	result := make([]string, 0, len(manifest))
	for path := range manifest {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func assertNoParallelOrganizationRoster(t *testing.T, manifest map[string]initManifestEntry) {
	t.Helper()
	for path := range manifest {
		if strings.EqualFold(filepath.Base(path), "ROSTER.md") {
			t.Fatalf("init generated forbidden parallel organization registry: %s", path)
		}
	}
}

func initRuleByRole(t *testing.T, cfg Config, role string) AgentRule {
	t.Helper()
	for _, rule := range cfg.Agents {
		if rule.hasResponsibility(role) {
			return rule
		}
	}
	t.Fatalf("missing role %s", role)
	return AgentRule{}
}

func TestInitMinimalCreatesCompleteValidatedCompany(t *testing.T) {
	e := newInitTestEnv(t)
	output := runInitForTest(t, e, false)
	want := []string{
		".", "AGENT-HANDBOOK.md", "COMPANY.md",
		"ceo-office", "ceo-office/decisions", "ceo-office/decisions/company-init.md", "ceo-office/records",
		"ceo-office/staff", "ceo-office/staff/secretary", "ceo-office/staff/secretary/v1", "ceo-office/staff/secretary/v1/AGENTS.md",
		"ceo-office/tools", "ceo-office/tools/hq", "ceo-office/tools/hq/bin", "ceo-office/tools/hq/bin/hq", "ceo-office/tools/hq/config.yaml",
		"delivery", "delivery/staff",
		"delivery/staff/delivery-manager", "delivery/staff/delivery-manager/v1", "delivery/staff/delivery-manager/v1/AGENTS.md",
		"delivery/staff/delivery-specialist", "delivery/staff/delivery-specialist/v1", "delivery/staff/delivery-specialist/v1/AGENTS.md",
	}
	manifest := initTreeManifest(t, e.root)
	assertNoParallelOrganizationRoster(t, manifest)
	if got := manifestPaths(manifest); !reflect.DeepEqual(got, want) {
		t.Fatalf("tree=%v want=%v", got, want)
	}
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 3 || len(cfg.RoleCards) != 3 || cfg.OwnerPrincipal != "ZC" || cfg.WorkspaceLabel != "test-company-hq" {
		t.Fatalf("config=%+v", cfg)
	}
	secretary := initRuleByRole(t, cfg, roleApprovalWitness)
	if !secretary.CanManageStaff || !secretary.CanClose || !secretary.CanIssue {
		t.Fatalf("secretary=%+v", secretary)
	}
	manager := initRuleByRole(t, cfg, "manager:delivery")
	worker := initRuleByRole(t, cfg, "specialist:delivery")
	if manager.ReportsTo != secretary.Name || worker.ReportsTo != manager.Name {
		t.Fatalf("invalid hierarchy manager=%+v worker=%+v", manager, worker)
	}
	if secretary.ActivationPolicy != activationAlways || manager.ActivationPolicy != activationAlways || worker.ActivationPolicy != activationOnAssignment || worker.MaxWIP != 1 {
		t.Fatalf("invalid activation secretary=%+v manager=%+v worker=%+v", secretary, manager, worker)
	}
	for _, rule := range cfg.Agents {
		if rule.PermissionMode != "native" || rule.WorkstationPath == "" || rule.ManualPath != filepath.Join(rule.WorkstationPath, "AGENTS.md") || rule.RoleCardDigest == "" || rule.SeatDigest != employeeSeatDigest(rule) {
			t.Fatalf("invalid role/seat binding: %+v", rule)
		}
	}
	if info, err := os.Stat(filepath.Join(e.office, "tools", "hq", "bin", "hq")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("binary info=%v err=%v", info, err)
	}
	if _, _, err := readApproval(filepath.Join(e.office, "decisions", "company-init.md"), e.office, "ZC", true); err != nil {
		t.Fatal(err)
	}
	companyRaw, err := os.ReadFile(filepath.Join(e.root, "COMPANY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if company := string(companyRaw); !strings.Contains(company, "ceo-office/tools/hq/config.yaml") || !strings.Contains(company, "唯一组织编制") {
		t.Fatalf("COMPANY.md does not identify config.yaml as the sole organization registry:\n%s", company)
	}
	for _, fragment := range []string{"公司本地 HQ 二进制", "通过静态校验", "未连接 Herdr"} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("output missing %q:\n%s", fragment, output)
		}
	}
}

func TestInitAllTemplatesProduceValidRegistries(t *testing.T) {
	for _, item := range companyTemplates {
		t.Run(item.Slug, func(t *testing.T) {
			e := newInitTestEnv(t)
			workspace := map[string]string{"minimal": "minimal-hq", "product-engineering": "product-hq", "saas": "saas-hq", "professional-services": "services-hq", "commerce": "commerce-hq", "virtual-company": "virtual-hq"}[item.Slug]
			args := []string{"init", e.root, "--silent", "--company-name", item.Label, "--owner", "ZC", "--workspace", workspace, "--template", item.Slug, "--prepare-only", "--permission-mode", "native"}
			var out, errOut bytes.Buffer
			if err := execute(args, &out, &errOut); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			cfg, err := loadConfig(e.config)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateRegistryManuals(cfg, e.root); err != nil {
				t.Fatal(err)
			}
			for _, rule := range cfg.Agents {
				if rule.PermissionMode != "native" || nativeAgentArgs(rule) != nil {
					t.Fatalf("permission mode not honored: %+v", rule)
				}
			}
		})
	}
}

func TestInitFreezesExplicitSecretaryAndDefaultAgentArgs(t *testing.T) {
	e := newInitTestEnv(t)
	args := []string{
		"init", e.root, "--silent", "--company-name", "Explicit Args Company", "--owner", "ZC",
		"--workspace", "explicit-args-hq", "--template", "minimal", "--permission-mode", "native", "--prepare-only",
		"--secretary-agent-arg=-c", `--secretary-agent-arg=model_reasoning_effort="medium"`,
		"--default-agent-arg=--sandbox", "--default-agent-arg=danger-full-access",
	}
	var out, errOut bytes.Buffer
	if err := execute(args, &out, &errOut); err != nil {
		t.Fatalf("init with agent args failed: %v\nstderr=%s\nstdout=%s", err, errOut.String(), out.String())
	}
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	secretary := initRuleByRole(t, cfg, roleApprovalWitness)
	if !reflect.DeepEqual(secretary.AgentArgs, []string{"-c", `model_reasoning_effort="medium"`}) || secretary.SeatDigest != employeeSeatDigest(secretary) {
		t.Fatalf("secretary argv/digest not frozen: %+v", secretary)
	}
	for _, rule := range cfg.Agents {
		if rule.Name == secretary.Name {
			continue
		}
		if !reflect.DeepEqual(rule.AgentArgs, []string{"--sandbox", "danger-full-access"}) || rule.SeatDigest != employeeSeatDigest(rule) {
			t.Fatalf("default argv/digest not frozen: %+v", rule)
		}
	}
}

func TestInitCustomSecretaryNameIsRoleBasedOwnerChannel(t *testing.T) {
	e := newInitTestEnv(t)
	args := []string{
		"init", e.root, "--silent", "--company-name", "Custom Liaison Company", "--owner", "HUMAN-OWNER",
		"--workspace", "liaison-hq", "--template", "virtual-company", "--prepare-only",
		"--secretary-name", "owner-channel", "--secretary-nickname", "总部联络官",
	}
	var out, errOut bytes.Buffer
	if err := execute(args, &out, &errOut); err != nil {
		t.Fatalf("init with custom secretary failed: %v\nstderr=%s\nstdout=%s", err, errOut.String(), out.String())
	}
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	secretary := initRuleByRole(t, cfg, roleApprovalWitness)
	wantName := scopedAgentName("liaison-hq", "owner-channel")
	if secretary.Name != wantName || secretary.Nickname != "总部联络官" || secretary.RoleCardID != "owner-channel" {
		t.Fatalf("custom secretary identity was not frozen from init input: %+v", secretary)
	}
	if strings.Contains(strings.ToLower(secretary.Name+" "+secretary.Nickname+" "+secretary.Label), "penny") {
		t.Fatalf("custom secretary leaked a template-specific name: %+v", secretary)
	}
	for _, rule := range cfg.Agents {
		if cfg.isManager(rule) && rule.ReportsTo != secretary.Name {
			t.Fatalf("manager %s does not report to the configured owner channel %s: %+v", rule.Name, secretary.Name, rule)
		}
	}
	envelope := startupEnvelopeWithBinary(secretary, cfg.ownerPrincipal(), "/company/hq")
	for _, want := range []string{"agent=" + secretary.Name, "等待公司所有者的正式治理输入"} {
		if !strings.Contains(envelope, want) {
			t.Fatalf("custom secretary startup envelope missing %q: %s", want, envelope)
		}
	}
}

func TestInitCustomSecretaryNameCollisionFailsBeforeWriting(t *testing.T) {
	e := newInitTestEnv(t)
	args := []string{
		"init", e.root, "--silent", "--company-name", "Collision Company", "--owner", "HUMAN-OWNER",
		"--workspace", "collision-hq", "--template", "virtual-company", "--prepare-only",
		"--secretary-name", "product-manager", "--secretary-nickname", "重名联络官",
	}
	var out, errOut bytes.Buffer
	err := execute(args, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "重复配置") {
		t.Fatalf("colliding secretary slug was not rejected clearly: %v\nstderr=%s\nstdout=%s", err, errOut.String(), out.String())
	}
	if _, statErr := os.Lstat(e.root); !os.IsNotExist(statErr) {
		t.Fatalf("colliding secretary init wrote target state before validation: %v", statErr)
	}
}

func TestInitVirtualCompanyCreatesTenIndependentSpecialistCards(t *testing.T) {
	e := newInitTestEnv(t)
	args := []string{"init", e.root, "--silent", "--company-name", "Role Card Company", "--owner", "ZC", "--workspace", "virtual-hq", "--template", "virtual-company", "--prepare-only", "--permission-mode", "native"}
	var out, errOut bytes.Buffer
	if err := execute(args, &out, &errOut); err != nil {
		t.Fatalf("virtual-company init failed: %v\nstderr=%s\nstdout=%s", err, errOut.String(), out.String())
	}
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 14 || len(cfg.RoleCards) != 14 {
		t.Fatalf("virtual registry agents=%d role_cards=%d, want 14/14", len(cfg.Agents), len(cfg.RoleCards))
	}
	for _, manager := range []struct {
		name, nickname string
	}{
		{scopedAgentName("virtual-hq", "engineering-manager"), "数字工程负责人"},
		{scopedAgentName("virtual-hq", "quality-manager"), "质量与业务验收负责人"},
	} {
		rule, ok := cfg.exactRule(manager.name)
		if !ok || rule.Nickname != manager.nickname || !cfg.isManager(rule) {
			t.Fatalf("virtual-company manager must use role-style identity: name=%s rule=%+v ok=%t", manager.name, rule, ok)
		}
	}
	for _, retiredPersonName := range []string{
		scopedAgentName("virtual-hq", "zantianyou"),
		scopedAgentName("virtual-hq", "baogong"),
	} {
		if _, exists := configRuleIncludingDisabled(cfg, retiredPersonName); exists {
			t.Fatalf("virtual-company must not generate historical-person manager identity: %s", retiredPersonName)
		}
	}
	assertNoParallelOrganizationRoster(t, initTreeManifest(t, e.root))
	if err := validateRegistryManuals(cfg, e.root); err != nil {
		t.Fatal(err)
	}

	type specialistExpectation struct {
		roleID, department, managerBase, anchor string
	}
	expected := map[string]specialistExpectation{
		"data_engineer:engineering":         {"eng-data-engineer", "engineering", "engineering-manager", "DATA_LINEAGE_BEFORE_METRICS"},
		"application_developer:engineering": {"eng-app-developer", "engineering", "engineering-manager", "ROLLBACK_BEFORE_SHIP"},
		"security_engineer:engineering":     {"eng-security-engineer", "engineering", "engineering-manager", "THREAT_MODEL_BEFORE_APPROVAL"},
		"product_researcher:product":        {"product-researcher", "product", "product-manager", "EVIDENCE_BEFORE_PRODUCT_CLAIM"},
		"browser_blackbox:quality":          {"qa-browser-blackbox", "quality", "quality-manager", "OBSERVE_ONLY_THROUGH_PUBLIC_UI"},
		"code_reviewer:engineering":         {"eng-code-reviewer", "engineering", "engineering-manager", "REVIEW_DIFF_NOT_INTENT"},
		"copy_reviewer:product":             {"product-copy-reviewer", "product", "product-manager", "ONE_CONCEPT_ONE_TERM"},
		"data_gate:quality":                 {"qa-data-gate", "quality", "quality-manager", "NO_EVIDENCE_NO_GATE"},
		"first_use_tester:quality":          {"qa-first-use", "quality", "quality-manager", "FIRST_USE_CLEAN_ROOM"},
		"usability_reviewer:quality":        {"qa-usability", "quality", "quality-manager", "WALK_THE_CRITICAL_PATH"},
	}
	manuals, anchors := map[string]bool{}, map[string]bool{}
	seenSpecialists := 0
	for responsibility, want := range expected {
		rule := initRuleByRole(t, cfg, responsibility)
		seenSpecialists++
		managerName := scopedAgentName("virtual-hq", want.managerBase)
		wantWorkstation := filepath.Join(want.department, "staff", want.roleID, "v1")
		if rule.RoleCardID != want.roleID || rule.RoleCardVersion != 1 || rule.Department != want.department || rule.ReportsTo != managerName ||
			rule.WorkstationPath != wantWorkstation || rule.ManualPath != filepath.Join(wantWorkstation, "AGENTS.md") {
			t.Fatalf("specialist %s binding=%+v want=%+v", responsibility, rule, want)
		}
		if rule.ActivationPolicy != activationOnAssignment || rule.MaxWIP != 1 || rule.CanCreate || rule.CanIssue {
			t.Fatalf("specialist %s has unsafe activation/authority: %+v", responsibility, rule)
		}
		if manuals[rule.ManualPath] {
			t.Fatalf("duplicate specialist manual: %s", rule.ManualPath)
		}
		manuals[rule.ManualPath] = true
		card, ok := cfg.roleCard(rule.RoleCardID, rule.RoleCardVersion)
		if !ok || card.Status != roleCardApproved || card.Digest != rule.RoleCardDigest || card.ManualPath != rule.ManualPath {
			t.Fatalf("specialist %s invalid role card: %+v ok=%t", responsibility, card, ok)
		}
		raw, err := os.ReadFile(filepath.Join(e.root, rule.ManualPath))
		if err != nil {
			t.Fatal(err)
		}
		manual := string(raw)
		for _, fragment := range []string{want.anchor, "HQ × Herdr 工作协议", "Assignment Contract", "不得自行修改本文件或 registry"} {
			if !strings.Contains(manual, fragment) {
				t.Fatalf("manual %s missing %q:\n%s", rule.ManualPath, fragment, manual)
			}
		}
		if anchors[want.anchor] {
			t.Fatalf("behavior anchor reused: %s", want.anchor)
		}
		anchors[want.anchor] = true
		if roleCardFileDigest(raw) != card.ManualDigest || card.Digest != roleCardDigest(card) || rule.SeatDigest != employeeSeatDigest(rule) {
			t.Fatalf("digest chain invalid for %s", responsibility)
		}
	}
	if seenSpecialists != 10 || len(manuals) != 10 || len(anchors) != 10 {
		t.Fatalf("specialists=%d manuals=%d anchors=%d", seenSpecialists, len(manuals), len(anchors))
	}

	for _, responsibility := range []string{roleApprovalWitness, "manager:product", "manager:engineering", "manager:quality"} {
		rule := initRuleByRole(t, cfg, responsibility)
		if rule.ActivationPolicy != activationAlways || rule.MaxWIP < 2 {
			t.Fatalf("management seat is not always-on: %+v", rule)
		}
	}
	secretary := initRuleByRole(t, cfg, roleApprovalWitness)
	if secretary.Name != scopedAgentName("virtual-hq", "secretary") || secretary.Nickname != "总裁秘书" {
		t.Fatalf("virtual-company owner channel did not use the role-based default: %+v", secretary)
	}

	handbookRaw, err := os.ReadFile(filepath.Join(e.root, "AGENT-HANDBOOK.md"))
	if err != nil {
		t.Fatal(err)
	}
	handbook := string(handbookRaw)
	for _, fragment := range []string{"角色卡是岗位", "Assignment 是合同", "Herdr Prompt 是门铃", "明确授权代理", "外部工具权限", "virtual-hq", "on_assignment", "Recursive Self-Improvement", "config.yaml", "唯一组织编制"} {
		if !strings.Contains(handbook, fragment) {
			t.Fatalf("agent handbook missing %q:\n%s", fragment, handbook)
		}
	}
}

func TestInitSecondRunIsPureNoOpAndMismatchFails(t *testing.T) {
	e := newInitTestEnv(t)
	runInitForTest(t, e, false)
	before := initTreeManifest(t, e.root)
	output := runInitForTest(t, e, true)
	after := initTreeManifest(t, e.root)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("second init mutated company")
	}
	if strings.Contains(output, "CREATE") || !strings.Contains(output, "created=0") {
		t.Fatalf("output=%s", output)
	}
	args := e.args(false)
	for index := range args {
		if args[index] == "minimal" {
			args[index] = "saas"
		}
	}
	var out bytes.Buffer
	if err := execute(args, &out, &out); err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("mismatch err=%v out=%s", err, out.String())
	}
}

func TestInitSilentMissingFieldsWritesNothing(t *testing.T) {
	e := newInitTestEnv(t)
	var out bytes.Buffer
	err := execute([]string{"init", e.root, "--silent", "--owner", "ZC", "--prepare-only"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "--company-name") || !strings.Contains(err.Error(), "--template") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Lstat(e.root); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("root unexpectedly created: %v", statErr)
	}
}

func TestInitRejectsUnrelatedNonEmptyDirectory(t *testing.T) {
	e := newInitTestEnv(t)
	if err := os.MkdirAll(e.root, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(e.root, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := execute(e.args(false), &out, &out)
	if err == nil || !strings.Contains(err.Error(), "非空") {
		t.Fatalf("err=%v out=%s", err, out.String())
	}
	raw, _ := os.ReadFile(sentinel)
	if string(raw) != "keep" {
		t.Fatal("sentinel changed")
	}
}

func TestInitConcurrentCallsConverge(t *testing.T) {
	e := newInitTestEnv(t)
	const callers = 8
	var wg sync.WaitGroup
	errorsCh := make(chan error, callers)
	for index := 0; index < callers; index++ {
		wg.Add(1)
		go func() { defer wg.Done(); var out bytes.Buffer; errorsCh <- execute(e.args(false), &out, &out) }()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 3 {
		t.Fatalf("agents=%d", len(cfg.Agents))
	}
}

func TestInitInteractiveWizardAndHelp(t *testing.T) {
	e := newInitTestEnv(t)
	var out bytes.Buffer
	app, _ := newInitApp(globalOptions{}, &out, &out)
	app.In = strings.NewReader("Interactive Co\nZC\ninteractive-hq\n2\nowner-channel\n总部联络官\nclaude\ncodex\nnative\nyes\n")
	if err := app.cmdInit([]string{"--prepare-only", e.root}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 5 || cfg.WorkspaceLabel != "interactive-hq" {
		t.Fatalf("cfg=%+v", cfg)
	}
	secretary := initRuleByRole(t, cfg, roleApprovalWitness)
	if secretary.Name != scopedAgentName("interactive-hq", "owner-channel") || secretary.Nickname != "总部联络官" {
		t.Fatalf("interactive wizard lost custom secretary identity: %+v", secretary)
	}
	out.Reset()
	if err := execute([]string{"init", "--help"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hq init <company-directory>", "--silent", "--prepare-only", "不连接或调用任何 LLM API", "product-engineering"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q:\n%s", want, out.String())
		}
	}
}

func TestInitDefaultStartsSecretaryThenGatewayThenRoster(t *testing.T) {
	e := newInitTestEnv(t)
	control := newFakeHerdrControl(e.root, "test-company-hq")
	gateway := &fakeGatewayState{health: GatewayHealth{Error: "offline"}}
	control.onRunPane = func(_ string, command string) {
		marker := "--server-id '"
		start := strings.Index(command, marker)
		if start < 0 {
			return
		}
		start += len(marker)
		end := strings.Index(command[start:], "'")
		if end >= 0 {
			gateway.setOnline("w-test", command[start:start+end])
		}
	}
	var out bytes.Buffer
	app, _ := newInitApp(globalOptions{}, &out, &out)
	app.Herdr, app.GatewayHealth = control, gateway
	args := []string{"--silent", "--company-name", "Test Company", "--owner", "ZC", "--workspace", "test-company-hq", "--template", "minimal", e.root}
	if err := app.cmdInit(args); err != nil {
		t.Fatalf("init start: %v\n%s", err, out.String())
	}
	joined := strings.Join(control.calls, "\n")
	secretaryName := scopedAgentName("test-company-hq", "secretary")
	managerName := scopedAgentName("test-company-hq", "delivery-manager")
	secretary := strings.Index(joined, "agent start "+secretaryName)
	gatewayTab := strings.Index(joined, "tab create hq-gateway")
	manager := strings.Index(joined, "agent start "+managerName)
	if secretary < 0 || gatewayTab <= secretary || manager <= gatewayTab {
		t.Fatalf("wrong startup order:\n%s", joined)
	}
	if strings.Contains(joined, "--approve-for-me") || !strings.Contains(out.String(), "公司已建立并启动") {
		t.Fatalf("startup incomplete:\n%s\n%s", joined, out.String())
	}
}

func TestPingDefaultsToConfiguredCompanyWorkspaceNotCallerEnvironment(t *testing.T) {
	t.Setenv("HERDR_WORKSPACE_ID", "wrong-caller-workspace")
	control := newFakeHerdrControl(t.TempDir(), "company-hq")
	gateway := &fakeGatewayState{}
	gateway.setOnline("w-test", "gateway-test")
	var out bytes.Buffer
	app := &App{Config: Config{WorkspaceLabel: "company-hq"}, Herdr: control, GatewayHealth: gateway, DataDir: t.TempDir(), Out: &out, Err: &out}
	if err := app.cmdPing(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "workspace=w-test") {
		t.Fatalf("output=%s", out.String())
	}
}
