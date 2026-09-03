package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func roleScopeDigest(action string, card RoleCard) string {
	card.ApprovalRef, card.CreatedAt = "", ""
	request := struct {
		Action string   `json:"action"`
		Target string   `json:"target"`
		Card   RoleCard `json:"role_card"`
	}{Action: action, Target: roleCardKey(card.ID, card.Version), Card: card}
	return canonicalJSONDigest(request)
}

func (a *App) cmdRole(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：hq role list|show|add|retire")
	}
	switch args[0] {
	case "list":
		return a.cmdRoleList(args[1:])
	case "show":
		return a.cmdRoleShow(args[1:])
	case "add":
		return a.cmdRoleAdd(args[1:])
	case "retire":
		return a.cmdRoleRetire(args[1:])
	default:
		return fmt.Errorf("未知 role 子命令 %q", args[0])
	}
}

func sortedRoleCards(cfg Config, includeRetired bool) []RoleCard {
	cards := make([]RoleCard, 0, len(cfg.RoleCards))
	for _, card := range cfg.RoleCards {
		if includeRetired || card.Status != roleCardRetired {
			cards = append(cards, card)
		}
	}
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].ID != cards[j].ID {
			return cards[i].ID < cards[j].ID
		}
		return cards[i].Version < cards[j].Version
	})
	return cards
}

func (a *App) cmdRoleList(args []string) error {
	fs := newLeafParser("role list")
	fs.SetOutput(a.Err)
	all := fs.Bool("all", false, "包含 retired role card")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cards := sortedRoleCards(a.Config, *all)
	if a.JSON {
		return a.output(cards, "")
	}
	fmt.Fprintf(a.Out, "%-28s %-12s %-10s %-36s %s\n", "ROLE", "DEPARTMENT", "STATUS", "CAPABILITIES", "MANUAL")
	for _, card := range cards {
		fmt.Fprintf(a.Out, "%-28s %-12s %-10s %-36s %s\n", roleCardKey(card.ID, card.Version), card.Department, card.Status, strings.Join(card.Capabilities, ","), card.ManualPath)
	}
	return nil
}

