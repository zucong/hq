package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBoundStoreRejectsStaleConfigBeforeEveryLedgerRoot(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}

	// Leave a fully durable pre-append intent behind. If any public root reaches
	// recovery before checking the registry lease, the manifest will change.
	crashStore := NewStore(e.data)
	fired := false
	crashStore.Failpoint = func(name string) error {
		if name == "journal_parent_fsync" && !fired {
			fired = true
			return errors.New("simulated crash after durable intent")
		}
		return nil
	}
	penny := actorFor(cfg, "penny", "registry-lease:penny", e.office)
	if _, err := transactCreateCase(crashStore, cfg, penny, "REGISTRY-LEASE-PENDING-001", "pending", false); err == nil {
		t.Fatal("durable-intent failpoint did not interrupt transaction")
	}
	if !fired {
		t.Fatal("journal_parent_fsync failpoint was not reached")
	}

	store := NewStore(e.data)
	if err := store.bindConfigPath(e.config); err != nil {
		t.Fatal(err)
	}
	stale := cfg
	stale.OwnerPrincipal = "StaleOwner"
	before := snapshotTree(t, e.data)
	builderCalled := false

	tests := []struct {
		name string
		call func() error
	}{
		{name: "TransactBatch", call: func() error {
			_, err := store.TransactBatch(stale, stableCommandID("registry-lease-stale", "transact"), requestDigest("registry-lease-stale", "transact"), false, func(*ledgerState) ([]Event, error) {
				builderCalled = true
				return nil, errors.New("stale transaction builder must not run")
			})
			return err
		}},
		{name: "Rebuild", call: func() error {
			_, err := store.Rebuild(stale)
			return err
		}},
		{name: "Snapshot", call: func() error {
			_, err := store.Snapshot(stale)
			return err
		}},
		{name: "ReadAll", call: func() error {
			_, err := store.ReadAll(stale)
			return err
		}},
		{name: "ReportAssignment", call: func() error {
			_, _, err := store.ReportAssignment(stale, "REGISTRY-LEASE-PENDING-001", "penny")
			return err
		}},
		{name: "Delivery", call: func() error {
			_, _, err := store.Delivery(stale, "delivery-registry-lease")
			return err
		}},
		{name: "Deliveries", call: func() error {
			_, err := store.Deliveries(stale)
			return err
		}},
		{name: "LedgerStateReadOnly", call: func() error {
			_, err := store.LedgerStateReadOnly(stale)
			return err
		}},
		{name: "SnapshotReadOnly", call: func() error {
			_, err := store.SnapshotReadOnly(stale)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			if err == nil || exitCodeForError(err) != exitConflict || !strings.Contains(err.Error(), "config 已过期") {
				t.Fatalf("stale config was not rejected as conflict: %v", err)
			}
			if builderCalled {
				t.Fatal("stale config reached transaction builder")
			}
			if after := snapshotTree(t, e.data); !reflect.DeepEqual(after, before) {
				t.Fatalf("stale root recovered or wrote the ledger\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

func TestNewAppBindsConcreteStoreAndRejectsDifferentRegistry(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(e.data)
	paths := runtimePaths{Office: e.office, HQRoot: e.root, DataDir: e.data, ConfigPath: e.config, HerdrBin: e.herdr}
	deps := AppDependencies{Store: store, Identity: e.identity, Transport: e.transport}
	if _, err := newAppWithDependencies(paths, cfg, globalOptions{}, deps, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want, err := normalizeStoreConfigPath(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.boundConfigPath(); got != want {
		t.Fatalf("Store config binding=%q want=%q", got, want)
	}

	other := filepath.Join(e.office, "other-config.yaml")
	raw, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	paths.ConfigPath = other
	if _, err := newAppWithDependencies(paths, cfg, globalOptions{}, deps, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "拒绝改绑") {
		t.Fatalf("constructor accepted the same Store with a different registry: %v", err)
	}
	if got := store.boundConfigPath(); got != want {
		t.Fatalf("failed rebind changed Store config binding=%q want=%q", got, want)
	}
}

func TestNewAppRejectsConcreteStoreWithStaleConfig(t *testing.T) {
	e := setupTestEnv(t)
	stale, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	stale.OwnerPrincipal = "StaleOwner"
	store := NewStore(e.data)
	paths := runtimePaths{Office: e.office, HQRoot: e.root, DataDir: e.data, ConfigPath: e.config, HerdrBin: e.herdr}
	deps := AppDependencies{Store: store, Identity: e.identity, Transport: e.transport}
	if _, err := newAppWithDependencies(paths, stale, globalOptions{}, deps, io.Discard, io.Discard); err == nil || exitCodeForError(err) != exitConflict || !strings.Contains(err.Error(), "config 已过期") {
		t.Fatalf("constructor accepted a stale config for a concrete Store: %v", err)
	}
	if _, err := os.Stat(e.data); !os.IsNotExist(err) {
		t.Fatalf("stale constructor wrote Store data: %v", err)
	}
}

func TestRegistryLeaseCrossProcessHelper(t *testing.T) {
	if os.Getenv("HQ_REGISTRY_LEASE_HELPER") != "1" {
		return
	}
	configPath := os.Getenv("HQ_REGISTRY_LEASE_CONFIG")
	dataDir := os.Getenv("HQ_REGISTRY_LEASE_DATA")
	office := os.Getenv("HQ_REGISTRY_LEASE_OFFICE")
	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stdout, "helper-error:load:%v\n", err)
		os.Exit(2)
	}
	fmt.Fprintln(os.Stdout, "loaded-old-config")
	if !bufio.NewScanner(os.Stdin).Scan() {
		fmt.Fprintln(os.Stdout, "helper-error:missing-go")
		os.Exit(3)
	}
	store := NewStore(dataDir)
	if err := store.bindConfigPath(configPath); err != nil {
		fmt.Fprintf(os.Stdout, "helper-error:bind:%v\n", err)
		os.Exit(4)
	}
	penny := actorFor(cfg, "penny", "registry-lease-helper:penny", office)
	_, err = transactCreateCase(store, cfg, penny, "REGISTRY-LEASE-CROSS-PROCESS-001", "stale-writer", false)
	if err == nil {
		fmt.Fprintln(os.Stdout, "unexpected-success")
		os.Exit(5)
	}
	if exitCodeForError(err) != exitConflict || !strings.Contains(err.Error(), "config 已过期") {
		fmt.Fprintf(os.Stdout, "helper-error:wrong-error:%v\n", err)
		os.Exit(6)
	}
	fmt.Fprintf(os.Stdout, "stale-conflict:%v\n", err)
}

func TestRegistryLeaseOrdersConfigBeforeLedgerAcrossProcesses(t *testing.T) {
	e := setupTestEnv(t)
	store := NewStore(e.data)
	if err := store.bindConfigPath(e.config); err != nil {
		t.Fatal(err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestRegistryLeaseCrossProcessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"HQ_REGISTRY_LEASE_HELPER=1",
		"HQ_REGISTRY_LEASE_CONFIG="+e.config,
		"HQ_REGISTRY_LEASE_DATA="+e.data,
		"HQ_REGISTRY_LEASE_OFFICE="+e.office,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		_ = stdin.Close()
		if !waited && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	lines := make(chan string, 16)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	waitLine := func(prefix string, timeout time.Duration) string {
		t.Helper()
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("helper stdout closed before %q; stderr=%s", prefix, stderr.String())
				}
				if strings.HasPrefix(line, prefix) {
					return line
				}
				if strings.HasPrefix(line, "helper-error:") || line == "unexpected-success" {
					t.Fatalf("helper failed: %s; stderr=%s", line, stderr.String())
				}
			case <-timer.C:
				t.Fatalf("timed out waiting for helper line %q; stderr=%s", prefix, stderr.String())
			}
		}
	}
	waitLine("loaded-old-config", 5*time.Second)

	staffReady := make(chan struct{})
	releaseStaff := make(chan struct{})
	var releaseOnce sync.Once
	releaseStaffLock := func() { releaseOnce.Do(func() { close(releaseStaff) }) }
	defer releaseStaffLock()
	staffDone := make(chan error, 1)
	var readyOnce sync.Once
	go func() {
		_, mutateErr := mutateConfigWithOptions(e.config, configWriteOptions{
			candidateGuard: func(candidate *Config) (func(), error) {
				return store.LockAndReplayCandidate(*candidate)
			},
			failpoint: func(name string) error {
				if name == "config_temp_fsync" {
					readyOnce.Do(func() { close(staffReady) })
					<-releaseStaff
				}
				return nil
			},
		}, func(candidate *Config) error {
			for index := range candidate.Agents {
				if candidate.Agents[index].Name == "penny" {
					candidate.Agents[index].CanCreate = false
					finalizeTestSeatMutation(&candidate.Agents[index])
					return nil
				}
			}
			return errors.New("penny missing from candidate registry")
		})
		staffDone <- mutateErr
	}()
	select {
	case <-staffReady:
	case err := <-staffDone:
		t.Fatalf("staff mutation ended before lock-order checkpoint: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("staff mutation did not reach config_temp_fsync")
	}
	if _, err := fmt.Fprintln(stdin, "go"); err != nil {
		t.Fatal(err)
	}

	// While staff owns config EX and the ledger guard, the stale process must
	// wait on config SH. It cannot slip directly to the ledger lock.
	select {
	case line := <-lines:
		t.Fatalf("stale helper passed registry lease before staff release: %q", line)
	case <-time.After(200 * time.Millisecond):
	}
	releaseStaffLock()
	select {
	case err := <-staffDone:
		if err != nil {
			t.Fatalf("staff mutation failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("staff mutation did not release its locks")
	}
	waitLine("stale-conflict:", 5*time.Second)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper process failed: %v; stderr=%s", err, stderr.String())
	}
	waited = true
	<-scanDone

	current, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	penny, ok := configRuleIncludingDisabled(current, "penny")
	if !ok || penny.CanCreate {
		t.Fatalf("staff replacement was not installed: %+v", penny)
	}
	for _, path := range []string{filepath.Join(e.data, "events"), filepath.Join(e.data, "txn"), filepath.Join(e.data, "state.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale process wrote ledger artifact %s: %v", path, err)
		}
	}
}
