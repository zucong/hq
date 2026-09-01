package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRegistryRejectsUnreleasedAutoStartField(t *testing.T) {
	cfg := portableRegistry("strict-hq", "ceo-office", "delivery", "strict-secretary", "strict-manager", "strict-worker")
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("      activation_policy:")
	if !bytes.Contains(raw, marker) {
		t.Fatalf("fixture missing activation_policy marker:\n%s", raw)
	}
	withUnknownField := bytes.Replace(raw, marker, []byte("      auto_start: true\n      activation_policy:"), 1)
	if _, err := decodeCurrentConfig(withUnknownField); err == nil || !strings.Contains(err.Error(), "auto_start") {
		t.Fatalf("strict current registry accepted undeclared auto_start field: %v", err)
	}
}

func TestRegistryRejectsUnreleasedTabField(t *testing.T) {
	cfg := portableRegistry("strict-hq", "ceo-office", "delivery", "strict-secretary", "strict-manager", "strict-worker")
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("      department:")
	if !bytes.Contains(raw, marker) {
		t.Fatalf("fixture missing department marker:\n%s", raw)
	}
	withRemovedField := bytes.Replace(raw, marker, []byte("      tab: old-label\n      department:"), 1)
	if _, err := decodeCurrentConfig(withRemovedField); err == nil || !strings.Contains(err.Error(), "tab") {
		t.Fatalf("strict current registry accepted removed tab field: %v", err)
	}
}

func portableRegistry(workspace, officeDepartment, deliveryDepartment, witness, manager, worker string) Config {
	cfg := Config{
		Version: registrySchemaVersion, WorkspaceLabel: workspace, OwnerPrincipal: "Ada",
		Agents: []AgentRule{
			{Name: witness, Nickname: "Relay", DepartmentLabel: "Executive", Label: "Executive-Relay", Workspace: workspace,
				Responsibilities: []string{roleApprovalWitness, roleAccountCloser, "operations_admin"}, ManualPath: filepath.Join(officeDepartment, "roles", "channel.md"),
				Department: officeDepartment, Kind: "codex", ActivationPolicy: activationManual, CanCreate: true, CanIssue: true, CanAccept: true, CanClose: true, CanManageStaff: true, CanReceiveOrder: true},
			{Name: manager, Nickname: "Bridge", DepartmentLabel: "Delivery", Label: "Delivery-Bridge", Workspace: workspace,
				Responsibilities: []string{roleManagerPrefix + deliveryDepartment}, ManualPath: filepath.Join(deliveryDepartment, "roles", "manager.md"),
				Department: deliveryDepartment, Kind: "codex", ReportsTo: witness, ActivationPolicy: activationAlways, CanCreate: true, CanAccept: true, CanReceiveOrder: true},
			{Name: worker, Nickname: "Maker", DepartmentLabel: "Delivery", Label: "Delivery-Maker", Workspace: workspace,
				Responsibilities: []string{"developer:" + deliveryDepartment}, ManualPath: filepath.Join(deliveryDepartment, "roles", "developer.md"),
				Department: deliveryDepartment, Kind: "codex", ReportsTo: manager, ActivationPolicy: activationManual, CanCreate: true, CanAccept: true, CanReceiveOrder: true},
		},
	}
	return bindTestRoleContracts(cfg)
}

func writePortableRegistryFixture(t *testing.T, cfg Config, officeDepartment string) (root, office, config string) {
	t.Helper()
	root = canonicalTestTempDir(t)
	office = filepath.Join(root, officeDepartment)
	for _, rule := range cfg.Agents {
		path := filepath.Join(root, rule.ManualPath)
		writeTestFile(t, path, string(testRoleManual(rule.Name)))
	}
	config = filepath.Join(office, "tools", "hq", "config.yaml")
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, config, string(raw))
	return root, office, config
}

