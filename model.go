package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Event struct {
	Version                  int      `json:"event_version"`
	Sequence                 int64    `json:"sequence"`
	ID                       string   `json:"event_id"`
	CommandID                string   `json:"command_id"`
	CommandDigest            string   `json:"command_digest"`
	PreviousEventHash        string   `json:"previous_event_hash"`
	EventHash                string   `json:"event_hash"`
	CaseID                   string   `json:"case_id"`
	ParentCaseID             string   `json:"parent_case_id,omitempty"`
	RootCaseID               string   `json:"root_case_id,omitempty"`
	CaseVersion              int      `json:"case_version,omitempty"`
	CaseDigest               string   `json:"case_digest,omitempty"`
	PreviousCaseDigest       string   `json:"previous_case_digest,omitempty"`
	At                       string   `json:"at"`
	Type                     string   `json:"event_type"`
	Actor                    string   `json:"actor"`
	ActorLabel               string   `json:"actor_label"`
	ActorPaneID              string   `json:"actor_pane_id,omitempty"`
	Recipient                string   `json:"recipient,omitempty"`
	RecipientLabel           string   `json:"recipient_label,omitempty"`
	RelatedEventID           string   `json:"related_event_id,omitempty"`
	AssignmentEventID        string   `json:"assignment_event_id,omitempty"`
	AssignmentID             string   `json:"assignment_id,omitempty"`
	AssignmentDigest         string   `json:"assignment_digest,omitempty"`
	AssignmentIssuer         string   `json:"assignment_issuer,omitempty"`
	AssigneeSeatVersion      int      `json:"assignee_seat_version,omitempty"`
	AssigneeSeatDigest       string   `json:"assignee_seat_digest,omitempty"`
	RoleCardID               string   `json:"role_card_id,omitempty"`
	RoleCardVersion          int      `json:"role_card_version,omitempty"`
	RoleCardDigest           string   `json:"role_card_digest,omitempty"`
	RoleCardManualPath       string   `json:"role_card_manual_path,omitempty"`
	Reviewer                 string   `json:"reviewer,omitempty"`
	ReviewerLabel            string   `json:"reviewer_label,omitempty"`
	Acceptor                 string   `json:"acceptor,omitempty"`
	AcceptorLabel            string   `json:"acceptor_label,omitempty"`
	DueAt                    string   `json:"due_at,omitempty"`
	AcceptanceDigest         string   `json:"acceptance_digest,omitempty"`
	FromState                string   `json:"from_state,omitempty"`
	ToState                  string   `json:"to_state,omitempty"`
	Owner                    string   `json:"owner,omitempty"`
	Title                    string   `json:"title,omitempty"`
	Project                  string   `json:"project,omitempty"`
	Result                   string   `json:"result,omitempty"`
	Severity                 string   `json:"severity,omitempty"`
	SourceRef                string   `json:"source_ref,omitempty"`
	ArtifactRef              string   `json:"artifact_ref,omitempty"`
	ApprovalRef              string   `json:"approval_ref,omitempty"`
	ApprovalID               string   `json:"approval_id,omitempty"`
	ApprovalAction           string   `json:"approval_action,omitempty"`
	ApprovalStatus           string   `json:"approval_status,omitempty"`
	ApprovalMode             string   `json:"approval_mode,omitempty"`
	AuthorizationType        string   `json:"authorization_type,omitempty"`
	AuthorizationDigest      string   `json:"authorization_digest,omitempty"`
	DecisionRef              string   `json:"decision_ref,omitempty"`
	Issuer                   string   `json:"issuer,omitempty"`
	CapturedBy               string   `json:"captured_by,omitempty"`
	Objective                string   `json:"objective,omitempty"`
	Acceptance               string   `json:"acceptance,omitempty"`
	Constraints              string   `json:"constraints,omitempty"`
	Priority                 string   `json:"priority,omitempty"`
	SpecRef                  string   `json:"spec_ref,omitempty"`
	MessageKind              string   `json:"message_kind,omitempty"`
	MessageID                string   `json:"message_id,omitempty"`
	ThreadID                 string   `json:"thread_id,omitempty"`
	ReplyTo                  string   `json:"reply_to,omitempty"`
	RefFiles                 []string `json:"ref_files,omitempty"`
	RefCases                 []string `json:"ref_cases,omitempty"`
	RefMessages              []string `json:"ref_messages,omitempty"`
	RefEvents                []string `json:"ref_events,omitempty"`
	Location                 string   `json:"location,omitempty"`
	Verification             string   `json:"verification,omitempty"`
	NextAction               string   `json:"next_action,omitempty"`
	Note                     string   `json:"note,omitempty"`
	Delivery                 string   `json:"delivery,omitempty"`
	DeliveryID               string   `json:"delivery_id,omitempty"`
	DeliveryMode             string   `json:"delivery_mode,omitempty"`
	DeliveryTarget           string   `json:"delivery_target,omitempty"`
	DeliveryReason           string   `json:"delivery_reason,omitempty"`
	PayloadDigest            string   `json:"payload_digest,omitempty"`
	TurnBundleVersion        int      `json:"turn_bundle_version,omitempty"`
	TurnBundleDigest         string   `json:"turn_bundle_digest,omitempty"`
	TurnPromptDigest         string   `json:"turn_prompt_digest,omitempty"`
	TurnBundleDeliveryIDs    []string `json:"turn_bundle_delivery_ids,omitempty"`
	TurnBundlePayloadDigests []string `json:"turn_bundle_payload_digests,omitempty"`
	TurnBundleItemBytes      []int    `json:"turn_bundle_item_bytes,omitempty"`
	TurnBundleBytes          int      `json:"turn_bundle_bytes,omitempty"`
	TurnBundleOverflow       int      `json:"turn_bundle_overflow,omitempty"`
	TurnBundleMaxItems       int      `json:"turn_bundle_max_items,omitempty"`
	TurnBundleMaxBytes       int      `json:"turn_bundle_max_bytes,omitempty"`
	TurnBundleNextItemBytes  int      `json:"turn_bundle_next_item_bytes,omitempty"`
	TurnBundleBasePayload    string   `json:"turn_bundle_base_payload,omitempty"`
	TurnBundleEnvelopes      []string `json:"turn_bundle_envelopes,omitempty"`
	TurnBundleNextDeliveryID string   `json:"turn_bundle_next_delivery_id,omitempty"`
	TurnBundleNextDigest     string   `json:"turn_bundle_next_payload_digest,omitempty"`
	TurnBundleNextEnvelope   string   `json:"turn_bundle_next_envelope,omitempty"`
	TurnBundleParentAttempt  string   `json:"turn_bundle_parent_attempt_id,omitempty"`
	AttemptEventID           string   `json:"attempt_event_id,omitempty"`
	ResolutionRef            string   `json:"resolution_ref,omitempty"`
	NudgeID                  string   `json:"nudge_id,omitempty"`
	DedupeKey                string   `json:"dedupe_key,omitempty"`
	Message                  string   `json:"message,omitempty"`
	ExpiresAt                string   `json:"expires_at,omitempty"`
	ClaimID                  string   `json:"claim_id,omitempty"`
	ClaimExpiresAt           string   `json:"claim_expires_at,omitempty"`
	ReminderID               string   `json:"reminder_id,omitempty"`
	BasisEventID             string   `json:"basis_event_id,omitempty"`
	EstopID                  string   `json:"estop_id,omitempty"`
	WorkspaceID              string   `json:"workspace_id,omitempty"`
	TabID                    string   `json:"tab_id,omitempty"`
	PaneID                   string   `json:"pane_id,omitempty"`
	AgentKind                string   `json:"agent_kind,omitempty"`
	CWD                      string   `json:"cwd,omitempty"`
}

