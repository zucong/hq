package main

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func findTurnBundleAttempt(t *testing.T, events []Event, deliveryID string) (Event, int) {
	t.Helper()
	for index, event := range events {
		if event.Type == "delivery_attempted" && event.DeliveryID == deliveryID {
			return event, index
		}
	}
	t.Fatalf("delivery %s has no attempted event", deliveryID)
	return Event{}, -1
}

func deliveryViewsByID(t *testing.T, e testEnv) map[string]DeliveryView {
	t.Helper()
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	views, err := NewStore(e.data).Deliveries(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]DeliveryView, len(views))
	for _, view := range views {
		result[view.DeliveryID] = view
	}
	return result
}

type turnBundleBarrierTransport struct {
	mu           sync.Mutex
	prompts      []string
	firstEntered chan struct{}
	releaseFirst chan struct{}
	once         sync.Once
}

type turnBundleBlockingWriter struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	buffer  strings.Builder
}

func newTurnBundleBlockingWriter() *turnBundleBlockingWriter {
	return &turnBundleBlockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
}

func (writer *turnBundleBlockingWriter) Write(payload []byte) (int, error) {
	writer.once.Do(func() { close(writer.entered) })
	<-writer.release
	return writer.buffer.Write(payload)
}

func newTurnBundleBarrierTransport() *turnBundleBarrierTransport {
	return &turnBundleBarrierTransport{firstEntered: make(chan struct{}), releaseFirst: make(chan struct{})}
}

func (transport *turnBundleBarrierTransport) Deliver(_ string, prompt string) DeliveryAttempt {
	transport.mu.Lock()
	transport.prompts = append(transport.prompts, prompt)
	first := len(transport.prompts) == 1
	transport.mu.Unlock()
	if first {
		transport.once.Do(func() { close(transport.firstEntered) })
		<-transport.releaseFirst
	}
	return DeliveryAttempt{Outcome: transportSent}
}

func (transport *turnBundleBarrierTransport) snapshot() []string {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([]string(nil), transport.prompts...)
}

func enqueueTurnBundleContext(t *testing.T, e testEnv, count int) (*App, []Event) {
	t.Helper()
	sender := deliveryPolicyTestApp(t, e, "eng-developer", "turn-bundle:sender")
	sender.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeIdle, nil }
	for index := 0; index < count; index++ {
		body := "bundle-context-" + string(rune('a'+index))
		if _, err := runDeliveryPolicyTest(sender, "message", "--to", "zantianyou", "--kind", "info", "--text", body, "--delivery", "inject"); err != nil {
			t.Fatal(err)
		}
	}
	return sender, deliveryPolicyMessages(t, e)
}

