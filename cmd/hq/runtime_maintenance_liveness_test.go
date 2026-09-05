package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

type rejectRecoveryReplayStore struct{ *Store }

func (s *rejectRecoveryReplayStore) ReadAll(Config) ([]Event, error) {
	return nil, fmt.Errorf("unexpected full recovery replay")
}

func TestRuntimeMaintenanceHealthyProfilesSkipRecoveryReplay(t *testing.T) {
	_, _, rule, app, reader, _ := profileRepairFixture(t, "idle")
	app.Store = &rejectRecoveryReplayStore{app.Store.(*Store)}
	reader.terminal = []byte("gpt-5.6-sol medium · 100% left · ~/work\n› Ask Codex to do anything\n")
	if err := app.recoverRuntimeProfileDriftsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	policy := RuntimeFallbackPolicy{FromKind: "codex", ToKind: "grok"}
	if err := app.recoverRuntimeSeatFromSafeguard(context.Background(), "w-test", rule, policy, reader, false, false); err != nil {
		t.Fatal(err)
	}
	// Actual drift still needs authoritative recovery work; no fail-open.
	reader.terminal = []byte("gpt-5.6-luna low · 100% left · ~/work\n› Ask Codex to do anything\n")
	if err := app.recoverRuntimeProfileDriftsOnce(context.Background()); err == nil {
		t.Fatal("repair skipped authoritative replay")
	}
}

func TestRuntimeMaintenancePhaseReleasesConfigForWriter(t *testing.T) {
	e := setupTestEnv(t)
	app := e.app(t)
	app.ConfigAccess = &sync.RWMutex{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- app.runRuntimeMaintenanceStep(ctx, func(c *App) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if app.ConfigAccess.TryLock() {
		app.ConfigAccess.Unlock()
		t.Fatal("maintenance phase unprotected")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	unlock, err := app.lockGatewayConfigAccessContext(ctx, []string{"staff", "update"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = mutateConfig(e.config, func(cfg *Config) error { cfg.RuntimeProfiles = configuredCodexProfile(); return nil })
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.runRuntimeMaintenanceStep(ctx, func(c *App) error {
		if c.Config.RuntimeProfiles["codex"].Model != "gpt-5.6-sol" {
			return fmt.Errorf("next phase used stale registry")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeMaintenanceConfigWaitIsCancellable(t *testing.T) {
	app := &App{ConfigAccess: &sync.RWMutex{}}
	app.ConfigAccess.Lock()
	defer app.ConfigAccess.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := app.runRuntimeMaintenanceStep(ctx, func(*App) error { t.Fatal("canceled phase executed"); return nil }); err == nil {
		t.Fatal("canceled lock wait succeeded")
	}
}

func TestRuntimeMaintenanceQueuedWriterCannotBeOvertaken(t *testing.T) {
	app := &App{ConfigAccess: &sync.RWMutex{}}
	app.ConfigAccess.RLock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		unlock, err := app.lockGatewayConfigAccessContext(ctx, []string{"staff", "update"})
		if err == nil {
			unlock()
		}
		result <- err
	}()
	queued := false
	for ctx.Err() == nil {
		if !app.ConfigAccess.TryRLock() {
			queued = true
			break
		}
		app.ConfigAccess.RUnlock()
		time.Sleep(time.Millisecond)
	}
	app.ConfigAccess.RUnlock()
	if !queued {
		t.Fatal("registry writer did not queue; maintenance readers can starve it")
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeMaintenanceCanceledWriterReleasesLateAcquisition(t *testing.T) {
	app := &App{ConfigAccess: &sync.RWMutex{}}
	app.ConfigAccess.RLock()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := app.lockGatewayConfigAccessContext(ctx, []string{"staff", "update"}); result <- err }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !app.ConfigAccess.TryRLock() {
			break
		}
		app.ConfigAccess.RUnlock()
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-result; err == nil {
		t.Fatal("canceled writer acquired")
	}
	app.ConfigAccess.RUnlock()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	unlock, err := app.lockGatewayConfigAccessContext(ctx2, []string{"staff", "update"})
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
