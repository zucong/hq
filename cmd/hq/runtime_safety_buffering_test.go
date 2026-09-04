package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const codexSafetyBufferingFixture = `
Our systems are thinking a bit more about this request before responding.
Hang tight or retry with a faster model for a quicker response, though it may be less capable.
1. Retry with a faster model
› 2. Dismiss and keep waiting
3. Learn more
No action is required. Codex will keep waiting for the original response.
`

func TestCodexSafetyBufferingDetectionRequiresCompleteChooser(t *testing.T) {
	if !terminalShowsCodexSafetyBuffering([]byte(codexSafetyBufferingFixture)) {
		t.Fatal("complete safety-buffering chooser was not detected")
	}
	wrapped := strings.ReplaceAll(codexSafetyBufferingFixture, "Dismiss and keep waiting", "Dismiss and keep\nwaiting")
	if !terminalShowsCodexSafetyBuffering([]byte(wrapped)) {
		t.Fatal("terminal wrapping should not hide the chooser")
	}
	for _, incomplete := range []string{
		"Our systems are thinking a bit more about this request before responding.",
		"1. Retry with a faster model\n2. Dismiss and keep waiting\n",
		"This content can't be shown\n",
	} {
		if terminalShowsCodexSafetyBuffering([]byte(incomplete)) {
			t.Fatalf("incomplete/unrelated terminal was treated as chooser: %q", incomplete)
		}
	}
	if terminalReadyForHQPrompt("codex", []byte(codexSafetyBufferingFixture+"\n› Ask Codex to do anything\n")) {
		t.Fatal("HQ prompt delivery must wait until the chooser has been dismissed")
	}
	if _, err := observedCodexRuntimeProfile([]byte("gpt-5.6-luna low · old footer\n" + codexSafetyBufferingFixture)); !errors.Is(err, errCodexSafetyBufferingVisible) {
		t.Fatalf("chooser must obscure stale footer instead of reporting profile drift: %v", err)
	}
}

func TestGeneratedAgentHandbookExplainsNonDegradingWaitPolicy(t *testing.T) {
	handbook := string(companyAgentHandbook(initPlan{CompanyName: "Example", Owner: "OWNER", Workspace: "example-hq"}))
	for _, want := range []string{"Dismiss and keep waiting", "保留原 model/effort", "No action is required", "不要手工选择 faster model"} {
		if !strings.Contains(handbook, want) {
			t.Fatalf("generated Agent handbook omits safety-buffering policy %q", want)
		}
	}
}

func TestRuntimeProfilePatrolDoesNotMisreportSafetyBufferingAsDrift(t *testing.T) {
	e := setupTestEnv(t)
	app := e.app(t)
	app.Config.RuntimeProfiles = configuredCodexProfile()
	rule, _ := app.Config.exactRule("zantianyou")
	base := newFakeHerdrControl(e.root, app.Config.WorkspaceLabel)
	base.snapshot = healthySnapshot(e.root, rule, "working")
	control := &staticTerminalReaderControl{fakeHerdrControl: base, terminal: []byte(codexSafetyBufferingFixture)}

	report, err := (&PatrolService{Herdr: control}).Run(context.Background(), app.Config, e.root, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range report.Findings {
		if finding.Agent == rule.Name && (finding.SignalType == "runtime_profile_unverified" || finding.SignalType == "runtime_profile_mismatch") {
			t.Fatalf("transient chooser was misreported as profile drift: %+v", finding)
		}
	}
}

func TestRuntimeProfileWatcherWaitsWithoutMutatingSafetyBufferingTurn(t *testing.T) {
	_, _, _, app, reader, _ := profileRepairFixture(t, "working")
	reader.mu.Lock()
	reader.terminal = []byte(codexSafetyBufferingFixture)
	reader.mu.Unlock()

	if err := app.recoverRuntimeProfileDriftsOnce(context.Background()); err != nil {
		t.Fatalf("transient chooser should not be a profile failure: %v", err)
	}
	reader.fakeHerdrControl.mu.Lock()
	calls := strings.Join(reader.fakeHerdrControl.calls, "\n")
	reader.fakeHerdrControl.mu.Unlock()
	for _, forbidden := range []string{"tab close", "agent start", "prompt ", "send-keys"} {
		if strings.Contains(calls, forbidden) {
			t.Fatalf("safety-buffering wait policy mutated the active turn via %q:\n%s", forbidden, calls)
		}
	}
}