func TestTurnBundlePersistsDeterministicBoundedManifest(t *testing.T) {
	e := setupTestEnv(t)
	setDeliveryPolicyWithBundle(t, e, deliveryModeAuto, 10, 2, 64*1024)
	sender, pending := enqueueTurnBundleContext(t, e, 3)

	if _, err := runDeliveryPolicyTest(sender, "message", "--to", "zantianyou", "--kind", "request", "--text", "wake-with-manifest", "--delivery", "wakeup"); err != nil {
		t.Fatal(err)
	}
	if len(e.transport.calls) != 1 {
		t.Fatalf("transport calls=%d want=1", len(e.transport.calls))
	}
	prompt := e.transport.calls[0].message
	if !strings.Contains(prompt, pending[0].Message) || !strings.Contains(prompt, pending[1].Message) || strings.Contains(prompt, pending[2].Message) {
		t.Fatalf("prompt is not the exact bounded FIFO prefix: %q", prompt)
	}

	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	messages := deliveryPolicyMessages(t, e)
	wakeOrigin := messages[len(messages)-1]
	attempt, attemptIndex := findTurnBundleAttempt(t, events, wakeOrigin.DeliveryID)
	if attempt.Version != eventSchemaVersion || attempt.TurnBundleVersion != turnBundleVersion {
		t.Fatalf("attempt schema/manifest version=%d/%d", attempt.Version, attempt.TurnBundleVersion)
	}
	if len(attempt.TurnBundleDeliveryIDs) != 2 || attempt.TurnBundleDeliveryIDs[0] != pending[0].DeliveryID ||
		attempt.TurnBundleDeliveryIDs[1] != pending[1].DeliveryID || attempt.TurnBundleOverflow != 1 {
		t.Fatalf("unexpected manifest selection: %+v", attempt)
	}
	if attempt.TurnBundlePayloadDigests[0] != pending[0].PayloadDigest || attempt.TurnBundlePayloadDigests[1] != pending[1].PayloadDigest {
		t.Fatalf("manifest payload digests do not bind selected origins: %+v", attempt.TurnBundlePayloadDigests)
	}
	if attempt.TurnPromptDigest != digestText(prompt) || attempt.TurnBundleDigest != turnBundleManifestDigest(attempt, wakeOrigin) {
		t.Fatalf("manifest digest does not bind final prompt/manifest")
	}
	if attempt.TurnBundleBasePayload == "" || len(attempt.TurnBundleEnvelopes) != 2 ||
		attempt.TurnBundleEnvelopes[0] == "" || attempt.TurnBundleEnvelopes[1] == "" {
		t.Fatalf("manifest did not persist replayable base/envelopes: %+v", attempt)
	}
	if attempt.TurnBundleBytes < 1 || attempt.TurnBundleBytes > cfg.effectiveDeliveryPolicy().MaxBundleBytes {
		t.Fatalf("manifest bytes=%d", attempt.TurnBundleBytes)
	}

	views := deliveryViewsByID(t, e)
	for _, origin := range pending[:2] {
		if view := views[origin.DeliveryID]; view.ContextState != deliveryContextClaimed {
			t.Fatalf("manifest-selected delivery was not claimed: %+v", view)
		}
	}
	if view := views[pending[2].DeliveryID]; view.ContextState != deliveryContextPending || view.Status != deliveryQueued {
		t.Fatalf("overflow delivery did not remain pending: %+v", view)
	}
	if view := views[wakeOrigin.DeliveryID]; view.TurnBundleDigest != attempt.TurnBundleDigest || view.TurnBundleOverflow != 1 {
		t.Fatalf("delivery view omitted persisted manifest: %+v", view)
	}

	// Re-signing the hash chain is not enough to hide a semantically inconsistent
	// manifest: strict replay derives the pending FIFO cardinality independently.
	tampered := append([]Event(nil), events...)
	tampered[attemptIndex].TurnBundleOverflow++
	tampered[attemptIndex].TurnBundleDigest = turnBundleManifestDigest(tampered[attemptIndex], wakeOrigin)
	rehashEventChain(t, tampered)
	if _, err := validateLedger(tampered, cfg); err == nil || !strings.Contains(err.Error(), "selected/overflow") {
		t.Fatalf("strict replay accepted re-signed inconsistent manifest: %v", err)
	}

	tamperedBytes := append([]Event(nil), events...)
	tamperedBytes[attemptIndex].TurnBundleItemBytes = append([]int(nil), tamperedBytes[attemptIndex].TurnBundleItemBytes...)
	tamperedBytes[attemptIndex].TurnBundleItemBytes[0]++
	tamperedBytes[attemptIndex].TurnBundleBytes++
	tamperedBytes[attemptIndex].TurnBundleDigest = turnBundleManifestDigest(tamperedBytes[attemptIndex], wakeOrigin)
	rehashEventChain(t, tamperedBytes)
	if _, err := validateLedger(tamperedBytes, cfg); err == nil || !strings.Contains(err.Error(), "真实 envelope bytes") {
		t.Fatalf("strict replay accepted re-signed item_bytes tamper: %v", err)
	}

	tamperedPrompt := append([]Event(nil), events...)
	tamperedPrompt[attemptIndex].TurnPromptDigest = digestText("forged prompt")
	tamperedPrompt[attemptIndex].TurnBundleDigest = turnBundleManifestDigest(tamperedPrompt[attemptIndex], wakeOrigin)
	rehashEventChain(t, tamperedPrompt)
	if _, err := validateLedger(tamperedPrompt, cfg); err == nil || !strings.Contains(err.Error(), "真实 base+envelopes") {
		t.Fatalf("strict replay accepted re-signed prompt digest tamper: %v", err)
	}

	parentTerminalIndex := -1
	for index, event := range events {
		if event.DeliveryID == wakeOrigin.DeliveryID && event.Type == "message_sent" {
			parentTerminalIndex = index
		}
	}
	if parentTerminalIndex != len(events)-1 {
		t.Fatalf("parent terminal index=%d want final=%d", parentTerminalIndex, len(events)-1)
	}
	partial := append([]Event(nil), events[:parentTerminalIndex]...)
	if _, err := validateLedger(partial, cfg); err == nil || !strings.Contains(err.Error(), "child 已开始收敛") {
		t.Fatalf("strict replay accepted partial convergence tail: %v", err)
	}

	forgedFailure := append([]Event(nil), events...)
	forgedFailure[parentTerminalIndex].Type = "delivery_failed_pre_send"
	forgedFailure[parentTerminalIndex].Delivery = deliveryFailedPreSend
	forgedFailure[parentTerminalIndex].Note = "forged not-delivered terminal"
	rehashEventChain(t, forgedFailure)
	if _, err := validateLedger(forgedFailure, cfg); err == nil || !strings.Contains(err.Error(), "已开始收敛") {
		t.Fatalf("strict replay accepted failed parent after child convergence: %v", err)
	}
}