type CaseState struct {
	ID           string `json:"case_id"`
	ParentCaseID string `json:"parent_case_id,omitempty"`
	RootCaseID   string `json:"root_case_id,omitempty"`
	Title        string `json:"title"`
	Project      string `json:"project,omitempty"`
	Objective    string `json:"objective,omitempty"`
	Acceptance   string `json:"acceptance,omitempty"`
	Constraints  string `json:"constraints,omitempty"`
	Priority     string `json:"priority,omitempty"`
	SpecRef      string `json:"spec_ref,omitempty"`
	Version      int    `json:"version,omitempty"`
	Digest       string `json:"digest,omitempty"`
	SpecEventID  string `json:"spec_event_id,omitempty"`
	Status       string `json:"status"`
	Owner        string `json:"owner,omitempty"`
	Severity     string `json:"severity,omitempty"`
	SourceRef    string `json:"source_ref,omitempty"`
	NextAction   string `json:"next_action,omitempty"`
	LastEventID  string `json:"last_event_id"`
	UpdatedAt    string `json:"updated_at"`
}

type Snapshot struct {
	Version       int                   `json:"version"`
	GeneratedAt   string                `json:"generated_at"`
	EventCount    int                   `json:"event_count"`
	LastSequence  int64                 `json:"last_sequence"`
	LastEventHash string                `json:"last_event_hash"`
	Cases         map[string]*CaseState `json:"cases"`
}

