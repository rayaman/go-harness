package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Parallel tool execution tests (roadmap 2.1)
//
// Reuse the fake chat server + fakeTool from harness_test.go. The slow
// fake tools here block on channels or sleeps so the tests can prove
// (non-)overlap; every test also asserts History carries the tool
// messages in the ORIGINAL call order, byte-for-byte what the old
// sequential loop produced.
// ---------------------------------------------------------------------------

// chatToolCallsReply scripts one assistant message carrying several tool
// calls in the given order. Each call is [id, tool name, arguments JSON].
func chatToolCallsReply(calls ...[3]string) []byte {
	list := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		list = append(list, map[string]any{
			"id":   c[0],
			"type": "function",
			"function": map[string]any{
				"name":      c[1],
				"arguments": c[2],
			},
		})
	}
	return mustJSON(map[string]any{
		"choices": []map[string]any{
			{
				"message": map[string]any{
					"role":       "assistant",
					"content":    "",
					"tool_calls": list,
				},
				"finish_reason": "tool_calls",
			},
		},
	})
}

// eventLog is a mutex-guarded sequence recorder shared by tools and the
// Confirm hook so tests can assert exact interleavings.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(ev string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// barrierTool is a concurrency-safe fake tool that signals its Execute
// has started and then blocks until the test closes the hold channel.
// Two barrierTools can both signal "started" only if the harness runs
// them concurrently: a sequential dispatch deadlocks on the first hold.
type barrierTool struct {
	*fakeTool
	started chan struct{} // buffered (cap 1): one signal per Execute entry
	hold    chan struct{} // closed by the test to release the tool
}

func (b *barrierTool) Execute(args json.RawMessage) (string, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.hold:
	case <-time.After(5 * time.Second):
		return "hold timed out", nil
	}
	return b.fakeTool.Execute(args)
}

// slowSerialTool is a Serial() true fake tool that records start/end
// events around a sleep, so tests can pin its non-overlapping execution.
type slowSerialTool struct {
	*fakeTool
	rec   *eventLog
	tag   string
	delay time.Duration
}

func (s *slowSerialTool) Serial() bool { return true }

func (s *slowSerialTool) Execute(args json.RawMessage) (string, error) {
	s.rec.add("start:" + s.tag)
	time.Sleep(s.delay)
	out, err := s.fakeTool.Execute(args)
	s.rec.add("end:" + s.tag)
	return out, err
}

// delayedTool is a concurrency-safe fake tool that sleeps before
// returning, so staggered delays exercise completion-order vs call-order.
type delayedTool struct {
	*fakeTool
	delay time.Duration
}

func (d *delayedTool) Execute(args json.RawMessage) (string, error) {
	time.Sleep(d.delay)
	return d.fakeTool.Execute(args)
}

// assertToolMsgOrder checks that History's tool messages at offset
// base..base+n-1 carry the given tool_call_ids (and contents) in order.
func assertToolMsgOrder(t *testing.T, history []Message, base int, wantIDs, wantContents, wantNames []string) {
	t.Helper()
	for i := range wantIDs {
		m := history[base+i]
		if m.Role != "tool" || m.ToolCallID != wantIDs[i] ||
			m.Name != wantNames[i] || ExtractText(m.Content) != wantContents[i] {
			t.Errorf("History[%d] = role %q tool_call_id %q name %q content %q, want tool/%s/%s/%s",
				base+i, m.Role, m.ToolCallID, m.Name, ExtractText(m.Content),
				wantIDs[i], wantNames[i], wantContents[i])
		}
	}
}

