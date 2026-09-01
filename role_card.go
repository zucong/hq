package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	roleCardContractVersion     = 1
	roleCardApproved            = "approved"
	roleCardRetired             = "retired"
	activationAlways            = "always"
	activationOnAssignment      = "on_assignment"
	activationManual            = "manual"
	defaultOnAssignmentKeepWarm = 30 * time.Second
	maximumOnAssignmentKeepWarm = time.Hour
)

func roleCardKey(id string, version int) string {
	return strings.TrimSpace(id) + "@" + strconv.Itoa(version)
}

func parseRoleCardRef(value string) (string, int, error) {
	clean := strings.TrimSpace(value)
	index := strings.LastIndex(clean, "@")
	if index <= 0 || index == len(clean)-1 {
		return "", 0, fmt.Errorf("role 必须使用 id@version：%s", value)
	}
	version, err := strconv.Atoi(clean[index+1:])
	if err != nil || version < 1 {
		return "", 0, fmt.Errorf("role version 必须是正整数：%s", value)
	}
	id := clean[:index]
	if !roleCardIDPattern.MatchString(id) {
		return "", 0, fmt.Errorf("role_card_id 非法：%s", id)
	}
	return id, version, nil
}

func canonicalStringSet(values []string) ([]string, error) {
	result := append([]string(nil), values...)
	for index := range result {
		result[index] = strings.TrimSpace(result[index])
		if !responsibilityPattern.MatchString(result[index]) {
			return nil, fmt.Errorf("capability 非法：%q", result[index])
		}
	}
	sort.Strings(result)
	for index := 1; index < len(result); index++ {
		if result[index] == result[index-1] {
			return nil, fmt.Errorf("capability 重复：%s", result[index])
		}
	}
	return result, nil
}

func roleCardFileDigest(raw []byte) string {
	return digestText(string(raw))
}

func roleCardDigest(card RoleCard) string {
	capabilities := append([]string(nil), card.Capabilities...)
	sort.Strings(capabilities)
	parts := []string{
		strconv.Itoa(roleCardContractVersion), card.ID, strconv.Itoa(card.Version), card.Label,
		card.Department, card.ManualPath, card.ManualDigest,
	}
	parts = append(parts, capabilities...)
	return requestDigest("role-card-v1", parts...)
}

func employeeSeatDigest(rule AgentRule) string {
	responsibilities := append([]string(nil), rule.Responsibilities...)
	sort.Strings(responsibilities)
	keepWarm := strings.TrimSpace(rule.KeepWarm)
	if duration, err := effectiveSeatKeepWarm(rule); err == nil {
		if rule.ActivationPolicy == activationOnAssignment {
			keepWarm = duration.String()
		} else {
			keepWarm = ""
		}
	}
	parts := []string{
		rule.Name, rule.Label, rule.Nickname, rule.DepartmentLabel, rule.Workspace,
		rule.Department, rule.WorkstationPath, rule.Kind, rule.PermissionMode,
		rule.ReportsTo, strconv.FormatBool(rule.Disabled),
		strconv.FormatBool(rule.CanCreate), strconv.FormatBool(rule.CanIssue),
		strconv.FormatBool(rule.CanAccept), strconv.FormatBool(rule.CanClose),
		strconv.FormatBool(rule.CanManageStaff), strconv.FormatBool(rule.CanReceiveOrder),
		rule.RoleCardID, strconv.Itoa(rule.RoleCardVersion), rule.RoleCardDigest,
		rule.ManualPath, rule.ActivationPolicy, strconv.Itoa(rule.MaxWIP), strconv.Itoa(rule.SeatVersion),
	}
	if rule.ActivationPolicy == activationOnAssignment {
		parts = append(parts, "keep_warm="+keepWarm)
	}
	parts = append(parts, responsibilities...)
	parts = append(parts, rule.AgentArgs...)
	return requestDigest("employee-seat-v1", parts...)
}

func validActivationPolicy(value string) bool {
	return value == activationAlways || value == activationOnAssignment || value == activationManual
}

