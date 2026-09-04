package main

import (
	"context"
	"fmt"
)

// refreshMaintenanceBinding follows the same authorized maintenance seat
// across runtime incarnations. Gateway startup freezes the seat identity, not a
// forever-valid pane ID; a manager/profile restart must not silently disable
// internal queue watchdogs until the whole gateway is restarted.
func (a *App) refreshMaintenanceBinding(ctx context.Context) error {
	if a.MaintenanceActor == "" {
		return fmt.Errorf("gateway 缺少 maintenance actor；从实时在岗的 can_manage_staff 角色运行 `hq up` 重建 gateway")
	}
	rule, ok := a.Config.exactRule(a.MaintenanceActor)
	if !ok || rule.Disabled || !rule.CanManageStaff {
		return fmt.Errorf("maintenance actor %s 已停用、未登记或失去 can_manage_staff；从新的实时授权角色运行 `hq up` 重建 gateway", a.MaintenanceActor)
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("读取 maintenance actor live binding：%w", err)
	}
	binding, err := ResolveLiveBinding(snapshot, a.Config, a.HQRoot, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
	if err != nil {
		return fmt.Errorf("maintenance actor %s 当前没有唯一可交互 live binding：%w；HQ 暂停内部 nudge，不使用过期 pane", rule.Name, err)
	}
	if a.GatewayWorkspaceID != "" && binding.WorkspaceID != a.GatewayWorkspaceID {
		return fmt.Errorf("maintenance actor %s 位于 workspace=%s，gateway 绑定 workspace=%s；HQ 暂停内部 nudge", rule.Name, binding.WorkspaceID, a.GatewayWorkspaceID)
	}
	a.MaintenancePane = binding.PaneID
	return nil
}