// TestParallelSafeToolsConcurrent: two concurrency-safe fake tools each
// block until the other has ENTERED Execute. The test releases them only
// after observing both start signals, so the turn completes only if the
// scheduler actually runs them concurrently; a sequential loop would
// trip the 2s deadline. History must still show the tool messages in
// original call order.
func TestParallelSafeToolsConcurrent(t *testing.T) {
	a := &barrierTool{
		fakeTool: &fakeTool{name: "slow_a", output: "out-a"},
		started:  make(chan struct{}, 1),
		hold:     make(chan struct{}),
	}
	b := &barrierTool{
		fakeTool: &fakeTool{name: "slow_b", output: "out-b"},
		started:  make(chan struct{}, 1),
		hold:     make(chan struct{}),
	}
	reg := NewRegistry()
	reg.Register(a)
	reg.Register(b)

	srv := newFakeChatServer(t,
		okReply(chatToolCallsReply(
			[3]string{"call_1", "slow_a", `{}`},
			[3]string{"call_2", "slow_b", `{}`},
		)),
		okReply(chatTextReply("final answer")),
	)
	h := newLoopHarness(srv.URL(), Config{}, reg)

	done := make(chan struct{})
	var askErr error
	go func() {
		defer close(done)
		_, askErr = h.Ask(context.Background(), "run both slowly")
	}()

	started := 0
	deadline := time.After(2 * time.Second)
	for started < 2 {
		select {
		case <-a.started:
			started++
		case <-b.started:
			started++
		case <-deadline:
			// Unblock the stuck tool(s) so the test process can finish.
			close(a.hold)
			close(b.hold)
			t.Fatalf("only %d of 2 tools entered Execute before the deadline: dispatch is not concurrent", started)
		}
	}

	// Both Executes are in flight at this point: release them.
	close(a.hold)
	close(b.hold)
	<-done
	if askErr != nil {
		t.Fatalf("Ask: %v", askErr)
	}

	// user, assistant(tool_calls), tool, tool, assistant(final).
	if len(h.History) != 5 {
		t.Fatalf("History has %d messages, want 5: %+v", len(h.History), h.History)
	}
	assertToolMsgOrder(t, h.History, 2,
		[]string{"call_1", "call_2"},
		[]string{"out-a", "out-b"},
		[]string{"slow_a", "slow_b"})
	if final := h.History[4]; final.Role != "assistant" || ExtractText(final.Content) != "final answer" {
		t.Errorf("History[4] = role %q content %q, want assistant/final answer",
			final.Role, ExtractText(final.Content))
	}
}

// TestSerialToolsNotConcurrent: two Serial() true fake tools must run
// one at a time in call order even with MaxParallelTools=4: the
// recorded event sequence must be strictly start/end pairs with no
// overlap.
func TestSerialToolsNotConcurrent(t *testing.T) {
	rec := &eventLog{}
	a := &slowSerialTool{fakeTool: &fakeTool{name: "ser_a", output: "out-a"}, rec: rec, tag: "a", delay: 40 * time.Millisecond}
	b := &slowSerialTool{fakeTool: &fakeTool{name: "ser_b", output: "out-b"}, rec: rec, tag: "b", delay: 40 * time.Millisecond}
	reg := NewRegistry()
	reg.Register(a)
	reg.Register(b)

	srv := newFakeChatServer(t,
		okReply(chatToolCallsReply(
			[3]string{"s1", "ser_a", `{}`},
			[3]string{"s2", "ser_b", `{}`},
		)),
		okReply(chatTextReply("done")),
	)
	h := newLoopHarness(srv.URL(), Config{MaxParallelTools: 4}, reg)

	if _, err := h.Ask(context.Background(), "run both serially"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	want := []string{"start:a", "end:a", "start:b", "end:b"}
	got := rec.snapshot()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("event sequence = %v, want %v (serial tools must not overlap)", got, want)
	}
	if len(h.History) != 5 {
		t.Fatalf("History has %d messages, want 5", len(h.History))
	}
	assertToolMsgOrder(t, h.History, 2,
		[]string{"s1", "s2"},
		[]string{"out-a", "out-b"},
		[]string{"ser_a", "ser_b"})
}