func TestTurnBundleByteLimitStopsAtFIFOHead(t *testing.T) {
	e := setupTestEnv(t)
	setDeliveryPolicyWithBundle(t, e, deliveryModeAuto, 10, 10, maxDeliveryBundleBytes)
	sender, pending := enqueueTurnBundleContext(t, e, 2)
	firstEnvelope, err := sender.deliveryPayload(pending[0])
	if err != nil {
		t.Fatal(err)
	}
	setDeliveryPolicyWithBundle(t, e, deliveryModeAuto, 10, 10, len([]byte(firstEnvelope)))
	waker := deliveryPolicyTestApp(t, e, "eng-developer", "turn-bundle:sender")
	waker.DeliveryTargetState = sender.DeliveryTargetState
	if _, err := runDeliveryPolicyTest(waker, "message", "--to", "zantianyou", "--kind", "request", "--text", "byte-boundary", "--delivery", "wakeup"); err != nil {
		t.Fatal(err)
	}
	if len(e.transport.calls) != 1 || !strings.Contains(e.transport.calls[0].message, pending[0].Message) || strings.Contains(e.transport.calls[0].message, pending[1].Message) {
		t.Fatalf("byte-bounded prompt did not contain exactly the first item: %+v", e.transport.calls)
	}
	events, err := NewStore(e.data).ReadAll(waker.Config)
	if err != nil {
		t.Fatal(err)
	}
	messages := deliveryPolicyMessages(t, e)
	attempt, attemptIndex := findTurnBundleAttempt(t, events, messages[len(messages)-1].DeliveryID)
	if attempt.TurnBundleBytes != len([]byte(firstEnvelope)) || len(attempt.TurnBundleDeliveryIDs) != 1 || attempt.TurnBundleOverflow != 1 {
		t.Fatalf("byte boundary manifest=%+v", attempt)
	}
	if attempt.TurnBundleNextDeliveryID != pending[1].DeliveryID || attempt.TurnBundleNextDigest != pending[1].PayloadDigest ||
		attempt.TurnBundleNextItemBytes != len([]byte(attempt.TurnBundleNextEnvelope)) {
		t.Fatalf("byte overflow did not persist the real next FIFO item: %+v", attempt)
	}
	tampered := append([]Event(nil), events...)
	forgedNext := "\x00" + tampered[attemptIndex].TurnBundleNextEnvelope[1:]
	forgedDigest := digestText(forgedNext)
	for index := range tampered {
		if tampered[index].DeliveryID == pending[1].DeliveryID {
			tampered[index].PayloadDigest = forgedDigest
		}
	}
	tampered[attemptIndex].TurnBundleNextDigest = forgedDigest
	tampered[attemptIndex].TurnBundleNextEnvelope = forgedNext
	tampered[attemptIndex].TurnBundleDigest = turnBundleManifestDigest(tampered[attemptIndex], messages[len(messages)-1])
	rehashEventChain(t, tampered)
	if _, err := validateLedger(tampered, waker.Config); err == nil || !strings.Contains(err.Error(), "next envelope") {
		t.Fatalf("strict replay accepted re-signed unbounded/invalid next envelope: %v", err)
	}
}