func effectiveSeatKeepWarm(rule AgentRule) (time.Duration, error) {
	raw := strings.TrimSpace(rule.KeepWarm)
	if rule.ActivationPolicy != activationOnAssignment {
		if raw != "" {
			return 0, fmt.Errorf("activation_policy=%s 不得设置 keep_warm；永久常驻请使用 always", rule.ActivationPolicy)
		}
		return 0, nil
	}
	if raw == "" {
		return defaultOnAssignmentKeepWarm, nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration < 0 || duration > maximumOnAssignmentKeepWarm {
		return 0, fmt.Errorf("keep_warm 必须是 0s..%s 的有界 duration", maximumOnAssignmentKeepWarm)
	}
	return duration, nil
}

func cleanRegistryRelativePath(value, field string) (string, error) {
	if value == "" || filepath.IsAbs(value) {
		return "", fmt.Errorf("%s 必须是 workspace 内非空相对路径", field)
	}
	clean := filepath.Clean(value)
	if clean != value || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s 必须是规范化、不得越出 workspace 的相对路径：%s", field, value)
	}
	return clean, nil
}

func pathUnderDepartment(value, department, field string) error {
	clean, err := cleanRegistryRelativePath(value, field)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(department, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s 必须位于所属部门 %s 内：%s", field, department, value)
	}
	return nil
}

// validatePersonalWorkstationPath defines the only supported employee
// workstation layout. Keeping every workstation at the same fixed depth makes
// a seat's inherited instruction boundary explicit and rules out department
// roots plus ancestor/descendant sharing by construction.
func validatePersonalWorkstationPath(department, value string, roleVersion int) error {
	clean, err := cleanRegistryRelativePath(value, "workstation_path")
	if err != nil {
		return err
	}
	parts := strings.Split(filepath.ToSlash(clean), "/")
	wantVersion := "v" + strconv.Itoa(roleVersion)
	if roleVersion < 1 || len(parts) != 4 || parts[0] != department || parts[1] != "staff" ||
		!agentNamePattern.MatchString(parts[2]) || parts[3] != wantVersion {
		return fmt.Errorf("workstation_path 必须精确为 <department>/staff/<seat>/v<role-version>，当前=%s role_version=%d", value, roleVersion)
	}
	return nil
}

func validatePersonalManualPath(department, value string, roleVersion int) error {
	clean, err := cleanRegistryRelativePath(value, "manual_path")
	if err != nil {
		return err
	}
	if filepath.Base(clean) != "AGENTS.md" {
		return fmt.Errorf("manual_path 必须正好是个人工位下的 AGENTS.md：%s", value)
	}
	workstation := filepath.Dir(clean)
	if err := validatePersonalWorkstationPath(department, workstation, roleVersion); err != nil {
		return err
	}
	if clean != filepath.Join(workstation, "AGENTS.md") {
		return fmt.Errorf("manual_path 必须正好等于 workstation_path/AGENTS.md：%s", value)
	}
	return nil
}

func (c Config) roleCard(id string, version int) (RoleCard, bool) {
	for _, card := range c.RoleCards {
		if card.ID == id && card.Version == version {
			return card, true
		}
	}
	return RoleCard{}, false
}

func (c Config) roleCardForAgent(rule AgentRule) (RoleCard, error) {
	card, ok := c.roleCard(rule.RoleCardID, rule.RoleCardVersion)
	if !ok {
		return RoleCard{}, fmt.Errorf("员工 %s 绑定的 role card 不存在：%s", rule.Name, roleCardKey(rule.RoleCardID, rule.RoleCardVersion))
	}
	if card.Digest != rule.RoleCardDigest || card.ManualPath != rule.ManualPath || card.Department != rule.Department {
		return RoleCard{}, fmt.Errorf("员工 %s 的 role card 绑定与 registry card 不一致", rule.Name)
	}
	return card, nil
}