// TestParallelOrderPreserved: five safe tools with staggered delays
// where call 1 is the SLOWEST; History's tool messages must still match
// the original call order, not completion order.
func TestParallelOrderPreserved(t *testing.T) {
	// call_1 slowest, then descending-ish staggered delays so completion
	// order differs sharply from call order.
	delays := []time.Duration{80 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond,
		30 * time.Millisecond, 40 * time.Millisecond}
	reg := NewRegistry()
	ids := make([]string, 5)
	names := make([]string, 5)
	outs := make([]string, 5)
	calls := make([][3]string, 5)
	for i := 0; i < 5; i++ {
		ids[i] = fmt.Sprintf("call_%d", i+1)
		names[i] = fmt.Sprintf("slow_%d", i+1)
		outs[i] = fmt.Sprintf("out-%d", i+1)
		reg.Register(&delayedTool{fakeTool: &fakeTool{name: names[i], output: outs[i]}, delay: delays[i]})
		calls[i] = [3]string{ids[i], names[i], `{}`}
	}

	srv := newFakeChatServer(t,
		okReply(chatToolCallsReply(calls...)),
		okReply(chatTextReply("ordered")),
	)
	// MaxParallelTools left at 0 -> default pool size 4, so the 5th call
	// also has to wait for a worker slot: still ordered regardless.
	h := newLoopHarness(srv.URL(), Config{}, reg)

	if _, err := h.Ask(context.Background(), "five slow calls"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if len(h.History) != 8 {
		t.Fatalf("History has %d messages, want 8", len(h.History))
	}
	assertToolMsgOrder(t, h.History, 2, ids, outs, names)
}

// TestAskPolicyForcesSerial: two tools at PolicyAsk WITHOUT Serial().
// The scheduler must force them serial so the Confirm hook (stdin in
// the REPL) never runs two prompts at once. The hook records enter/exit
// events around a sleep; the recorded events must be strictly paired
// confirm-start/confirm-end with no interleaving.
func TestAskPolicyForcesSerial(t *testing.T) {
	rec := &eventLog{}
	probe := func(name, tag string) *askProbeTool {
		return &askProbeTool{fakeTool: &fakeTool{name: name, output: "out-" + tag}, rec: rec, tag: tag, delay: 20 * time.Millisecond}
	}
	a := probe("ask_a", "a")
	b := probe("ask_b", "b")
	reg := NewRegistry()
	reg.Register(a)
	reg.Register(b)
	reg.SetPolicy("ask_a", PolicyAsk)
	reg.SetPolicy("ask_b", PolicyAsk)

	srv := newFakeChatServer(t,
		okReply(chatToolCallsReply(
			[3]string{"q1", "ask_a", `{}`},
			[3]string{"q2", "ask_b", `{}`},
		)),
		okReply(chatTextReply("approved")),
	)
	h := newLoopHarness(srv.URL(), Config{MaxParallelTools: 4}, reg)
	h.Confirm = func(call ToolCall) bool {
		rec.add("confirm-start:" + call.ID)
		time.Sleep(25 * time.Millisecond)
		rec.add("confirm-end:" + call.ID)
		return true
	}

	if _, err := h.Ask(context.Background(), "confirm two tools"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	events := rec.snapshot()
	// Pull just the confirm events out and require strict pairing:
	// start, end, start, end with matching ids (no nesting/overlap).
	var confirms []string
	for _, ev := range events {
		if len(ev) >= 7 && ev[:7] == "confirm" {
			confirms = append(confirms, ev)
		}
	}
	wantConfirms := []string{"confirm-start:q1", "confirm-end:q1", "confirm-start:q2", "confirm-end:q2"}
	if fmt.Sprint(confirms) != fmt.Sprint(wantConfirms) {
		t.Errorf("confirm event sequence = %v, want %v (PolicyAsk prompts must never overlap)", confirms, wantConfirms)
	}

	if len(h.History) != 5 {
		t.Fatalf("History has %d messages, want 5", len(h.History))
	}
	assertToolMsgOrder(t, h.History, 2,
		[]string{"q1", "q2"},
		[]string{"out-a", "out-b"},
		[]string{"ask_a", "ask_b"})
}

// askProbeTool is a NON-serial fake tool (no Serial method) recording
// start/end events, used with PolicyAsk policies.
type askProbeTool struct {
	*fakeTool
	rec   *eventLog
	tag   string
	delay time.Duration
}

func (p *askProbeTool) Execute(args json.RawMessage) (string, error) {
	p.rec.add("exec-start:" + p.tag)
	time.Sleep(p.delay)
	out, err := p.fakeTool.Execute(args)
	p.rec.add("exec-end:" + p.tag)
	return out, err
}

// boundTool records the peak number of in-flight Executes and blocks
// each call until the Nth arrival closes the release channel (N = the
// configured pool cap). If the pool ever exceeded the cap the peak
// would rise above N; if the pool were smaller than N the release
// never fires and the peak stays below N. Either way the test's
// peak == cap assertion catches it deterministically.
type boundTool struct {
	*fakeTool
	inflight *int64
	arrived  *int64
	peak     *int64
	peakMu   *sync.Mutex
	release  chan struct{}
	closeOn  *sync.Once
	target   int64
}

func (b *boundTool) Execute(args json.RawMessage) (string, error) {
	n := atomic.AddInt64(b.inflight, 1)
	defer atomic.AddInt64(b.inflight, -1)
	b.peakMu.Lock()
	if n > *b.peak {
		*b.peak = n
	}
	b.peakMu.Unlock()

	if atomic.AddInt64(b.arrived, 1) >= b.target {
		b.closeOn.Do(func() { close(b.release) })
	}
	select {
	case <-b.release:
	case <-time.After(5 * time.Second):
		return "release timed out", nil
	}
	return b.fakeTool.Execute(args)
}

// TestMaxParallelToolsBounds: MaxParallelTools=2 with five safe tools;
// the observed peak in-flight count must be EXACTLY 2 (the arrival
// barrier proves the pool really ran 2 at once, the semaphore must keep
// it from running 3).
func TestMaxParallelToolsBounds(t *testing.T) {
	var inflight, arrived, peak int64
	var peakMu sync.Mutex
	var closeOnce sync.Once
	release := make(chan struct{})

	reg := NewRegistry()
	calls := make([][3]string, 5)
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("bound_%d", i+1)
		reg.Register(&boundTool{
			fakeTool: &fakeTool{name: name, output: fmt.Sprintf("out-%d", i+1)},
			inflight: &inflight,
			arrived:  &arrived,
			peak:     &peak,
			peakMu:   &peakMu,
			release:  release,
			closeOn:  &closeOnce,
			target:   2, // = MaxParallelTools
		})
		calls[i] = [3]string{fmt.Sprintf("b%d", i+1), name, `{}`}
	}

	srv := newFakeChatServer(t,
		okReply(chatToolCallsReply(calls...)),
		okReply(chatTextReply("bounded")),
	)
	h := newLoopHarness(srv.URL(), Config{MaxParallelTools: 2}, reg)

	if _, err := h.Ask(context.Background(), "five calls, cap two"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	peakMu.Lock()
	gotPeak := peak
	peakMu.Unlock()
	if gotPeak != 2 {
		t.Errorf("peak concurrent tool executions = %d, want exactly 2 (MaxParallelTools=2)", gotPeak)
	}
	if gotArrived := atomic.LoadInt64(&arrived); gotArrived != 5 {
		t.Errorf("tools executed %d time(s), want 5", gotArrived)
	}

	if len(h.History) != 8 {
		t.Fatalf("History has %d messages, want 8", len(h.History))
	}
	for i := 0; i < 5; i++ {
		m := h.History[2+i]
		if m.Role != "tool" || m.ToolCallID != fmt.Sprintf("b%d", i+1) {
			t.Errorf("History[%d] = role %q tool_call_id %q, want tool/b%d (call order must be preserved)",
				2+i, m.Role, m.ToolCallID, i+1)
		}
	}
}