type AgentRule struct {
	Name                string   `json:"name,omitempty" yaml:"name"`
	Label               string   `json:"label" yaml:"sender_label"`
	Nickname            string   `json:"nickname,omitempty" yaml:"nickname"`
	DepartmentLabel     string   `json:"department_label,omitempty" yaml:"department_label"`
	Workspace           string   `json:"workspace,omitempty" yaml:"workspace"`
	Responsibilities    []string `json:"responsibilities,omitempty" yaml:"responsibilities"`
	ManualPath          string   `json:"manual_path,omitempty" yaml:"manual_path"`
	RoleCardID          string   `json:"role_card_id,omitempty" yaml:"role_card_id,omitempty"`
	RoleCardVersion     int      `json:"role_card_version,omitempty" yaml:"role_card_version,omitempty"`
	RoleCardDigest      string   `json:"role_card_digest,omitempty" yaml:"role_card_digest,omitempty"`
	WorkstationPath     string   `json:"workstation_path,omitempty" yaml:"workstation_path,omitempty"`
	ActivationPolicy    string   `json:"activation_policy,omitempty" yaml:"activation_policy,omitempty"`
	KeepWarm            string   `json:"keep_warm,omitempty" yaml:"keep_warm,omitempty"`
	MaxWIP              int      `json:"max_wip,omitempty" yaml:"max_wip,omitempty"`
	SeatVersion         int      `json:"seat_version,omitempty" yaml:"seat_version,omitempty"`
	SeatDigest          string   `json:"seat_digest,omitempty" yaml:"seat_digest,omitempty"`
	Department          string   `json:"department" yaml:"department"`
	Kind                string   `json:"kind,omitempty" yaml:"kind,omitempty"`
	PermissionMode      string   `json:"permission_mode,omitempty" yaml:"permission_mode,omitempty"`
	AgentArgs           []string `json:"agent_args,omitempty" yaml:"agent_args,omitempty"`
	RuntimeFallbackKind string   `json:"-" yaml:"-"`
	ReportsTo           string   `json:"reports_to,omitempty" yaml:"reports_to"`
	Disabled            bool     `json:"disabled,omitempty" yaml:"disabled"`
	CanCreate           bool     `json:"can_create,omitempty" yaml:"can_create"`
	CanIssue            bool     `json:"can_issue,omitempty" yaml:"can_issue"`
	CanAccept           bool     `json:"can_accept,omitempty" yaml:"can_accept"`
	CanClose            bool     `json:"can_close,omitempty" yaml:"can_close"`
	CanManageStaff      bool     `json:"can_manage_staff,omitempty" yaml:"can_manage_staff"`
	CanReceiveOrder     bool     `json:"can_receive_order,omitempty" yaml:"can_receive_order"`
	ApprovalRef         string   `json:"approval_ref,omitempty" yaml:"approval_ref,omitempty"`
	UpdatedAt           string   `json:"updated_at,omitempty" yaml:"updated_at,omitempty"`
}