func TestTurnBundleSentTerminalBatchRecoversAtomicallyAfterCrash(t *testing.T) {
	e := setupTestEnv(t)
	setDeliveryPolicyWithBundle(t, e, deliveryModeAuto, 10, 2, 64*1024)
	sender, pending := enqueueTurnBundleContext(t, e, 2)
	store, ok := sender.Store.(*Store)
	if !ok {
		t.Fatalf("test store type=%T", sender.Store)
	}
	fired := false
	e.transport.hook = func() {
		store.Failpoint = func(name string) error {
			if name == "state_temp_write" && !fired {
				fired = true
				return errors.New("synthetic terminal batch crash")
			}
			return nil
		}
	}
	if _, err := runDeliveryPolicyTest(sender, "message", "--to", "zantianyou", "--kind", "request", "--text", "crash-after-bundle-prompt", "--delivery", "wakeup"); err == nil || !strings.Contains(err.Error(), "synthetic terminal batch crash") {
		t.Fatalf("terminal batch crash not surfaced: %v", err)
	}
	if !fired || len(e.transport.calls) != 1 {
		t.Fatalf("failpoint=%t transport calls=%d", fired, len(e.transport.calls))
	}

	// A fresh process recovers the single journal. The parent terminal and all
	// selected message sent/claim events must appear together, never partially.
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	recovered := NewStore(e.data)
	events, err := recovered.ReadAll(cfg)
	if err != nil {
		t.Fatalf("recover terminal batch: %v", err)
	}
	messages := deliveryPolicyMessages(t, e)
	main := messages[len(messages)-1]
	mainView, ok, err := recovered.Delivery(cfg, main.DeliveryID)
	if err != nil || !ok || mainView.Status != deliverySent {
		t.Fatalf("recovered parent delivery=%+v ok=%t err=%v", mainView, ok, err)
	}
	views, err := recovered.Deliveries(cfg)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]DeliveryView, len(views))
	for _, view := range views {
		byID[view.DeliveryID] = view
	}
	for _, origin := range pending {
		view := byID[origin.DeliveryID]
		if view.Status != deliverySent || view.ContextState != deliveryContextClaimed ||
			view.BundledByAttemptID != mainView.AttemptEventID || view.BundledByDeliveryID != main.DeliveryID {
			t.Fatalf("recovered context did not converge atomically: %+v", view)
		}
	}
	if _, err := validateLedger(events, cfg); err != nil {
		t.Fatalf("recovered batch does not strictly replay: %v", err)
	}
}