func (a *App) cmdRoleShow(args []string) error {
	fs := newLeafParser("role show")
	fs.SetOutput(a.Err)
	roleRef := fs.String("role", "", "role card id@version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, version, err := parseRoleCardRef(*roleRef)
	if err != nil {
		return err
	}
	card, ok := a.Config.roleCard(id, version)
	if !ok {
		return fmt.Errorf("role card 未登记：%s", roleCardKey(id, version))
	}
	return a.output(card, fmt.Sprintf("role=%s status=%s department=%s capabilities=%s manual=%s digest=%s", roleCardKey(id, version), card.Status, card.Department, strings.Join(card.Capabilities, ","), card.ManualPath, card.Digest))
}

func readRoleManual(hqRoot, department, manualRef string, roleVersion int) (string, string, error) {
	cleanManual, err := cleanRegistryRelativePath(strings.TrimSpace(manualRef), "manual")
	if err != nil {
		return "", "", err
	}
	if err := validatePersonalManualPath(department, cleanManual, roleVersion); err != nil {
		return "", "", err
	}
	want := filepath.Clean(filepath.Join(hqRoot, cleanManual))
	raw, canonical, err := readRoleManualFile(hqRoot, want, "role card AGENTS.md", nil)
	if err != nil {
		return "", "", err
	}
	if canonical != want {
		return "", "", fmt.Errorf("role card manual 必须使用 canonical 路径：%s", want)
	}
	return cleanManual, roleCardFileDigest(raw), nil
}

func (a *App) cmdRoleAdd(args []string) error {
	fs := newLeafParser("role add")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "稳定 role card id")
	versionText := fs.String("version", "", "不可变 role card version")
	label := fs.String("label", "", "角色显示名称")
	department := fs.String("department", "", "所属部门目录")
	capabilities := fs.StringSlice("capability", nil, "行为能力标签；可重复")
	manual := fs.String("manual", "", "独立 AGENTS.md 的 workspace 相对路径")
	approval := fs.String("approval", "", "公司所有者生效 decisions 文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.staffMutationActor()
	if err != nil {
		return err
	}
	cleanID := strings.TrimSpace(*id)
	if !roleCardIDPattern.MatchString(cleanID) {
		return fmt.Errorf("role_card_id 非法：%s", cleanID)
	}
	version, err := strconv.Atoi(strings.TrimSpace(*versionText))
	if err != nil || version < 1 {
		return fmt.Errorf("--version 必须是正整数")
	}
	cleanLabel, err := validateShortText("label", *label, true)
	if err != nil {
		return err
	}
	cleanDepartment := strings.TrimSpace(*department)
	if !safeDepartment(cleanDepartment) {
		return fmt.Errorf("--department 必须是总部直属目录名")
	}
	canonicalCapabilities, err := canonicalStringSet(*capabilities)
	if err != nil {
		return err
	}
	if len(canonicalCapabilities) == 0 {
		return fmt.Errorf("至少需要一个 --capability；capability 只描述角色行为，不授予 seat 权限")
	}
	cleanManual, manualDigest, err := readRoleManual(a.HQRoot, cleanDepartment, *manual, version)
	if err != nil {
		return err
	}
	card := RoleCard{
		ID: cleanID, Version: version, Label: cleanLabel, Department: cleanDepartment,
		Capabilities: canonicalCapabilities, ManualPath: cleanManual, ManualDigest: manualDigest,
		Status: roleCardApproved,
	}
	card.Digest = roleCardDigest(card)
	var added RoleCard
	var cleanApproval string
	cfg, err := mutateConfigWithOptions(a.ConfigPath, a.registryConfigWriteOptions("role card"), func(cfg *Config) error {
		liveActor, ok := cfg.exactRule(actor.Name)
		if !ok || liveActor.Disabled || !liveActor.CanManageStaff {
			return fmt.Errorf("当前 agent %s 已失去实时 can_manage_staff 权限", actor.Name)
		}
		maxVersion := 0
		for _, existing := range cfg.RoleCards {
			if existing.ID == card.ID && existing.Version > maxVersion {
				maxVersion = existing.Version
			}
			if existing.ID == card.ID && existing.Version == card.Version {
				return fmt.Errorf("role card version 已存在且不可变：%s", roleCardKey(card.ID, card.Version))
			}
			if filepath.Clean(existing.ManualPath) == filepath.Clean(card.ManualPath) {
				return fmt.Errorf("role card manual 必须独立；%s 已由 %s 使用", card.ManualPath, roleCardKey(existing.ID, existing.Version))
			}
		}
		if card.Version != maxVersion+1 {
			return fmt.Errorf("role card %s 新版本必须是 %d，实际=%d", card.ID, maxVersion+1, card.Version)
		}
		_, currentManualDigest, err := readRoleManual(a.HQRoot, card.Department, card.ManualPath, card.Version)
		if err != nil {
			return err
		}
		if currentManualDigest != card.ManualDigest {
			return conflictf("role card manual 在 approval 与 config 锁定之间已改变：%s", card.ManualPath)
		}
		cleanApproval, err = validateApprovalScope(*approval, a.Office, cfg.ownerPrincipal(), ApprovalScope{
			Action: "role:add", Target: roleCardKey(card.ID, card.Version), RequestDigest: roleScopeDigest("role:add", card),
		})
		if err != nil {
			return fmt.Errorf("approval：%w", err)
		}
		added = card
		added.ApprovalRef = cleanApproval
		added.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		cfg.RoleCards = append(cfg.RoleCards, added)
		return nil
	})
	if err != nil {
		return err
	}
	if a.DryRun {
		return a.output(added, fmt.Sprintf("DRY-RUN：将新增不可变 role card %s", roleCardKey(added.ID, added.Version)))
	}
	a.Config = cfg
	return a.output(added, fmt.Sprintf("已新增 role card %s；manual_digest=%s；card_digest=%s；批准=%s", roleCardKey(added.ID, added.Version), added.ManualDigest, added.Digest, cleanApproval))
}

func (a *App) cmdRoleRetire(args []string) error {
	fs := newLeafParser("role retire")
	fs.SetOutput(a.Err)
	roleRef := fs.String("role", "", "role card id@version")
	approval := fs.String("approval", "", "公司所有者生效 decisions 文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.staffMutationActor()
	if err != nil {
		return err
	}
	id, version, err := parseRoleCardRef(*roleRef)
	if err != nil {
		return err
	}
	key := roleCardKey(id, version)
	var retired RoleCard
	var cleanApproval string
	cfg, err := mutateConfigWithOptions(a.ConfigPath, a.registryConfigWriteOptions("role card"), func(cfg *Config) error {
		liveActor, ok := cfg.exactRule(actor.Name)
		if !ok || liveActor.Disabled || !liveActor.CanManageStaff {
			return fmt.Errorf("当前 agent %s 已失去实时 can_manage_staff 权限", actor.Name)
		}
		index := -1
		for i := range cfg.RoleCards {
			if cfg.RoleCards[i].ID == id && cfg.RoleCards[i].Version == version {
				index = i
				break
			}
		}
		if index < 0 {
			return fmt.Errorf("role card 未登记：%s", key)
		}
		if cfg.RoleCards[index].Status == roleCardRetired {
			return fmt.Errorf("role card 已 retired：%s", key)
		}
		for _, rule := range cfg.Agents {
			if rule.RoleCardID == id && rule.RoleCardVersion == version {
				return conflictf("role card %s 仍被员工 %s 绑定；先将该 seat 更新到另一张 approved card", key, rule.Name)
			}
		}
		retired = cfg.RoleCards[index]
		retired.Status = roleCardRetired
		var err error
		cleanApproval, err = validateApprovalScope(*approval, a.Office, cfg.ownerPrincipal(), ApprovalScope{
			Action: "role:retire", Target: key, RequestDigest: roleScopeDigest("role:retire", retired),
		})
		if err != nil {
			return fmt.Errorf("approval：%w", err)
		}
		retired.ApprovalRef = cleanApproval
		cfg.RoleCards[index] = retired
		return nil
	})
	if err != nil {
		return err
	}
	if a.DryRun {
		return a.output(retired, fmt.Sprintf("DRY-RUN：将 retire role card %s", key))
	}
	a.Config = cfg
	return a.output(retired, fmt.Sprintf("已 retire role card %s；冻结版本与历史仍保留；批准=%s", key, cleanApproval))
}