type RoleCard struct {
	ID           string   `json:"role_card_id" yaml:"role_card_id"`
	Version      int      `json:"version" yaml:"version"`
	Label        string   `json:"label" yaml:"label"`
	Department   string   `json:"department" yaml:"department"`
	Capabilities []string `json:"capabilities" yaml:"capabilities"`
	ManualPath   string   `json:"manual_path" yaml:"manual_path"`
	ManualDigest string   `json:"manual_digest" yaml:"manual_digest"`
	Digest       string   `json:"role_card_digest" yaml:"role_card_digest"`
	Status       string   `json:"status" yaml:"status"`
	ApprovalRef  string   `json:"approval_ref,omitempty" yaml:"approval_ref,omitempty"`
	CreatedAt    string   `json:"created_at,omitempty" yaml:"created_at,omitempty"`
}

type Config struct {
	Version         int                             `json:"version" yaml:"version"`
	WorkspaceLabel  string                          `json:"workspace_label,omitempty" yaml:"workspace_label"`
	OwnerPrincipal  string                          `json:"owner_principal,omitempty" yaml:"owner_principal"`
	RoleCards       []RoleCard                      `json:"role_cards,omitempty" yaml:"role_cards,omitempty"`
	Agents          []AgentRule                     `json:"agents" yaml:"agents"`
	DeliveryPolicy  *DeliveryPolicy                 `json:"delivery_policy,omitempty" yaml:"delivery_policy,omitempty"`
	RuntimeFallback *RuntimeFallbackPolicy          `json:"runtime_fallback,omitempty" yaml:"runtime_fallback,omitempty"`
	RuntimeProfiles map[string]RuntimeProfilePolicy `json:"runtime_profiles,omitempty" yaml:"runtime_profiles,omitempty"`
}

type DeliveryPolicy struct {
	DefaultMode               string `json:"default_mode" yaml:"default_mode"`
	MaxConsecutiveWakes       int    `json:"max_consecutive_wakes" yaml:"max_consecutive_wakes"`
	MaxBundleItems            int    `json:"max_bundle_items,omitempty" yaml:"max_bundle_items,omitempty"`
	MaxBundleBytes            int    `json:"max_bundle_bytes,omitempty" yaml:"max_bundle_bytes,omitempty"`
	AssignmentAcceptTimeout   string `json:"assignment_accept_timeout,omitempty" yaml:"assignment_accept_timeout,omitempty"`
	MaxActivationRedeliveries int    `json:"max_activation_redeliveries,omitempty" yaml:"max_activation_redeliveries,omitempty"`
	ManagerQueueStallTimeout  string `json:"manager_queue_stall_timeout,omitempty" yaml:"manager_queue_stall_timeout,omitempty"`
	ManagerQueueEscalateAfter string `json:"manager_queue_escalate_after,omitempty" yaml:"manager_queue_escalate_after,omitempty"`
	MaxManagerQueueNudges     int    `json:"max_manager_queue_nudges,omitempty" yaml:"max_manager_queue_nudges,omitempty"`
}

// RuntimeFallbackPolicy changes only the process that occupies a stable HQ
// seat. It deliberately lives outside AgentRule: an assignment remains bound
// to the same employee, role card, workstation and durable case even when a
// model provider cannot serve one turn.
type RuntimeFallbackPolicy struct {
	Auto           bool     `json:"auto" yaml:"auto"`
	Trigger        string   `json:"trigger" yaml:"trigger"`
	FromKind       string   `json:"from_kind" yaml:"from_kind"`
	ToKind         string   `json:"to_kind" yaml:"to_kind"`
	PermissionMode string   `json:"permission_mode" yaml:"permission_mode"`
	AgentArgs      []string `json:"agent_args,omitempty" yaml:"agent_args,omitempty"`
}

// RuntimeProfilePolicy is desired runtime state for a native Agent kind. It is
// deliberately company-level rather than part of an employee seat: changing a
// model carrier must not invalidate role cards, assignment contracts or WIP.
type RuntimeProfilePolicy struct {
	Model           string `json:"model" yaml:"model"`
	ReasoningEffort string `json:"reasoning_effort" yaml:"reasoning_effort"`
	OnDrift         string `json:"on_drift" yaml:"on_drift"`
}