func TestTurnBundleManualResolveDeliveredConvergesAndMessageCanAck(t *testing.T) {
	e := setupTestEnv(t)
	setDeliveryPolicyWithBundle(t, e, deliveryModeAuto, 10, 2, 64*1024)
	sender, pending := enqueueTurnBundleContext(t, e, 2)
	e.transport.result = transportAmbiguous
	e.transport.err = errors.New("synthetic ambiguous prompt")
	if _, err := runDeliveryPolicyTest(sender, "message", "--to", "zantianyou", "--kind", "request", "--text", "resolve-this-prompt", "--delivery", "wakeup"); err == nil {
		t.Fatal("ambiguous wake unexpectedly succeeded")
	}
	messages := deliveryPolicyMessages(t, e)
	main := messages[len(messages)-1]
	before := deliveryViewsByID(t, e)
	if before[main.DeliveryID].Status != deliveryUnknown {
		t.Fatalf("parent did not freeze unknown: %+v", before[main.DeliveryID])
	}
	for _, origin := range pending {
		if view := before[origin.DeliveryID]; view.Status != deliveryQueued || view.ContextState != deliveryContextPending {
			t.Fatalf("unknown claimed context before resolution: %+v", view)
		}
	}

	evidence := writeTestFile(t, filepath.Join(e.office, "turn-bundle-resolve-evidence.md"), "# verified prompt delivery\n")
	resolver := deliveryPolicyTestApp(t, e, "penny", "turn-bundle:resolver")
	if _, err := runDeliveryPolicyTest(resolver, "delivery", "resolve", "--id", main.DeliveryID, "--outcome", "delivered",
		"--reason", "operator verified target history", "--evidence", evidence); err != nil {
		t.Fatal(err)
	}
	after := deliveryViewsByID(t, e)
	parentAttempt := after[main.DeliveryID].AttemptEventID
	for _, origin := range pending {
		view := after[origin.DeliveryID]
		if view.Status != deliverySent || view.ContextState != deliveryContextClaimed || view.BundledByAttemptID != parentAttempt {
			t.Fatalf("resolve delivered did not converge bundled message: %+v", view)
		}
	}

	acker := deliveryPolicyTestApp(t, e, "zantianyou", "turn-bundle:acker")
	if _, err := runDeliveryPolicyTest(acker, "message", "ack", "--message", pending[0].MessageID); err != nil {
		t.Fatalf("bundled message ack failed: %v", err)
	}
	acked := deliveryViewsByID(t, e)[pending[0].DeliveryID]
	if acked.ProjectionStatus != "acked" || acked.AckedBy != "zantianyou" || acked.TerminalEventID == "" {
		t.Fatalf("bundled message ack projection is not explainable: %+v", acked)
	}
}

func TestTurnBundleConcurrentAttemptsReserveDisjointFIFOManifests(t *testing.T) {
	e := setupTestEnv(t)
	setDeliveryPolicyWithBundle(t, e, deliveryModeAuto, 10, 2, 64*1024)
	_, pending := enqueueTurnBundleContext(t, e, 3)
	transport := newTurnBundleBarrierTransport()
	apps := []*App{
		deliveryPolicyTestApp(t, e, "eng-developer", "turn-bundle:concurrent-a"),
		deliveryPolicyTestApp(t, e, "eng-developer", "turn-bundle:concurrent-b"),
	}
	for _, app := range apps {
		app.Transport = transport
		app.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeIdle, nil }
	}
	errs := make(chan error, len(apps))
	var workers sync.WaitGroup
	for index, app := range apps {
		workers.Add(1)
		go func(index int, app *App) {
			defer workers.Done()
			_, err := runDeliveryPolicyTest(app, "message", "--to", "zantianyou", "--kind", "request",
				"--text", "concurrent-wake-"+string(rune('a'+index)), "--delivery", "wakeup")
			errs <- err
		}(index, app)
	}
	select {
	case <-transport.firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first concurrent command did not enter transport")
	}
	// Both commands are genuinely concurrent, but the per-seat lifecycle lease
	// must keep the second Prompt out until the first transport call returns.
	time.Sleep(30 * time.Millisecond)
	if prompts := transport.snapshot(); len(prompts) != 1 {
		t.Fatalf("same-seat prompts were not serialized: %d", len(prompts))
	}
	close(transport.releaseFirst)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	prompts := transport.snapshot()
	if len(prompts) != 2 {
		t.Fatalf("concurrent prompts=%d want=2", len(prompts))
	}
	for _, origin := range pending {
		occurrences := 0
		for _, prompt := range prompts {
			occurrences += strings.Count(prompt, origin.Message)
		}
		if occurrences != 1 {
			t.Fatalf("pending context %s appeared in %d concurrent prompts", origin.MessageID, occurrences)
		}
	}

	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	selected := map[string]string{}
	manifestCount := 0
	for _, event := range events {
		if event.Type != "delivery_attempted" || !hasTurnBundleManifest(event) {
			continue
		}
		manifestCount++
		for _, deliveryID := range event.TurnBundleDeliveryIDs {
			if prior := selected[deliveryID]; prior != "" {
				t.Fatalf("context %s reserved by both %s and %s", deliveryID, prior, event.ID)
			}
			selected[deliveryID] = event.ID
		}
	}
	if manifestCount != 2 || len(selected) != len(pending) {
		t.Fatalf("concurrent manifests=%d selected=%d want=%d", manifestCount, len(selected), len(pending))
	}
	views := deliveryViewsByID(t, e)
	for _, origin := range pending {
		if view := views[origin.DeliveryID]; view.Status != deliverySent || view.ContextState != deliveryContextClaimed {
			t.Fatalf("concurrent context did not converge: %+v", view)
		}
	}
}