func validateRoleCardRegistry(cfg Config) error {
	if cfg.Version != registrySchemaVersion {
		return fmt.Errorf("不支持的 registry version=%d", cfg.Version)
	}

	seen := map[string]bool{}
	cardManualOwners := map[string]string{}
	for _, card := range cfg.RoleCards {
		key := roleCardKey(card.ID, card.Version)
		if !roleCardIDPattern.MatchString(card.ID) || card.Version < 1 {
			return fmt.Errorf("role card key 非法：%s", key)
		}
		if seen[key] {
			return fmt.Errorf("role card 重复：%s", key)
		}
		seen[key] = true
		if card.Label == "" || !safeDepartment(card.Department) {
			return fmt.Errorf("role card %s 缺少 label 或 department 非法", key)
		}
		if len(card.Capabilities) == 0 {
			return fmt.Errorf("role card %s 至少需要一个 capability", key)
		}
		canonicalCapabilities, err := canonicalStringSet(card.Capabilities)
		if err != nil {
			return fmt.Errorf("role card %s：%w", key, err)
		}
		for index := range canonicalCapabilities {
			if canonicalCapabilities[index] != card.Capabilities[index] {
				return fmt.Errorf("role card %s capabilities 必须去重并按字典序排列", key)
			}
		}
		if err := validatePersonalManualPath(card.Department, card.ManualPath, card.Version); err != nil {
			return fmt.Errorf("role card %s：%w", key, err)
		}
		manualPath := filepath.Clean(card.ManualPath)
		if owner := cardManualOwners[manualPath]; owner != "" {
			return fmt.Errorf("role card manual 必须独立：%s 与 %s 共用 %s", owner, key, card.ManualPath)
		}
		cardManualOwners[manualPath] = key
		if err := validateDigest("manual_digest", card.ManualDigest); err != nil {
			return fmt.Errorf("role card %s：%w", key, err)
		}
		if card.Digest != roleCardDigest(card) {
			return fmt.Errorf("role card %s digest 与冻结字段不匹配", key)
		}
		if card.Status != roleCardApproved && card.Status != roleCardRetired {
			return fmt.Errorf("role card %s status 必须是 approved|retired", key)
		}
		if card.CreatedAt != "" {
			created, err := time.Parse(time.RFC3339, card.CreatedAt)
			if err != nil || created.UTC().Format(time.RFC3339) != card.CreatedAt {
				return fmt.Errorf("role card %s created_at 必须是规范化 UTC RFC3339", key)
			}
		}
	}
	if len(cfg.RoleCards) == 0 {
		return fmt.Errorf("registry v%d 至少需要一张 role card", registrySchemaVersion)
	}

	workstations := map[string]string{}
	manuals := map[string]string{}
	for _, rule := range cfg.Agents {
		card, ok := cfg.roleCard(rule.RoleCardID, rule.RoleCardVersion)
		if !ok {
			return fmt.Errorf("员工 %s 缺少 role card %s", rule.Name, roleCardKey(rule.RoleCardID, rule.RoleCardVersion))
		}
		if card.Status != roleCardApproved && !rule.Disabled {
			return fmt.Errorf("在职员工 %s 不得绑定 retired role card %s", rule.Name, roleCardKey(card.ID, card.Version))
		}
		if card.Department != rule.Department || card.ManualPath != rule.ManualPath || card.Digest != rule.RoleCardDigest {
			return fmt.Errorf("员工 %s 的 role card department/manual/digest 绑定不匹配", rule.Name)
		}
		if rule.WorkstationPath == "" {
			return fmt.Errorf("员工 %s 缺少 workstation_path", rule.Name)
		}
		if err := validatePersonalWorkstationPath(rule.Department, rule.WorkstationPath, rule.RoleCardVersion); err != nil {
			return fmt.Errorf("员工 %s：%w", rule.Name, err)
		}
		if filepath.Clean(rule.ManualPath) != filepath.Join(filepath.Clean(rule.WorkstationPath), "AGENTS.md") {
			return fmt.Errorf("员工 %s manual_path 必须等于 workstation_path/AGENTS.md", rule.Name)
		}
		if owner := workstations[rule.WorkstationPath]; owner != "" {
			return fmt.Errorf("在职员工工位重复：%s 与 %s 共用 %s", owner, rule.Name, rule.WorkstationPath)
		}
		if owner := manuals[rule.ManualPath]; owner != "" {
			return fmt.Errorf("员工角色卡手册重复：%s 与 %s 共用 %s", owner, rule.Name, rule.ManualPath)
		}
		workstations[rule.WorkstationPath], manuals[rule.ManualPath] = rule.Name, rule.Name
		if !validActivationPolicy(rule.ActivationPolicy) {
			return fmt.Errorf("员工 %s activation_policy 必须是 always|on_assignment|manual", rule.Name)
		}
		if _, err := effectiveSeatKeepWarm(rule); err != nil {
			return fmt.Errorf("员工 %s：%w", rule.Name, err)
		}
		if rule.MaxWIP < 1 || rule.MaxWIP > 16 {
			return fmt.Errorf("员工 %s max_wip 必须在 1..16", rule.Name)
		}
		if rule.SeatVersion < 1 || rule.SeatDigest != employeeSeatDigest(rule) {
			return fmt.Errorf("员工 %s seat_version/digest 无效", rule.Name)
		}
	}
	return nil
}
