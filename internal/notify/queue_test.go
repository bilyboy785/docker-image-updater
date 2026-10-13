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
	script  []error                 // result of the n-th call; missing entries succeed
	entered chan struct{}           // receives one value per call when non-nil
	gate    chan struct{}           // when non-nil, the first call blocks until it is closed
	block   bool                    // block every call until ctx is done
	fn      func(text string) error // when set, decides the result of every call
}

func (f *fakeInner) Notify(ctx context.Context, text string) error {
	f.mu.Lock()
	n := len(f.calls)
	f.calls = append(f.calls, call{text, f.clock.now()})
	var err error
	if n < len(f.script) {
		err = f.script[n]
	}
	gate, entered, block, fn := f.gate, f.entered, f.block, f.fn
	f.mu.Unlock()
	if fn != nil {
		if e := fn(text); e != nil {
			return e
		}
	}
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
		if unitLen(c) > maxMessageUnits {
			t.Fatalf("request of %d runes", unitLen(c))
		}
	}
}

func TestTruncateRuneSafe(t *testing.T) {
	in := strings.Repeat("é", 5000)
	out := truncateTo(in, maxMessageUnits)
	if n := unitLen(out); n != maxMessageUnits {
		t.Fatalf("runes %d", n)
	}
	if !utf8.ValidString(out) || !strings.HasSuffix(out, "…") {
		t.Fatal("bad truncation")
	}
	if short := "short"; truncateTo(short, maxMessageUnits) != short {
		t.Fatal("short message must be untouched")
	}
}

func TestTruncateKeepsHTMLValid(t *testing.T) {
	// Cut inside a tag.
	in := strings.Repeat("x", maxMessageUnits-3) + "<code>tail</code>"
	out := truncateTo(in, maxMessageUnits)
	if strings.Contains(out, "<code") || strings.Contains(out, "<") {
		t.Fatalf("dangling tag fragment: ...%q", out[len(out)-20:])
	}
	// Cut inside an entity.
	in = strings.Repeat("x", maxMessageUnits-3) + "&amp; more text"
	if out = truncateTo(in, maxMessageUnits); strings.Contains(out, "&") {
		t.Fatalf("dangling entity: ...%q", out[len(out)-20:])
	}
	// Open tags are closed.
	in = "<b>" + strings.Repeat("x", 5000) + "</b>"
	out = truncateTo(in, maxMessageUnits)
	if !strings.HasSuffix(out, "…</b>") || unitLen(out) > maxMessageUnits {
		t.Fatalf("open tag not closed: ...%q (%d runes)", out[len(out)-20:], unitLen(out))
	}
	in = "<b>Title</b>\n<code>" + strings.Repeat("y", 5000) + "</code>"
	out = truncateTo(in, maxMessageUnits)
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

func TestQueueBacklogNoticeSurvivesHugeHeadMessage(t *testing.T) {
	clock := newFakeClock()
	inner := &fakeInner{clock: clock, entered: make(chan struct{}, 10), gate: make(chan struct{})}
	q, _ := testQueue(inner, clock)
	q.Notify(context.Background(), "first")
	waitCalls(t, inner.entered, 1)
	for i := 0; i < 5; i++ {
		q.Notify(context.Background(), "filler")
	}
	q.Notify(context.Background(), strings.Repeat("x", 4000))
	for i := 0; i < 199; i++ {
		q.Notify(context.Background(), "s")
	}
	close(inner.gate)
	q.Close(time.Second)
	got := inner.texts()
	if len(got) < 3 {
		t.Fatalf("requests %d", len(got))
	}
	notice := "⚠️ 5 earlier notifications were dropped (backlog limit)"
	if !strings.HasPrefix(got[1], notice+"\n\nxxx") || !strings.Contains(got[1], "…") {
		t.Fatalf("notice lost: %.100q", got[1])
	}
	if unitLen(got[1]) > maxMessageUnits {
		t.Fatalf("request of %d units", unitLen(got[1]))
	}
	if strings.Count(strings.Join(got, "|"), "earlier notifications") != 1 {
		t.Fatal("notice must be sent exactly once")
	}
}

func TestQueueBadRequestOnMergedBatchResendsOneByOne(t *testing.T) {
	clock := newFakeClock()
	inner := &fakeInner{clock: clock, entered: make(chan struct{}, 20), gate: make(chan struct{})}
	inner.fn = func(text string) error {
		if text == "a\n\nb\n\nc" || text == "b" {
			return &StatusError{Code: 400, Body: "can't parse entities"}
		}
		return nil
	}
	q, logs := testQueue(inner, clock)
	q.Notify(context.Background(), "first")
	waitCalls(t, inner.entered, 1)
	for _, m := range []string{"a", "b", "c"} {
		q.Notify(context.Background(), m)
	}
	close(inner.gate)
	q.Close(time.Second)
	want := []string{"first", "a\n\nb\n\nc", "a", "b", "c"}
	calls := inner.all()
	if len(calls) != len(want) {
		t.Fatalf("calls %q", inner.texts())
	}
	for i, w := range want {
		if calls[i].text != w {
			t.Fatalf("calls %q, want %q", inner.texts(), want)
		}
		if i > 0 && calls[i].at.Sub(calls[i-1].at) < defaultMinInterval {
			t.Fatalf("call %d not spaced", i)
		}
	}
	if !strings.Contains(logs.String(), "not retryable") {
		t.Fatalf("expected a drop warning: %s", logs.String())
	}
}

func TestQueueRecoversFromPanickingNotifier(t *testing.T) {
	clock := newFakeClock()
	var mu sync.Mutex
	n := 0
	inner := &fakeInner{clock: clock}
	inner.fn = func(string) error {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			panic("secret message text")
		}
		return nil
	}
	q, logs := testQueue(inner, clock)
	q.Notify(context.Background(), "x")
	q.Close(time.Second)
	if len(inner.all()) != 2 {
		t.Fatalf("calls %d", len(inner.all()))
	}
	if l := logs.String(); !strings.Contains(l, "panicked") || strings.Contains(l, "secret message text") {
		t.Fatalf("logs %s", l)
	}
}