func validateNativeAgentArgs(label string, args []string) error {
	if len(args) > 16 {
		return fmt.Errorf("%s 的 agent_args 最多 16 项", label)
	}
	for _, arg := range args {
		if arg == "" || strings.TrimSpace(arg) != arg || strings.ContainsAny(arg, "\r\n\x00") || utf8.RuneCountInString(arg) > 200 {
			return fmt.Errorf("%s 的 agent_args 必须是非空、无首尾空白、至多 200 rune 的单行 argv", label)
		}
	}
	return nil
}

func (c Config) runtimeKindAllowed(rule AgentRule, kind string) bool {
	if kind == rule.Kind {
		return true
	}
	if rule.RuntimeFallbackKind != "" && kind == rule.RuntimeFallbackKind {
		return true
	}
	policy := c.RuntimeFallback
	return policy != nil && policy.FromKind == rule.Kind && policy.ToKind == kind
}

func hydrateRuntimeFallback(cfg *Config) {
	if cfg == nil {
		return
	}
	for index := range cfg.Agents {
		cfg.Agents[index].RuntimeFallbackKind = ""
	}
	if cfg.RuntimeFallback == nil {
		return
	}
	for index := range cfg.Agents {
		if cfg.Agents[index].Kind == cfg.RuntimeFallback.FromKind {
			cfg.Agents[index].RuntimeFallbackKind = cfg.RuntimeFallback.ToKind
		}
	}
}

type Actor struct {
	Name       string
	Label      string
	Department string
	PaneID     string
	CWD        string
	Rule       AgentRule
}

var agentNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
var roleCardIDPattern = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,63}$`)

const (
	roleApprovalWitness = "approval_witness"
	roleAccountCloser   = "account_closer"
	roleManagerPrefix   = "manager:"
)

var responsibilityPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*(?::[a-z0-9][a-z0-9_-]*)?$`)

func validateOwnerPrincipal(value string) error {
	if value == "" {
		return fmt.Errorf("owner_principal 不能为空")
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("owner_principal 不得包含首尾空白")
	}
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 200 {
		return fmt.Errorf("owner_principal 必须是至多 200 个 Unicode 字符")
	}
	for _, current := range value {
		if unicode.IsControl(current) {
			return fmt.Errorf("owner_principal 不得包含控制字符")
		}
	}
	return nil
}

func (c Config) ownerPrincipal() string {
	return c.OwnerPrincipal
}

