package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	configWatchDebounce    = 250 * time.Millisecond
	configWatchPoll        = time.Minute
	configWatchRetry       = 5 * time.Second
	configWatchSeatTimeout = 30 * time.Second
)

// The detector never performs runtime I/O. A one-element wake queue coalesces
// saves while the independent worker is reconciling a seat. The worker always
// reloads disk, never an event's potentially obsolete config snapshot.
func (a *App) runConfigWatcher(ctx context.Context) {
	// Install before the initial scan so a startup save cannot fall between
	// the scan and watcher registration.
	watcher, err := fsnotify.NewWatcher()
	if err == nil {
		err = watcher.Add(filepath.Dir(a.ConfigPath))
	}
	wake := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.runConfigProfileWorker(ctx, wake, configWatchPoll, configWatchRetry)
	}()
	defer func() { <-done }()
	if err != nil {
		if watcher != nil {
			_ = watcher.Close()
		}
		fmt.Fprintf(a.Err, "[HQ config watcher] 文件通知不可用：%v；已降级为每分钟核对 %s；修复目录访问后重启 gateway 可恢复实时通知。\n", err, a.ConfigPath)
		<-ctx.Done()
		return
	}
	defer watcher.Close()
	a.watchConfigEvents(ctx, watcher.Events, watcher.Errors, wake, configWatchDebounce)
}

func (a *App) watchConfigEvents(ctx context.Context, events <-chan fsnotify.Event, failures <-chan error, wake chan<- struct{}, debounce time.Duration) {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	var timerC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if filepath.Clean(event.Name) != filepath.Clean(a.ConfigPath) || event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			timer.Reset(debounce)
			timerC = timer.C
		case err, ok := <-failures:
			if !ok {
				failures = nil
				continue
			}
			fmt.Fprintf(a.Err, "[HQ config watcher] 文件通知失败：%v；每分钟核对仍有效，运行 hq doctor --json 检查配置。\n", err)
			select {
			case wake <- struct{}{}:
			default:
			}
		case <-timerC:
			timerC = nil
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}
}

type configProfileController struct {
	desired    map[string]runtimeProfile
	pending    map[string]bool
	lastError  string
	seatErrors map[string]string
}

func effectiveConfigProfiles(cfg Config) map[string]runtimeProfile {
	profiles := make(map[string]runtimeProfile)
	for _, rule := range cfg.Agents {
		if rule.Disabled {
			continue
		}
		if profile, ok := runtimeProfileForEmployee(cfg, rule.Kind, rule.Name); ok {
			profiles[rule.Name] = profile
		}
	}
	return profiles
}

func (c *configProfileController) accept(cfg Config) {
	next := effectiveConfigProfiles(cfg)
	if c.pending == nil {
		c.pending = make(map[string]bool)
	}
	if c.seatErrors == nil {
		c.seatErrors = make(map[string]string)
	}
	for name, profile := range next {
		if previous, ok := c.desired[name]; !ok || previous != profile {
			c.pending[name] = true
		}
	}
	for name := range c.pending {
		if _, exists := next[name]; !exists {
			delete(c.pending, name)
			delete(c.seatErrors, name)
		}
	}
	c.desired = next
}

func (a *App) runConfigProfileWorker(ctx context.Context, wake <-chan struct{}, poll, retry time.Duration) {
	pollTicker, retryTicker := time.NewTicker(poll), time.NewTicker(retry)
	defer pollTicker.Stop()
	defer retryTicker.Stop()
	c := &configProfileController{}
	a.reconcileConfigProfiles(ctx, c)
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-pollTicker.C:
		case <-retryTicker.C:
			if len(c.pending) == 0 {
				continue
			}
		}
		a.reconcileConfigProfiles(ctx, c)
	}
}

func (a *App) reconcileConfigProfiles(ctx context.Context, controller *configProfileController) {
	if ctx.Err() != nil {
		return
	}
	cfg, err := loadConfig(a.ConfigPath)
	if err != nil {
		message := fmt.Sprintf("配置无效，未应用此次变更、未触发 runtime 切换：%v；请修正 %s 后保存，运行 hq doctor --json 核验。", err, a.ConfigPath)
		if controller.lastError != message {
			fmt.Fprintf(a.Err, "[HQ config watcher] %s\n", message)
		}
		controller.lastError = message
		return
	}
	if controller.lastError != "" {
		fmt.Fprintln(a.Err, "[HQ config watcher] 配置校验已恢复，重新核对员工期望配置。")
	}
	controller.lastError = ""
	controller.accept(cfg)
	names := make([]string, 0, len(controller.pending))
	for name := range controller.pending {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		seatCtx, cancel := context.WithTimeout(ctx, configWatchSeatTimeout)
		settled := false
		// Lease scope is ONE seat, not a company-wide batch. The existing
		// queued-writer protocol still allows staff update between seats.
		err := a.runRuntimeMaintenanceStep(seatCtx, func(child *App) error {
			rule, ok := child.Config.exactRule(name)
			if !ok {
				settled = true
				return nil
			}
			profile, ok := runtimeProfileForEmployee(child.Config, rule.Kind, name)
			if !ok {
				settled = true
				return nil
			}
			// A newer save superseded this batch. Do not act on an obsolete
			// generation; the queued wake or retry will load the latest one.
			if profile != controller.desired[name] {
				return nil
			}
			reader, ok := child.Herdr.(HerdrAgentReader)
			if !ok {
				return fmt.Errorf("Herdr 缺少 terminal read；无法核验模型，未切换 runtime")
			}
			snapshot, err := child.herdrSnapshot(seatCtx)
			if err != nil {
				return err
			}
			workspaceID := ""
			for _, w := range snapshot.Workspaces {
				if w.Label == child.Config.WorkspaceLabel {
					if workspaceID != "" {
						return fmt.Errorf("workspace label 不唯一：%s", w.Label)
					}
					workspaceID = w.ID
				}
			}
			if workspaceID == "" {
				return fmt.Errorf("workspace 不在线；请运行 hq up")
			}
			if err := child.recoverRuntimeProfileSeat(seatCtx, workspaceID, rule, reader, false, false); err != nil {
				return err
			}
			status := child.employeeRuntimeProfileStatus(seatCtx, rule)
			switch status.State {
			case "applied", "next_activation", "report_only", "disabled", "unconfigured", "different_kind":
				settled = true
			}
			return nil
		})
		cancel()
		if err != nil {
			message := err.Error()
			if controller.seatErrors[name] != message {
				fmt.Fprintf(a.Err, "[HQ config watcher] seat=%s 应用未完成：%s；运行 hq staff get --name %s --live --json 和 hq patrol --json 核验，禁止裸 Herdr 重启。\n", name, message, name)
			}
			controller.seatErrors[name] = message
		} else {
			delete(controller.seatErrors, name)
			if settled {
				delete(controller.pending, name)
			}
		}
	}
}