func TestQueueRateLimitRetriesAreCappedAndSeparateFromAttempts(t *testing.T) {
	rl := &RateLimitError{RetryAfter: time.Second}
	boom := errors.New("boom")
	// 20 rate limits and 4 temporary failures still end in success.
	script := []error{}
	for i := 0; i < 20; i++ {
		script = append(script, rl)
	}
	script = append(script, boom, boom, boom, boom)
	clock := newFakeClock()
	inner := &fakeInner{clock: clock, script: script}
	q, _ := testQueue(inner, clock)
	q.Notify(context.Background(), "m")
	q.Close(time.Second)
	if n := len(inner.all()); n != 25 {
		t.Fatalf("calls %d, want 25", n)
	}
	// Permanent rate limiting: the batch is dropped after 1 + 20 tries.
	script = nil
	for i := 0; i < 40; i++ {
		script = append(script, rl)
	}
	clock = newFakeClock()
	inner = &fakeInner{clock: clock, script: script}
	q, logs := testQueue(inner, clock)
	q.Notify(context.Background(), "m")
	q.Close(time.Second)
	if n := len(inner.all()); n != 21 {
		t.Fatalf("calls %d, want 21", n)
	}
	if !strings.Contains(logs.String(), "still rate limited") {
		t.Fatalf("logs %s", logs.String())
	}
}

func TestTruncateUsesUTF16Units(t *testing.T) {
	in := strings.Repeat("😀", 3000) // 3000 runes, 6000 units
	out := truncateTo(in, maxMessageUnits)
	if n := unitLen(out); n > maxMessageUnits || n < maxMessageUnits-2 {
		t.Fatalf("units %d", n)
	}
	if !utf8.ValidString(out) || !strings.HasSuffix(out, "…") {
		t.Fatal("bad truncation")
	}
	// 2000 emoji = 4000 units fit exactly; 2001 do not.
	if truncateTo(strings.Repeat("😀", 2000), maxMessageUnits) != strings.Repeat("😀", 2000) {
		t.Fatal("4000 units must fit")
	}
	if len([]rune(truncateTo(strings.Repeat("😀", 2001), maxMessageUnits))) > 2000 {
		t.Fatal("4002 units must be cut")
	}
}

func TestTruncatePathologicalTagNesting(t *testing.T) {
	out := truncateTo(strings.Repeat("<b>", 5000), maxMessageUnits)
	if unitLen(out) > maxMessageUnits {
		t.Fatalf("units %d", unitLen(out))
	}
}

func TestTruncateLinksAndNestedPre(t *testing.T) {
	// Cut inside the href attribute: the whole tag fragment goes away.
	in := strings.Repeat("x", 3980) + `<a href="https://example.com/path?a=1&amp;b=2">link text</a>` + strings.Repeat("y", 100)
	out := truncateTo(in, maxMessageUnits)
	if strings.ContainsAny(out, "<&") || unitLen(out) > maxMessageUnits {
		t.Fatalf("bad cut: ...%q", out[len(out)-30:])
	}
	// Cut after the opening tag: the link is closed.
	in = `<a href="https://example.com/">` + strings.Repeat("x", 5000) + `</a>`
	if out = truncateTo(in, maxMessageUnits); !strings.HasSuffix(out, "…</a>") || unitLen(out) > maxMessageUnits {
		t.Fatalf("link not closed: ...%q", out[len(out)-30:])
	}
	in = "<pre><code>" + strings.Repeat("z", 5000) + "</code></pre>"
	if out = truncateTo(in, maxMessageUnits); !strings.HasSuffix(out, "…</code></pre>") || unitLen(out) > maxMessageUnits {
		t.Fatalf("pre not closed: ...%q", out[len(out)-30:])
	}
}