func validateConfig(cfg Config) error {
	if cfg.Version != registrySchemaVersion {
		return fmt.Errorf("不支持的配置版本 %d", cfg.Version)
	}
	if !agentNamePattern.MatchString(cfg.WorkspaceLabel) {
		return fmt.Errorf("workspace_label 必须是小写 ASCII 名称且不超过 32 字符")
	}
	if err := validateOwnerPrincipal(cfg.OwnerPrincipal); err != nil {
		return err
	}
	if cfg.DeliveryPolicy != nil {
		if !validDeliveryRequestMode(cfg.DeliveryPolicy.DefaultMode) {
			return fmt.Errorf("delivery_policy.default_mode 必须是 auto|wakeup|quiet|inject")
		}
		if cfg.DeliveryPolicy.MaxConsecutiveWakes < 1 || cfg.DeliveryPolicy.MaxConsecutiveWakes > 100 {
			return fmt.Errorf("delivery_policy.max_consecutive_wakes 必须在 1..100")
		}
		if cfg.DeliveryPolicy.MaxBundleItems < 0 || cfg.DeliveryPolicy.MaxBundleItems > maxDeliveryBundleItems {
			return fmt.Errorf("delivery_policy.max_bundle_items 必须省略/为 0，或在 1..%d", maxDeliveryBundleItems)
		}
		if cfg.DeliveryPolicy.MaxBundleBytes < 0 || cfg.DeliveryPolicy.MaxBundleBytes > maxDeliveryBundleBytes {
			return fmt.Errorf("delivery_policy.max_bundle_bytes 必须省略/为 0，或在 1..%d", maxDeliveryBundleBytes)
		}
		if value := strings.TrimSpace(cfg.DeliveryPolicy.AssignmentAcceptTimeout); value != "" {
			duration, err := time.ParseDuration(value)
			if err != nil || duration < 15*time.Second || duration > time.Hour {
				return fmt.Errorf("delivery_policy.assignment_accept_timeout 必须是 15s..1h 的 Go duration")
			}
		}
		if value := cfg.DeliveryPolicy.MaxActivationRedeliveries; value < 0 || value > 10 {
			return fmt.Errorf("delivery_policy.max_activation_redeliveries 必须省略/为 0，或在 1..10")
		}
		stall := defaultManagerQueueStallTimeout
		if value := strings.TrimSpace(cfg.DeliveryPolicy.ManagerQueueStallTimeout); value != "" {
			duration, err := time.ParseDuration(value)
			if err != nil || duration < 15*time.Second || duration > time.Hour {
				return fmt.Errorf("delivery_policy.manager_queue_stall_timeout 必须是 15s..1h 的 Go duration")
			}
			stall = duration
		}
		escalate := defaultManagerQueueEscalateAfter
		if value := strings.TrimSpace(cfg.DeliveryPolicy.ManagerQueueEscalateAfter); value != "" {
			duration, err := time.ParseDuration(value)
			if err != nil || duration < 30*time.Second || duration > 24*time.Hour {
				return fmt.Errorf("delivery_policy.manager_queue_escalate_after 必须是 30s..24h 的 Go duration")
			}
			escalate = duration
		}
		if escalate <= stall {
			return fmt.Errorf("delivery_policy.manager_queue_escalate_after 必须大于 manager_queue_stall_timeout")
		}
		if value := cfg.DeliveryPolicy.MaxManagerQueueNudges; value < 0 || value > 5 {
			return fmt.Errorf("delivery_policy.max_manager_queue_nudges 必须省略/为 0，或在 1..5")
		}
	}
	if policy := cfg.RuntimeFallback; policy != nil {
		if policy.Trigger != "content_safeguard" {
			return fmt.Errorf("runtime_fallback.trigger 必须是 content_safeguard")
		}
		if strings.TrimSpace(policy.FromKind) == "" || strings.TrimSpace(policy.ToKind) == "" || policy.FromKind == policy.ToKind {
			return fmt.Errorf("runtime_fallback.from_kind/to_kind 必须是两个不同的非空 agent kind")
		}
		if policy.PermissionMode != "native" && policy.PermissionMode != "yolo" {
			return fmt.Errorf("runtime_fallback.permission_mode 必须显式为 native|yolo")
		}
		if err := validateNativeAgentArgs("runtime_fallback", policy.AgentArgs); err != nil {
			return err
		}
	}
	if err := validateRuntimeProfiles(cfg); err != nil {
		return err
	}
	seen := map[string]bool{}
	roles := map[string]string{}
	staffMaintainers := 0
	for _, rule := range cfg.Agents {
		if !agentNamePattern.MatchString(rule.Name) {
			return fmt.Errorf("agent 名 %q 不符合 herdr 命名规则", rule.Name)
		}
		if rule.Label == "" || rule.Department == "" {
			return fmt.Errorf("agent %s 缺少 label/department", rule.Name)
		}
		if rule.Nickname == "" || rule.DepartmentLabel == "" || rule.Workspace == "" || rule.ManualPath == "" || len(rule.Responsibilities) == 0 {
			return fmt.Errorf("agent %s 缺少 nickname/department_label/workspace/responsibilities/manual_path", rule.Name)
		}
		if rule.Workspace != cfg.WorkspaceLabel {
			return fmt.Errorf("agent %s 跨 workspace：%s != %s", rule.Name, rule.Workspace, cfg.WorkspaceLabel)
		}
		if !safeDepartment(rule.Department) {
			return fmt.Errorf("agent %s 的 department 非法：%q", rule.Name, rule.Department)
		}
		if seen[rule.Name] {
			return fmt.Errorf("agent %s 重复配置", rule.Name)
		}
		if !rule.Disabled && rule.Kind == "" {
			return fmt.Errorf("在职 agent %s 缺少 kind", rule.Name)
		}
		if rule.PermissionMode != "native" && rule.PermissionMode != "yolo" {
			return fmt.Errorf("agent %s 的 permission_mode 必须显式为 native|yolo", rule.Name)
		}
		if err := validateNativeAgentArgs("agent "+rule.Name, rule.AgentArgs); err != nil {
			return err
		}
		if rule.CanManageStaff && !rule.Disabled {
			staffMaintainers++
		}
		if rule.CanReceiveOrder && !rule.CanAccept {
			return fmt.Errorf("agent %s 配置了 can_receive_order 但缺少 can_accept；接令席位必须能确认 assignment", rule.Name)
		}
		seen[rule.Name] = true
		for _, role := range rule.Responsibilities {
			if !responsibilityPattern.MatchString(role) {
				return fmt.Errorf("agent %s 的职责位非法：%q", rule.Name, role)
			}
			if owner, exists := roles[role]; exists {
				return fmt.Errorf("职责位 %s 重复配置给 %s 与 %s", role, owner, rule.Name)
			}
			roles[role] = rule.Name
		}
	}
	if staffMaintainers == 0 {
		return fmt.Errorf("至少需要一名在职 agent 具备 can_manage_staff 权限")
	}
	for _, rule := range cfg.Agents {
		if rule.ReportsTo == "" {
			continue
		}
		if rule.ReportsTo == rule.Name {
			return fmt.Errorf("agent %s 不能向自己汇报", rule.Name)
		}
		if !seen[rule.ReportsTo] {
			return fmt.Errorf("agent %s 的 reports_to 未登记：%s", rule.Name, rule.ReportsTo)
		}
		parent, _ := configRuleIncludingDisabled(cfg, rule.ReportsTo)
		if !rule.Disabled && parent.Disabled {
			return fmt.Errorf("在职 agent %s 不能向已停用 agent %s 汇报", rule.Name, rule.ReportsTo)
		}
		if !cfg.isManager(rule) && !cfg.isManager(parent) {
			return fmt.Errorf("agent %s 的 reports_to %s 未登记经理职责位", rule.Name, rule.ReportsTo)
		}
		if cfg.isManager(rule) && !cfg.isManager(parent) && !parent.hasResponsibility(roleAccountCloser) {
			return fmt.Errorf("经理 %s 的 reports_to %s 既非经理也非销账职责位", rule.Name, rule.ReportsTo)
		}
	}
	for _, rule := range cfg.Agents {
		if !rule.Disabled && rule.ReportsTo == "" && !rule.hasResponsibility(roleAccountCloser) {
			return fmt.Errorf("在职 agent %s 缺少汇报线且不是根销账职责位", rule.Name)
		}
		for _, role := range rule.Responsibilities {
			if strings.HasPrefix(role, roleManagerPrefix) && role != roleManagerPrefix+rule.Department {
				return fmt.Errorf("agent %s 的经理职责位 %s 与 department %s 不匹配", rule.Name, role, rule.Department)
			}
		}
	}
	witness, ok := cfg.uniqueRole(roleApprovalWitness)
	if !ok || !witness.CanIssue {
		return fmt.Errorf("职责位 %s 必须唯一授予一名在职且 can_issue 的 agent", roleApprovalWitness)
	}
	if cfg.isManager(witness) {
		return fmt.Errorf("职责位 %s 不得兼任 manager；否则会绕过公司所有者 approval 使用经理内建授权", roleApprovalWitness)
	}
	closer, ok := cfg.uniqueRole(roleAccountCloser)
	if !ok || !closer.CanClose {
		return fmt.Errorf("职责位 %s 必须唯一授予一名在职且 can_close 的 agent", roleAccountCloser)
	}
	for _, rule := range cfg.Agents {
		visited := map[string]bool{rule.Name: true}
		current := rule.ReportsTo
		for current != "" {
			if visited[current] {
				return fmt.Errorf("汇报线存在环：%s", rule.Name)
			}
			visited[current] = true
			parent, _ := configRuleIncludingDisabled(cfg, current)
			current = parent.ReportsTo
		}
	}
	return validateRoleCardRegistry(cfg)
}