func TestTurnBundleAndContextConsumeNeverExposeTheSameEnvelope(t *testing.T) {
	t.Run("consume output holds ledger boundary through claim", func(t *testing.T) {
		e := setupTestEnv(t)
		setDeliveryPolicyWithBundle(t, e, deliveryModeAuto, 10, 2, 64*1024)
		_, pending := enqueueTurnBundleContext(t, e, 2)
		consumer := deliveryPolicyTestApp(t, e, "zantianyou", "turn-bundle:consume-first")
		writer := newTurnBundleBlockingWriter()
		consumer.Out, consumer.Err = writer, writer
		consumeErr := make(chan error, 1)
		go func() { consumeErr <- consumer.run([]string{"delivery", "consume", "--limit", "1"}) }()
		<-writer.entered

		waker := deliveryPolicyTestApp(t, e, "eng-developer", "turn-bundle:wake-second")
		waker.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeIdle, nil }
		wakeErr := make(chan error, 1)
		go func() {
			_, err := runDeliveryPolicyTest(waker, "message", "--to", "zantianyou", "--kind", "request", "--text", "wake-after-consume", "--delivery", "wakeup")
			wakeErr <- err
		}()
		close(writer.release)
		if err := <-consumeErr; err != nil {
			t.Fatal(err)
		}
		if err := <-wakeErr; err != nil {
			t.Fatal(err)
		}
		if len(e.transport.calls) != 1 {
			t.Fatalf("transport calls=%d", len(e.transport.calls))
		}
		prompt := e.transport.calls[0].message
		if strings.Contains(prompt, pending[0].Message) || !strings.Contains(prompt, pending[1].Message) {
			t.Fatalf("wake prompt duplicated consumed context or lost suffix: %q", prompt)
		}
	})

	t.Run("parent reservation excludes consume before transport terminal", func(t *testing.T) {
		e := setupTestEnv(t)
		setDeliveryPolicyWithBundle(t, e, deliveryModeAuto, 10, 2, 64*1024)
		_, pending := enqueueTurnBundleContext(t, e, 2)
		transportEntered := make(chan struct{})
		transportRelease := make(chan struct{})
		var once sync.Once
		e.transport.hook = func() {
			once.Do(func() { close(transportEntered) })
			<-transportRelease
		}
		waker := deliveryPolicyTestApp(t, e, "eng-developer", "turn-bundle:wake-first")
		waker.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeIdle, nil }
		wakeErr := make(chan error, 1)
		go func() {
			_, err := runDeliveryPolicyTest(waker, "message", "--to", "zantianyou", "--kind", "request", "--text", "wake-before-consume", "--delivery", "wakeup")
			wakeErr <- err
		}()
		<-transportEntered

		consumer := deliveryPolicyTestApp(t, e, "zantianyou", "turn-bundle:consume-second")
		output, err := runDeliveryPolicyTest(consumer, "delivery", "consume")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output, pending[0].Message) || strings.Contains(output, pending[1].Message) {
			t.Fatalf("consume exposed parent-reserved context: %q", output)
		}
		close(transportRelease)
		if err := <-wakeErr; err != nil {
			t.Fatal(err)
		}
		if len(e.transport.calls) != 1 || !strings.Contains(e.transport.calls[0].message, pending[0].Message) ||
			!strings.Contains(e.transport.calls[0].message, pending[1].Message) {
			t.Fatalf("parent prompt lost reserved context: %+v", e.transport.calls)
		}
	})
}

