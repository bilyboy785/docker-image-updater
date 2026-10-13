package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeClock is a manual clock: sleeping advances it instantly.
type fakeClock struct {
	mu      sync.Mutex
	t       time.Time
	sleeps  []time.Duration
	onSleep func(d time.Duration)
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_000_000, 0)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
	hook := c.onSleep
	c.mu.Unlock()
	if hook != nil {
		hook(d)
	}
	return true
}

func (c *fakeClock) recordedSleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

type call struct {
	text string
	at   time.Time
}

// fakeInner records calls; results come from the script (nil = success).
type fakeInner struct {
	clock   *fakeClock
	mu      sync.Mutex
	calls   []call
	script  []error       // result of the n-th call; missing entries succeed
	entered chan struct{} // receives one value per call when non-nil
	gate    chan struct{} // when non-nil, the first call blocks until it is closed
	block   bool          // block every call until ctx is done
}

func (f *fakeInner) Notify(ctx context.Context, text string) error {
	f.mu.Lock()
	n := len(f.calls)
	f.calls = append(f.calls, call{text, f.clock.now()})
	var err error
	if n < len(f.script) {
		err = f.script[n]
	}
	gate, entered, block := f.gate, f.entered, f.block
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if gate != nil && n == 0 {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeInner) all() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeInner) texts() []string {
	var out []string
	for _, c := range f.all() {
		out = append(out, c.text)
	}
	return out
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func testQueue(inner Notifier, clock *fakeClock) (*Queue, *lockedBuf) {
	buf := &lockedBuf{}
	log := slog.New(slog.NewTextHandler(buf, nil))
	q := newQueue(inner, log, clock.now, clock.sleep)
	return q, buf
}

func waitCalls(t *testing.T, ch chan struct{}, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for call %d", i+1)
		}
	}
}

func TestQueueFirstMessageSentImmediately(t *testing.T) {
	clock := newFakeClock()
	inner := &fakeInner{clock: clock, entered: make(chan struct{}, 10)}
	q, _ := testQueue(inner, clock)
	defer q.Close(time.Second)
	if err := q.Notify(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, inner.entered, 1)
	if got := inner.texts(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("calls %q", got)
	}
	if s := clock.recordedSleeps(); len(s) != 0 {
		t.Fatalf("idle queue must not wait, slept %v", s)
	}
}

func TestQueueCoalescesBurst(t *testing.T) {
	clock := newFakeClock()
	inner := &fakeInner{clock: clock, entered: make(chan struct{}, 10), gate: make(chan struct{})}
	q, _ := testQueue(inner, clock)
	q.Notify(context.Background(), "a")
	waitCalls(t, inner.entered, 1) // "a" is in flight
	for _, m := range []string{"b", "c", "d"} {
		q.Notify(context.Background(), m)
	}
	close(inner.gate)
	waitCalls(t, inner.entered, 1)
	q.Close(time.Second)
	got := inner.texts()
	if len(got) != 2 || got[0] != "a" || got[1] != "b\n\nc\n\nd" {
		t.Fatalf("calls %q", got)
	}
}

func TestQueueBatchRespectsRuneCap(t *testing.T) {
	clock := newFakeClock()
	inner := &fakeInner{clock: clock, entered: make(chan struct{}, 10), gate: make(chan struct{})}
	q, _ := testQueue(inner, clock)
	q.Notify(context.Background(), "first")
	waitCalls(t, inner.entered, 1)
	m := strings.Repeat("é", 1500) // 1500 runes, 3000 bytes
	for i := 0; i < 3; i++ {
		q.Notify(context.Background(), m)
	}
	close(inner.gate)
	q.Close(time.Second)
	got := inner.texts()
	if len(got) != 3 {
		t.Fatalf("want 3 requests, got %d", len(got))
	}
	if got[1] != m+"\n\n"+m {
		t.Fatal("second request should hold two messages (3002 runes)")
	}
	if got[2] != m {
		t.Fatal("third request should hold the remaining message")
	}
	for _, c := range got {
		if utf8.RuneCountInString(c) > maxMessageRunes {
			t.Fatalf("request of %d runes", utf8.RuneCountInString(c))
		}
	}
}

func TestTruncateRuneSafe(t *testing.T) {
	in := strings.Repeat("é", 5000)
	out := truncateMessage(in)
	if n := utf8.RuneCountInString(out); n != maxMessageRunes {
		t.Fatalf("runes %d", n)
	}
	if !utf8.ValidString(out) || !strings.HasSuffix(out, "…") {
		t.Fatal("bad truncation")
	}
	if short := "short"; truncateMessage(short) != short {
		t.Fatal("short message must be untouched")
	}
}