func (r AgentRule) hasResponsibility(role string) bool {
	for _, current := range r.Responsibilities {
		if current == role {
			return true
		}
	}
	return false
}

func (c Config) uniqueRole(role string) (AgentRule, bool) {
	var found AgentRule
	count := 0
	for _, rule := range c.Agents {
		if !rule.Disabled && rule.Workspace == c.WorkspaceLabel && rule.hasResponsibility(role) {
			found, count = rule, count+1
		}
	}
	return found, count == 1
}

func (c Config) isManager(rule AgentRule) bool {
	for _, role := range rule.Responsibilities {
		if strings.HasPrefix(role, roleManagerPrefix) {
			return true
		}
	}
	return false
}

func safeDepartment(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value
}

func configRuleIncludingDisabled(cfg Config, name string) (AgentRule, bool) {
	for _, rule := range cfg.Agents {
		if rule.Name == name {
			return rule, true
		}
	}
	return AgentRule{}, false
}

func (c Config) ruleFor(name string) (AgentRule, bool) {
	for _, rule := range c.Agents {
		if rule.Name == name && !rule.Disabled {
			return rule, true
		}
	}
	return AgentRule{}, false
}

func (c Config) exactRule(name string) (AgentRule, bool) {
	for _, rule := range c.Agents {
		if rule.Name == name && !rule.Disabled {
			return rule, true
		}
	}
	return AgentRule{}, false
}