func TestTurnBundleExcludesAcknowledgedHistory(t *testing.T) {
	e := setupTestEnv(t)
	setDeliveryPolicyWithBundle(t, e, deliveryModeAuto, 10, 2, 64*1024)
	sender, pending := enqueueTurnBundleContext(t, e, 1)
	if err := sender.deliverQueuedAtNaturalWake(pending[0]); err != nil {
		t.Fatal(err)
	}
	acker := deliveryPolicyTestApp(t, e, "zantianyou", "turn-bundle:history-acker")
	if _, err := runDeliveryPolicyTest(acker, "message", "ack", "--message", pending[0].MessageID); err != nil {
		t.Fatal(err)
	}
	waker := deliveryPolicyTestApp(t, e, "eng-developer", "turn-bundle:acked-waker")
	waker.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeIdle, nil }
	if _, err := runDeliveryPolicyTest(waker, "message", "--to", "zantianyou", "--kind", "request", "--text", "wake-after-ack", "--delivery", "wakeup"); err != nil {
		t.Fatal(err)
	}
	if len(e.transport.calls) != 1 || strings.Contains(e.transport.calls[0].message, pending[0].Message) {
		t.Fatalf("acked history was bundled again: %+v", e.transport.calls)
	}
	events, err := NewStore(e.data).ReadAll(waker.Config)
	if err != nil {
		t.Fatal(err)
	}
	messages := deliveryPolicyMessages(t, e)
	attempt, _ := findTurnBundleAttempt(t, events, messages[len(messages)-1].DeliveryID)
	if len(attempt.TurnBundleDeliveryIDs) != 0 || attempt.TurnBundleOverflow != 0 {
		t.Fatalf("acked history remained a bundle candidate: %+v", attempt)
	}
}

func TestTurnBundleFailureAndUnknownClaimNothing(t *testing.T) {
	for _, test := range []struct {
		name    string
		outcome TransportOutcome
	}{
		{name: "definitely-not-sent", outcome: transportDefinitelyNotSent},
		{name: "ambiguous", outcome: transportAmbiguous},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := setupTestEnv(t)
			setDeliveryPolicyWithBundle(t, e, deliveryModeAuto, 10, 2, 64*1024)
			sender, pending := enqueueTurnBundleContext(t, e, 3)
			e.transport.result = test.outcome
			e.transport.err = errors.New("synthetic transport failure")
			if _, err := runDeliveryPolicyTest(sender, "message", "--to", "zantianyou", "--kind", "request", "--text", "failed-wake", "--delivery", "wakeup"); err == nil {
				t.Fatal("failing transport returned nil error")
			}
			if got := countDeliveryEvents(t, e, "delivery_context_claimed"); got != 0 {
				t.Fatalf("failed/unknown transport claimed %d context items", got)
			}
			views := deliveryViewsByID(t, e)
			for _, origin := range pending {
				if view := views[origin.DeliveryID]; view.ContextState != deliveryContextPending || view.Status != deliveryQueued {
					t.Fatalf("failed/unknown transport mutated pending context: %+v", view)
				}
			}
			cfg, err := loadConfig(e.config)
			if err != nil {
				t.Fatal(err)
			}
			events, err := NewStore(e.data).ReadAll(cfg)
			if err != nil {
				t.Fatal(err)
			}
			messages := deliveryPolicyMessages(t, e)
			attempt, _ := findTurnBundleAttempt(t, events, messages[len(messages)-1].DeliveryID)
			if len(attempt.TurnBundleDeliveryIDs) != 2 || attempt.TurnBundleOverflow != 1 {
				t.Fatalf("failed attempt did not durably preserve manifest: %+v", attempt)
			}
		})
	}
}

func TestTurnBundleFieldsRequireV3Attempt(t *testing.T) {
	e := setupTestEnv(t)
	setDeliveryPolicyWithBundle(t, e, deliveryModeAuto, 10, 1, 64*1024)
	sender, _ := enqueueTurnBundleContext(t, e, 1)
	if _, err := runDeliveryPolicyTest(sender, "message", "--to", "zantianyou", "--kind", "request", "--text", "schema-check", "--delivery", "wakeup"); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	messages := deliveryPolicyMessages(t, e)
	_, index := findTurnBundleAttempt(t, events, messages[len(messages)-1].DeliveryID)
	events[index].Version = 2
	rehashEventChain(t, events)
	if _, err := validateLedger(events, cfg); err == nil || !strings.Contains(err.Error(), "不支持事件版本 2") {
		t.Fatalf("v2 attempted event accepted: %v", err)
	}
}