func TestRegistryPortabilitySyntheticWitnessApprovalIssueAck(t *testing.T) {
	cfgDoc := portableRegistry("north-hq", "executive", "delivery", "relay", "bridge", "maker")
	root, office, config := writePortableRegistryFixture(t, cfgDoc, "executive")
	cfg, err := loadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	managerRule, _ := cfg.exactRule("bridge")
	managerRule.CanClose = true
	if cfg.canCloseAsAccount(managerRule) {
		t.Fatal("registry accepted can_close without account_closer responsibility")
	}
	registryManager := AgentRule{Department: "delivery"}
	if err := applyPermissions(&registryManager, "manager", true); err != nil || !registryManager.hasResponsibility("manager:delivery") {
		t.Fatalf("manager permission did not use responsibility: rule=%+v err=%v", registryManager, err)
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
	setActor := func(name, pane string) {
		rule, _ := cfg.exactRule(name)
		identity.actors[pane] = Actor{Name: name, Label: rule.Label, Department: rule.Department, PaneID: pane, CWD: filepath.Join(root, rule.WorkstationPath), Rule: rule}
		app.CallerPane = pane
	}
	setActor("relay", "pane-relay")
	source := writeTestFile(t, filepath.Join(office, "source.md"), "# synthetic source\n")
	if err := app.run([]string{"case", "create", "--id", "REGISTRY-SYNTH-001", "--title", "synthetic", "--project", "test-project", "--source", source}); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if err := app.run([]string{"approval", "request", "--id", "APR-REGISTRY-SYNTH", "--case", "REGISTRY-SYNTH-001", "--target", "bridge", "--expires", expires}); err != nil {
		t.Fatal(err)
	}
	if err := app.run([]string{"approval", "grant", "--id", "APR-REGISTRY-SYNTH", "--issuer", "NotTheOwner"}); err == nil || !strings.Contains(err.Error(), "owner_principal") {
		t.Fatalf("registry accepted hard-coded owner principal: %v", err)
	}
	if err := app.run([]string{"approval", "grant", "--id", "APR-REGISTRY-SYNTH", "--issuer", cfg.OwnerPrincipal}); err != nil {
		t.Fatal(err)
	}
	if err := app.run([]string{"issue", "--case", "REGISTRY-SYNTH-001", "--to", "bridge", "--approval", "APR-REGISTRY-SYNTH", "--next", "ack"}); err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var sent Event
	for _, event := range events {
		if event.Type == "issue_sent" {
			sent = event
		}
	}
	if sent.ID == "" || sent.Actor != "relay" || sent.CapturedBy != "relay" || sent.Issuer != cfg.OwnerPrincipal {
		t.Fatalf("synthetic witness audit mismatch: %+v", sent)
	}
	setActor("bridge", "pane-bridge")
	if err := app.run([]string{"accept", "--event", sent.ID, "--next", "accepted"}); err != nil {
		t.Fatal(err)
	}
	changedOwner := cfg
	changedOwner.OwnerPrincipal = "Grace"
	if _, err := NewStore(data).ReadAll(changedOwner); err == nil {
		t.Fatal("owner principal change silently re-authorized historical approval")
	}
}

func TestRegistryPortabilityInitUpAndEnvelope(t *testing.T) {
	cfgDoc := portableRegistry("shop-hq", "front-office", "fulfillment", "courier", "foreman", "builder")
	root, office, config := writePortableRegistryFixture(t, cfgDoc, "front-office")
	data := filepath.Join(office, "records")
	cfg, err := loadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	control := newFakeHerdrControl(root, cfg.WorkspaceLabel)
	app, err := newAppWithDependencies(runtimePaths{Office: office, HQRoot: root, DataDir: data, ConfigPath: config}, cfg, globalOptions{}, AppDependencies{
		Store: NewStore(data), Identity: &fakeIdentityProvider{actors: map[string]Actor{}}, Transport: &fakeTransport{}, Herdr: control,
		Sessions: &FileSessionStore{Root: filepath.Join(data, "sessions")},
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.runUp([]string{"--no-gateway"}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(control.calls, "\n")
	for _, required := range []string{"agent start foreman", "prompt foreman", "fulfillment/staff/foreman/v1/AGENTS.md", "等待直属经理通过 hq issue"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("portable startup missing %q:\n%s", required, joined)
		}
	}
	if strings.Contains(joined, "当次任务正文") {
		t.Fatalf("startup envelope leaked task body: %s", joined)
	}
}

func TestRegistryPortabilityRegistryNegativeMatrix(t *testing.T) {
	base := portableRegistry("matrix-hq", "executive", "delivery", "relay", "bridge", "maker")
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"missing field", func(c *Config) { c.Agents[2].Nickname = "" }, "缺少"},
		{"duplicate identifier", func(c *Config) { c.Agents[2].Name = c.Agents[1].Name }, "重复"},
		{"duplicate responsibility", func(c *Config) { c.Agents[2].Responsibilities = []string{roleApprovalWitness} }, "职责位"},
		{"dangling report", func(c *Config) { c.Agents[2].ReportsTo = "absent" }, "未登记"},
		{"cross workspace", func(c *Config) { c.Agents[2].Workspace = "other-hq" }, "跨 workspace"},
		{"unauthorized witness", func(c *Config) { c.Agents[0].CanIssue = false }, roleApprovalWitness},
		{"wrong skeleton", func(c *Config) { c.Agents[2].ReportsTo = c.Agents[0].Name }, "经理职责位"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.Agents = append([]AgentRule(nil), base.Agents...)
			for i := range cfg.Agents {
				cfg.Agents[i].Responsibilities = append([]string(nil), base.Agents[i].Responsibilities...)
			}
			test.edit(&cfg)
			_, _, config := writePortableRegistryFixture(t, cfg, "executive")
			_, err := loadConfig(config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("negative accepted or wrong error: %v", err)
			}
		})
	}
}

func TestRegistryPortabilityManualMissingSymlinkAndBoundaryFailClosed(t *testing.T) {
	base := portableRegistry("manual-hq", "executive", "delivery", "relay", "bridge", "maker")
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string, *Config)
		want  string
	}{
		{"missing", func(t *testing.T, root string, cfg *Config) {
			_ = os.Remove(filepath.Join(root, cfg.Agents[2].ManualPath))
		}, "maker"},
		{"symlink", func(t *testing.T, root string, cfg *Config) {
			manual := filepath.Join(root, cfg.Agents[2].ManualPath)
			if err := os.Remove(manual); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(root, cfg.Agents[1].ManualPath), manual); err != nil {
				t.Fatal(err)
			}
		}, "symlink"},
		{"boundary", func(t *testing.T, _ string, cfg *Config) {
			cfg.Agents[2].ManualPath = "../outside.md"
			for index := range cfg.RoleCards {
				if cfg.RoleCards[index].ID == cfg.Agents[2].RoleCardID {
					cfg.RoleCards[index].ManualPath = "../outside.md"
				}
			}
		}, "workspace"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.Agents = append([]AgentRule(nil), base.Agents...)
			root, office, config := writePortableRegistryFixture(t, cfg, "executive")
			test.setup(t, root, &cfg)
			if test.name == "boundary" {
				raw, _ := yaml.Marshal(cfg)
				if err := os.WriteFile(config, raw, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			loaded, err := loadConfig(config)
			if err == nil {
				err = validateRegistryManuals(loaded, filepath.Dir(office))
			}
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("manual negative accepted or wrong error: %v", err)
			}
		})
	}
}