func newSnapshot() Snapshot {
	return Snapshot{
		Version:     snapshotSchemaVersion,
		GeneratedAt: time.Time{}.UTC().Format(time.RFC3339),
		Cases:       map[string]*CaseState{},
	}
}

func applyEvent(snapshot *Snapshot, event Event) {
	snapshot.EventCount++
	snapshot.LastSequence = event.Sequence
	snapshot.LastEventHash = event.EventHash
	if isInfrastructureEvent(event.Type) || isApprovalMessageProjectionNeutralEvent(event.Type) {
		snapshot.GeneratedAt = event.At
		return
	}
	stateful := event.Type == "case_created" || event.Type == "case_revised" || event.ToState != ""
	// Delivery resolutions for case-less messages are authoritative ledger
	// events, but they do not describe a business case. Preserve the snapshot
	// tail metadata without manufacturing Cases[""]. A stateful event is never
	// hidden by this projection guard: transition validation must reject an
	// empty CaseID before applyEvent, and a direct projection remains visible.
	if event.CaseID == "" && !stateful {
		snapshot.GeneratedAt = event.At
		return
	}
	state, ok := snapshot.Cases[event.CaseID]
	if !ok {
		state = &CaseState{ID: event.CaseID, Status: "unknown"}
		snapshot.Cases[event.CaseID] = state
	}
	if !stateful {
		state.LastEventID = event.ID
		state.UpdatedAt = event.At
		snapshot.GeneratedAt = event.At
		return
	}
	if event.Title != "" {
		state.Title = event.Title
	}
	if event.Project != "" {
		state.Project = event.Project
	}
	if event.Type == "case_created" || event.Type == "case_revised" {
		state.ParentCaseID, state.RootCaseID = event.ParentCaseID, event.RootCaseID
		state.Objective, state.Acceptance, state.Constraints = event.Objective, event.Acceptance, event.Constraints
		state.Priority, state.SpecRef = event.Priority, event.SpecRef
		state.Version, state.Digest = event.CaseVersion, event.CaseDigest
		state.SpecEventID = event.ID
		state.SourceRef = event.SourceRef
	}
	if event.ToState != "" {
		state.Status = event.ToState
	}
	if event.Owner != "" {
		state.Owner = event.Owner
	}
	if event.Severity != "" {
		state.Severity = event.Severity
	}
	if event.Type == "case_created" && event.SourceRef != "" {
		state.SourceRef = event.SourceRef
	}
	if event.NextAction != "" {
		state.NextAction = event.NextAction
	}
	state.LastEventID = event.ID
	state.UpdatedAt = event.At
	snapshot.GeneratedAt = event.At
}