func TestTruncateKeepsHTMLValid(t *testing.T) {
	// Cut inside a tag.
	in := strings.Repeat("x", maxMessageRunes-3) + "<code>tail</code>"
	out := truncateMessage(in)
	if strings.Contains(out, "<code") || strings.Contains(out, "<") {
		t.Fatalf("dangling tag fragment: ...%q", out[len(out)-20:])
	}
	// Cut inside an entity.
	in = strings.Repeat("x", maxMessageRunes-3) + "&amp; more text"
	if out = truncateMessage(in); strings.Contains(out, "&") {
		t.Fatalf("dangling entity: ...%q", out[len(out)-20:])
	}
	// Open tags are closed.
	in = "<b>" + strings.Repeat("x", 5000) + "</b>"
	out = truncateMessage(in)
	if !strings.HasSuffix(out, "…</b>") || utf8.RuneCountInString(out) > maxMessageRunes {
		t.Fatalf("open tag not closed: ...%q (%d runes)", out[len(out)-20:], utf8.RuneCountInString(out))
	}
	in = "<b>Title</b>\n<code>" + strings.Repeat("y", 5000) + "</code>"
	out = truncateMessage(in)
	if !strings.HasSuffix(out, "…</code>") || strings.Count(out, "<b>") != strings.Count(out, "</b>") {
		t.Fatalf("bad close: ...%q", out[len(out)-20:])
	}
}

func TestQueueSpacingBetweenSends(t *testing.T) {
	clock := newFakeClock()
	inner := &fakeInner{clock: clock, entered: make(chan struct{}, 10), gate: make(chan struct{})}
	q, _ := testQueue(inner, clock)
	q.Notify(context.Background(), "a")
	waitCalls(t, inner.entered, 1)
	q.Notify(context.Background(), "b")
	close(inner.gate)
	q.Close(time.Second)
	calls := inner.all()
	if len(calls) != 2 {
		t.Fatalf("calls %d", len(calls))
	}
	if gap := calls[1].at.Sub(calls[0].at); gap < defaultMinInterval {
		t.Fatalf("gap %v < %v", gap, defaultMinInterval)
	}
}

func TestQueueRateLimitWaitsRetryAfterPlusMargin(t *testing.T) {
	clock := newFakeClock()
	inner := &fakeInner{clock: clock, script: []error{&RateLimitError{RetryAfter: 7 * time.Second}}}
	q, _ := testQueue(inner, clock)
	q.Notify(context.Background(), "msg")
	q.Close(time.Second)
	calls := inner.all()
	if len(calls) != 2 || calls[0].text != "msg" || calls[1].text != "msg" {
		t.Fatalf("calls %+v", calls)
	}
	if gap := calls[1].at.Sub(calls[0].at); gap != 8*time.Second {
		t.Fatalf("waited %v, want 8s", gap)
	}
}

func TestQueueRetryBackoffThenDrop(t *testing.T) {
	clock := newFakeClock()
	boom := errors.New("boom")
	inner := &fakeInner{clock: clock, script: []error{boom, boom, boom, boom, boom, boom}}
	q, logs := testQueue(inner, clock)
	q.Notify(context.Background(), "msg")
	q.Close(time.Second)
	calls := inner.all()
	if len(calls) != 5 {
		t.Fatalf("attempts %d, want 5", len(calls))
	}
	want := []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}
	for i, w := range want {
		if gap := calls[i+1].at.Sub(calls[i].at); gap != w {
			t.Errorf("backoff %d: %v, want %v", i, gap, w)
		}
	}
	if l := logs.String(); !strings.Contains(l, "level=WARN") || !strings.Contains(l, "dropped") {
		t.Fatalf("logs %s", l)
	}
}

func TestQueueRetrySucceedsLater(t *testing.T) {
	clock := newFakeClock()
	inner := &fakeInner{clock: clock, script: []error{errors.New("boom"), errors.New("boom")}}
	q, _ := testQueue(inner, clock)
	q.Notify(context.Background(), "msg")
	q.Close(time.Second)
	if n := len(inner.all()); n != 3 {
		t.Fatalf("attempts %d, want 3", n)
	}
}

func TestQueueNonRetryableDroppedAfterOneAttempt(t *testing.T) {
	clock := newFakeClock()
	inner := &fakeInner{clock: clock, script: []error{&StatusError{Code: 400, Body: "can't parse entities"}}}
	q, logs := testQueue(inner, clock)
	q.Notify(context.Background(), "bad <b")
	q.Close(time.Second)
	if n := len(inner.all()); n != 1 {
		t.Fatalf("attempts %d, want 1", n)
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("no warn: %s", logs.String())
	}
}

