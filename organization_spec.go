package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	organizationSpecVersion  = 1
	maxOrganizationSpecBytes = 1 << 20
)

var behaviorAnchorPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`)

// organizationSpec is an immutable company-formation input. It is compiled
// into Config; config.yaml remains the sole live organization registry.
type organizationSpec struct {
	Version     int                      `yaml:"version"`
	ID          string                   `yaml:"id"`
	Label       string                   `yaml:"label"`
	Departments []organizationDepartment `yaml:"departments"`
	Seats       []organizationSeat       `yaml:"seats"`
}

type organizationDepartment struct {
	ID    string `yaml:"id"`
	Label string `yaml:"label"`
}

type organizationSeat struct {
	ID               string                  `yaml:"id"`
	Nickname         string                  `yaml:"nickname"`
	Department       string                  `yaml:"department"`
	ReportsTo        string                  `yaml:"reports_to"`
	Responsibilities []string                `yaml:"responsibilities"`
	Activation       string                  `yaml:"activation"`
	KeepWarm         string                  `yaml:"keep_warm"`
	MaxWIP           int                     `yaml:"max_wip"`
	RuntimeProfile   string                  `yaml:"runtime_profile"`
	Permissions      organizationPermissions `yaml:"permissions"`
	Role             organizationRole        `yaml:"role"`
}

type organizationPermissions struct {
	Create       bool `yaml:"create"`
	Issue        bool `yaml:"issue"`
	Accept       bool `yaml:"accept"`
	Close        bool `yaml:"close"`
	ManageStaff  bool `yaml:"manage_staff"`
	ReceiveOrder bool `yaml:"receive_order"`
}

type organizationRole struct {
	Capabilities   []string `yaml:"capabilities"`
	Mission        string   `yaml:"mission"`
	Temperament    string   `yaml:"temperament"`
	BehaviorAnchor string   `yaml:"behavior_anchor"`
	Duties         []string `yaml:"duties"`
	Method         []string `yaml:"method"`
	Evidence       []string `yaml:"evidence"`
	Boundaries     []string `yaml:"boundaries"`
}

type compiledOrganizationSpec struct {
	Spec     organizationSpec
	Raw      []byte
	Digest   string
	Config   Config
	Profiles map[string]roleManualProfile
}

func loadAndCompileOrganizationSpec(path string, opts initOptions) (compiledOrganizationSpec, error) {
	cleanPath := strings.TrimSpace(path)
	if cleanPath == "" {
		return compiledOrganizationSpec{}, fmt.Errorf("--organization-spec 不能为空")
	}
	abs, err := filepath.Abs(cleanPath)
	if err != nil {
		return compiledOrganizationSpec{}, fmt.Errorf("解析 organization spec %s：%w", cleanPath, err)
	}
	abs = filepath.Clean(abs)
	parent, err := canonicalReferenceDirectory(filepath.Dir(abs))
	if err != nil {
		return compiledOrganizationSpec{}, fmt.Errorf("organization spec 父目录必须是 canonical 非 symlink 目录：%w", err)
	}
	file, _, err := openAllowedRegularFile(abs, []allowedReferenceRoot{{lexical: filepath.Dir(abs), canonical: parent}}, nil)
	if err != nil {
		return compiledOrganizationSpec{}, fmt.Errorf("安全打开 organization spec %s：%w", cleanPath, err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxOrganizationSpecBytes+1))
	if err != nil {
		return compiledOrganizationSpec{}, fmt.Errorf("读取 organization spec %s：%w", cleanPath, err)
	}
	if len(raw) > maxOrganizationSpecBytes {
		return compiledOrganizationSpec{}, fmt.Errorf("organization spec 超过 %d bytes", maxOrganizationSpecBytes)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '{' || trimmed[0] == '[' {
		return compiledOrganizationSpec{}, fmt.Errorf("organization spec 必须是非空 YAML 文档，不接受 JSON")
	}
	var spec organizationSpec
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&spec); err != nil {
		return compiledOrganizationSpec{}, fmt.Errorf("严格解析 organization spec：%w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return compiledOrganizationSpec{}, fmt.Errorf("organization spec 只能包含一个 YAML 文档")
		}
		return compiledOrganizationSpec{}, fmt.Errorf("读取 organization spec 尾部：%w", err)
	}
	compiled, err := compileOrganizationSpec(spec, opts)
	if err != nil {
		return compiledOrganizationSpec{}, err
	}
	compiled.Raw = append([]byte(nil), raw...)
	sum := sha256.Sum256(raw)
	compiled.Digest = hex.EncodeToString(sum[:])
	return compiled, nil
}

func compileOrganizationSpec(spec organizationSpec, opts initOptions) (compiledOrganizationSpec, error) {
	if spec.Version != organizationSpecVersion {
		return compiledOrganizationSpec{}, fmt.Errorf("organization spec version 必须为 %d", organizationSpecVersion)
	}
	if !roleCardIDPattern.MatchString(spec.ID) {
		return compiledOrganizationSpec{}, fmt.Errorf("organization spec id 必须是合法稳定 slug")
	}
	if _, err := organizationText("organization label", spec.Label, 200); err != nil {
		return compiledOrganizationSpec{}, err
	}
	if len(spec.Departments) == 0 || len(spec.Departments) > 16 {
		return compiledOrganizationSpec{}, fmt.Errorf("organization spec departments 必须在 1..16 项")
	}
	if len(spec.Seats) == 0 || len(spec.Seats) > 128 {
		return compiledOrganizationSpec{}, fmt.Errorf("organization spec seats 必须在 1..128 项")
	}

	departmentLabels := make(map[string]string, len(spec.Departments))
	for _, department := range spec.Departments {
		if !safeDepartment(department.ID) || !agentNamePattern.MatchString(department.ID) {
			return compiledOrganizationSpec{}, fmt.Errorf("organization department id 非法：%q", department.ID)
		}
		label, err := organizationText("department "+department.ID+" label", department.Label, 200)
		if err != nil {
			return compiledOrganizationSpec{}, err
		}
		if _, exists := departmentLabels[department.ID]; exists {
			return compiledOrganizationSpec{}, fmt.Errorf("organization department 重复：%s", department.ID)
		}
		departmentLabels[department.ID] = label
	}
	if _, ok := departmentLabels["ceo-office"]; !ok {
		return compiledOrganizationSpec{}, fmt.Errorf("organization spec 必须显式包含 ceo-office")
	}

	seatNames := make(map[string]string, len(spec.Seats))
	for _, seat := range spec.Seats {
		if !agentNamePattern.MatchString(seat.ID) || !roleCardIDPattern.MatchString(seat.ID) {
			return compiledOrganizationSpec{}, fmt.Errorf("organization seat id 非法：%q", seat.ID)
		}
		if _, exists := seatNames[seat.ID]; exists {
			return compiledOrganizationSpec{}, fmt.Errorf("organization seat 重复：%s", seat.ID)
		}
		seatNames[seat.ID] = scopedAgentName(opts.Workspace, seat.ID)
	}
	fullNames := map[string]string{}
	for base, full := range seatNames {
		if previous := fullNames[full]; previous != "" {
			return compiledOrganizationSpec{}, fmt.Errorf("organization seat %s 与 %s 经 workspace scope 后名称冲突：%s", previous, base, full)
		}
		fullNames[full] = base
	}

	agents := make([]AgentRule, 0, len(spec.Seats))
	roleCards := make([]RoleCard, 0, len(spec.Seats))
	profiles := make(map[string]roleManualProfile, len(spec.Seats))
	for _, seat := range spec.Seats {
		departmentLabel, ok := departmentLabels[seat.Department]
		if !ok {
			return compiledOrganizationSpec{}, fmt.Errorf("organization seat %s 引用了未登记 department：%s", seat.ID, seat.Department)
		}
		if seat.ReportsTo != "" {
			if _, ok := seatNames[seat.ReportsTo]; !ok {
				return compiledOrganizationSpec{}, fmt.Errorf("organization seat %s reports_to 未登记：%s", seat.ID, seat.ReportsTo)
			}
		}
		nickname, err := organizationText("seat "+seat.ID+" nickname", seat.Nickname, 200)
		if err != nil {
			return compiledOrganizationSpec{}, err
		}
		responsibilities, err := canonicalStringSet(seat.Responsibilities)
		if err != nil || len(responsibilities) == 0 {
			if err == nil {
				err = fmt.Errorf("至少需要一项")
			}
			return compiledOrganizationSpec{}, fmt.Errorf("organization seat %s responsibilities：%w", seat.ID, err)
		}
		if !validActivationPolicy(seat.Activation) {
			return compiledOrganizationSpec{}, fmt.Errorf("organization seat %s activation 必须是 always|on_assignment|manual", seat.ID)
		}
		if seat.RuntimeProfile != "owner_channel" && seat.RuntimeProfile != "default" {
			return compiledOrganizationSpec{}, fmt.Errorf("organization seat %s runtime_profile 必须是 owner_channel|default", seat.ID)
		}
		profile, err := compileOrganizationRole(seat.ID, seat.Role)
		if err != nil {
			return compiledOrganizationSpec{}, err
		}
		kind, agentArgs := opts.DefaultAgentKind, append([]string(nil), opts.DefaultAgentArgs...)
		if seat.RuntimeProfile == "owner_channel" {
			kind, agentArgs = opts.SecretaryKind, append([]string(nil), opts.SecretaryAgentArgs...)
		}
		workstation := strings.Join([]string{seat.Department, "staff", seat.ID, "v1"}, "/")
		rule := AgentRule{
			Name: seatNames[seat.ID], Nickname: nickname, DepartmentLabel: departmentLabel,
			Label: departmentLabel + "-" + nickname, Workspace: opts.Workspace,
			Responsibilities: responsibilities, Department: seat.Department,
			Kind: kind, AgentArgs: agentArgs, PermissionMode: opts.PermissionMode,
			ActivationPolicy: seat.Activation, KeepWarm: strings.TrimSpace(seat.KeepWarm), MaxWIP: seat.MaxWIP,
			CanCreate: seat.Permissions.Create, CanIssue: seat.Permissions.Issue,
			CanAccept: seat.Permissions.Accept, CanClose: seat.Permissions.Close,
			CanManageStaff: seat.Permissions.ManageStaff, CanReceiveOrder: seat.Permissions.ReceiveOrder,
			RoleCardID: seat.ID, RoleCardVersion: 1, WorkstationPath: workstation,
			ManualPath: workstation + "/AGENTS.md", SeatVersion: 1,
		}
		if seat.ReportsTo != "" {
			rule.ReportsTo = seatNames[seat.ReportsTo]
		}
		manual := agentRoleCardManualWithProfile(opts.CompanyName, opts.Workspace, rule, profile)
		card := RoleCard{ID: seat.ID, Version: 1, Label: nickname, Department: seat.Department,
			Capabilities: profile.Capabilities, ManualPath: rule.ManualPath, ManualDigest: roleCardFileDigest(manual),
			Status: roleCardApproved, ApprovalRef: "ceo-office/decisions/company-init.md"}
		card.Digest = roleCardDigest(card)
		rule.RoleCardDigest = card.Digest
		rule.SeatDigest = employeeSeatDigest(rule)
		agents = append(agents, rule)
		roleCards = append(roleCards, card)
		profiles[rule.Name] = profile
	}
	cfg := Config{Version: registrySchemaVersion, WorkspaceLabel: opts.Workspace, OwnerPrincipal: opts.Owner,
		RoleCards: roleCards, Agents: agents, DeliveryPolicy: &DeliveryPolicy{DefaultMode: "auto", MaxConsecutiveWakes: 3}}
	if err := validateConfig(cfg); err != nil {
		return compiledOrganizationSpec{}, fmt.Errorf("organization spec 生成了无效配置：%w", err)
	}
	return compiledOrganizationSpec{Spec: spec, Config: cfg, Profiles: profiles}, nil
}

func compileOrganizationRole(seatID string, role organizationRole) (roleManualProfile, error) {
	capabilities, err := canonicalStringSet(role.Capabilities)
	if err != nil || len(capabilities) == 0 {
		if err == nil {
			err = fmt.Errorf("至少需要一项")
		}
		return roleManualProfile{}, fmt.Errorf("organization seat %s role capabilities：%w", seatID, err)
	}
	mission, err := organizationText("seat "+seatID+" role mission", role.Mission, 1000)
	if err != nil {
		return roleManualProfile{}, err
	}
	temperament, err := organizationText("seat "+seatID+" role temperament", role.Temperament, 1000)
	if err != nil {
		return roleManualProfile{}, err
	}
	anchor := strings.TrimSpace(role.BehaviorAnchor)
	if !behaviorAnchorPattern.MatchString(anchor) {
		return roleManualProfile{}, fmt.Errorf("organization seat %s behavior_anchor 必须匹配 %s", seatID, behaviorAnchorPattern.String())
	}
	duties, err := organizationTextList("seat "+seatID+" role duties", role.Duties)
	if err != nil {
		return roleManualProfile{}, err
	}
	method, err := organizationTextList("seat "+seatID+" role method", role.Method)
	if err != nil {
		return roleManualProfile{}, err
	}
	evidence, err := organizationTextList("seat "+seatID+" role evidence", role.Evidence)
	if err != nil {
		return roleManualProfile{}, err
	}
	boundaries, err := organizationTextList("seat "+seatID+" role boundaries", role.Boundaries)
	if err != nil {
		return roleManualProfile{}, err
	}
	return roleManualProfile{Mission: mission, Temperament: temperament, BehaviorAnchor: anchor,
		Capabilities: capabilities, Duties: duties, Method: method, Evidence: evidence, Boundaries: boundaries}, nil
}

func organizationTextList(name string, values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 32 {
		return nil, fmt.Errorf("organization %s 必须在 1..32 项", name)
	}
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		clean, err := organizationText(name, value, 1000)
		if err != nil {
			return nil, err
		}
		if seen[clean] {
			return nil, fmt.Errorf("organization %s 包含重复项", name)
		}
		seen[clean] = true
		result = append(result, clean)
	}
	return result, nil
}

func organizationText(name, value string, maxRunes int) (string, error) {
	clean := strings.TrimSpace(value)
	if clean == "" || clean != value || !utf8.ValidString(clean) || strings.ContainsAny(clean, "\r\n\x00") || utf8.RuneCountInString(clean) > maxRunes {
		return "", fmt.Errorf("organization %s 必须是无首尾空白、不含换行、至多 %d rune 的非空文本", name, maxRunes)
	}
	for _, current := range clean {
		if unicode.IsControl(current) {
			return "", fmt.Errorf("organization %s 不得包含控制字符", name)
		}
	}
	return clean, nil
}

func sortedOrganizationDepartments(spec organizationSpec) []organizationDepartment {
	result := append([]organizationDepartment(nil), spec.Departments...)
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
