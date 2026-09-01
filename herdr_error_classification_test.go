package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFakeHerdrExit(t *testing.T, stdout, stderr string, exit int) string {
	t.Helper()
	root := canonicalTestTempDir(t)
	path := filepath.Join(root, "herdr")
	script := "#!/bin/sh\n"
	if stdout != "" {
		script += "printf '%s' '" + stdout + "'\n"
	}
	if stderr != "" {
		script += "printf '%s' '" + stderr + "' >&2\n"
	}
	script += "exit " + string(rune('0'+exit)) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHerdrMutationClassifiesStructuredPreconditionAsNoEffect(t *testing.T) {
	for _, code := range []string{"agent_blocked", "agent_not_ready", "agent_not_running", "agent_not_found", "pane_not_found"} {
		t.Run(code, func(t *testing.T) {
			bin := writeFakeHerdrExit(t, "", `{"id":"cli:request","error":{"code":"`+code+`","message":"rejected before input"}}`, 1)
			control, err := newExecHerdrControl(bin)
			if err != nil {
				t.Fatal(err)
			}
			result := control.Prompt(context.Background(), "worker", "do work")
			if result.Outcome != herdrDefinitelyNotRun || result.ErrorCode != code {
				t.Fatalf("result=%+v", result)
			}
			var apiErr *HerdrAPIError
			if !errors.As(result.Err, &apiErr) || apiErr.Code != code {
				t.Fatalf("structured error lost: %T %v", result.Err, result.Err)
			}
			attempt := (herdrDeliveryTransport{Control: control}).Deliver("worker", "do work")
			if attempt.Outcome != transportDefinitelyNotSent {
				t.Fatalf("delivery outcome=%s err=%v", attempt.Outcome, attempt.Err)
			}
		})
	}
}

func TestHerdrMutationKeepsUnknownOrMalformedFailureAmbiguous(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stderr string
		code   string
	}{
		{name: "unknown structured code", stderr: `{"error":{"code":"agent_prompt_failed","message":"input may be partial"}}`, code: "agent_prompt_failed"},
		{name: "malformed text", stderr: "connection reset after request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := writeFakeHerdrExit(t, "", tc.stderr, 1)
			control, err := newExecHerdrControl(bin)
			if err != nil {
				t.Fatal(err)
			}
			result := control.Prompt(context.Background(), "worker", "do work")
			if result.Outcome != herdrAmbiguous || result.ErrorCode != tc.code {
				t.Fatalf("result=%+v", result)
			}
			attempt := (herdrDeliveryTransport{Control: control}).Deliver("worker", "do work")
			if attempt.Outcome != transportAmbiguous {
				t.Fatalf("delivery outcome=%s err=%v", attempt.Outcome, attempt.Err)
			}
		})
	}
}

func TestStartAgentClassifiesStructuredPaneBusyAsDefinitelyNotRun(t *testing.T) {
	bin := writeFakeHerdrExit(t, "", `{"id":"cli:request","error":{"code":"agent_pane_busy","message":"shell not ready"}}`, 1)
	control, err := newExecHerdrControl(bin)
	if err != nil {
		t.Fatal(err)
	}
	result := control.StartAgent(context.Background(), "worker", "codex", "pane-1", nil)
	if result.Outcome != herdrDefinitelyNotRun || result.ErrorCode != "agent_pane_busy" {
		t.Fatalf("result=%+v", result)
	}
	var apiErr *HerdrAPIError
	if !errors.As(result.Err, &apiErr) || apiErr.Code != result.ErrorCode {
		t.Fatalf("structured busy error lost: %T %v", result.Err, result.Err)
	}
}

type busyOnceHerdrControl struct {
	*fakeHerdrControl
	starts int
}

func (c *busyOnceHerdrControl) StartAgent(ctx context.Context, name, kind, paneID string, native []string) HerdrMutationResult {
	c.starts++
	if c.starts == 1 {
		return HerdrMutationResult{
			Outcome: herdrDefinitelyNotRun, ErrorCode: "agent_pane_busy",
			Err: errors.New("localized precondition message without raw JSON"),
		}
	}
	return c.fakeHerdrControl.StartAgent(ctx, name, kind, paneID, native)
}

func TestUpRetriesPaneBusyByStructuredCode(t *testing.T) {
	e := setupTestEnv(t)
	app := e.app(t)
	control := &busyOnceHerdrControl{fakeHerdrControl: newFakeHerdrControl(e.root, app.Config.WorkspaceLabel)}
	app.Herdr = control
	app.Sessions = &FileSessionStore{Root: filepath.Join(e.data, "sessions")}
	if err := app.runUp([]string{"--no-gateway", "eng-developer"}); err != nil {
		t.Fatalf("structured busy result was not retried: %v", err)
	}
	if control.starts != 2 {
		t.Fatalf("start calls=%d, want one busy attempt plus one successful retry", control.starts)
	}
}

func TestHerdrMutationDeadlineNeverTrustsLateErrorEnvelope(t *testing.T) {
	root := canonicalTestTempDir(t)
	path := filepath.Join(root, "herdr")
	script := "#!/bin/sh\nsleep 1\nprintf '%s' '{\"error\":{\"code\":\"agent_blocked\",\"message\":\"late\"}}' >&2\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	control, err := newExecHerdrControl(path)
	if err != nil {
		t.Fatal(err)
	}
	control.PromptTimeout = 10 * time.Millisecond
	result := control.Prompt(context.Background(), "worker", "do work")
	if result.Outcome != herdrAmbiguous || result.ErrorCode != "" {
		t.Fatalf("deadline result=%+v", result)
	}
}