func TestQueueMessagesDuringBackoffGoToNextBatch(t *testing.T) {
	clock := newFakeClock()
	inner := &fakeInner{clock: clock, script: []error{errors.New("boom")}, entered: make(chan struct{}, 10)}
	q, _ := testQueue(inner, clock)
	var once sync.Once
	clock.onSleep = func(d time.Duration) {
		if d == 5*time.Second {
			once.Do(func() { q.Notify(context.Background(), "late1"); q.Notify(context.Background(), "late2") })
		}
	}
	q.Notify(context.Background(), "early")
	waitCalls(t, inner.entered, 3) // Close would otherwise reject the late messages
	q.Close(time.Second)
	got := inner.texts()
	want := []string{"early", "early", "late1\n\nlate2"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

func TestQueueBacklogCap(t *testing.T) {
	clock := newFakeClock()
	inner := &fakeInner{clock: clock, entered: make(chan struct{}, 10), gate: make(chan struct{})}
	q, logs := testQueue(inner, clock)
	q.Notify(context.Background(), "first")
	waitCalls(t, inner.entered, 1)
	for i := 1; i <= 205; i++ {
		q.Notify(context.Background(), fmt.Sprintf("m%d", i))
	}
	close(inner.gate)
	q.Close(time.Second)
	got := inner.texts()
	if len(got) != 2 {
		t.Fatalf("requests %d", len(got))
	}
	notice := "⚠️ 5 earlier notifications were dropped (backlog limit)"
	if !strings.HasPrefix(got[1], notice+"\n\nm6\n\nm7") || !strings.HasSuffix(got[1], "m205") || strings.Contains(got[1], "\nm5\n") {
		t.Fatalf("unexpected batch: %.120q", got[1])
	}
	if n := strings.Count(logs.String(), "backlog"); n != 1 {
		t.Fatalf("want one backlog warning, got %d: %s", n, logs.String())
	}
}

func TestQueueCloseFlushesPendingAndIsIdempotent(t *testing.T) {
	clock := newFakeClock()
	inner := &fakeInner{clock: clock}
	q, _ := testQueue(inner, clock)
	for i := 0; i < 5; i++ {
		q.Notify(context.Background(), fmt.Sprintf("m%d", i))
	}
	q.Close(5 * time.Second)
	q.Close(5 * time.Second)
	joined := strings.Join(inner.texts(), "\n\n")
	if !strings.Contains(joined, "m0") || !strings.Contains(joined, "m4") {
		t.Fatalf("not flushed: %q", joined)
	}
	select {
	case <-q.done:
	default:
		t.Fatal("worker still running after Close")
	}
	n := len(inner.all())
	if err := q.Notify(context.Background(), "after"); err != nil {
		t.Fatal(err)
	}
	if len(inner.all()) != n {
		t.Fatal("Notify after Close must be a no-op")
	}
}

func TestQueueNotifyNeverBlocksAndCloseTimesOut(t *testing.T) {
	clock := newFakeClock()
	inner := &fakeInner{clock: clock, block: true, entered: make(chan struct{}, 100)}
	q, _ := testQueue(inner, clock)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			q.Notify(context.Background(), "x")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Notify blocked while inner is stuck")
	}
	waitCalls(t, inner.entered, 1)
	start := time.Now()
	q.Close(20 * time.Millisecond)
	if time.Since(start) > 5*time.Second {
		t.Fatal("Close did not honour its timeout")
	}
	select {
	case <-q.done:
	default:
		t.Fatal("worker leaked after Close")
	}
}

func TestQueueCloseInterruptsRetryWait(t *testing.T) {
	// Real sleeper: a 10 minute backoff must be cut short by Close.
	inner := &fakeInner{clock: newFakeClock(), script: []error{&RateLimitError{RetryAfter: 10 * time.Minute}}, entered: make(chan struct{}, 10)}
	q := NewQueue(inner, slog.New(slog.NewTextHandler(&lockedBuf{}, nil)))
	q.Notify(context.Background(), "x")
	waitCalls(t, inner.entered, 1)
	start := time.Now()
	q.Close(50 * time.Millisecond)
	if time.Since(start) > 5*time.Second {
		t.Fatal("Close blocked on retry wait")
	}
	select {
	case <-q.done:
	default:
		t.Fatal("worker leaked")
	}
}
