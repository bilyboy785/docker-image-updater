package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

const (
	// defaultMinInterval is the minimum spacing between two requests
	// (Telegram allows about one message per second per chat).
	defaultMinInterval = 1200 * time.Millisecond
	// maxMessageUnits is the size cap of one request, in UTF-16 code units
	// (Telegram's limit is 4096 and counts non-BMP characters such as emoji
	// twice).
	maxMessageUnits = 4000
	// maxPending is the backlog bound; the oldest messages are dropped beyond it.
	maxPending = 200
	// maxAttempts is the number of tries for a batch failing with a temporary error.
	maxAttempts = 5
	// maxRateLimitRetries bounds the retries of a batch that keeps being rate
	// limited. Worst case, a batch waits 20 x (10 min + 1 s), about 3h20, before
	// being dropped; the backlog stays bounded meanwhile (oldest dropped first).
	maxRateLimitRetries = 20
	// rateLimitMargin is added to Telegram's retry_after.
	rateLimitMargin = time.Second
)

// retryBackoff is the wait after the n-th failed attempt (temporary errors).
var retryBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// Queue is a Notifier that sends in the background through an inner Notifier,
// one request at a time, rate limited, coalescing bursts and retrying
// temporary failures. Notify never blocks on the network.
type Queue struct {
	inner Notifier
	log   *slog.Logger
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) bool // false when ctx ended first

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{} // closed when the worker exits

	mu             sync.Mutex
	pending        []string
	dropped        int // messages dropped by the backlog bound, not yet announced
	overflowLogged bool
	closed         bool
	last           time.Time // end of the last request
}

// NewQueue starts the worker goroutine. Call Close to stop it.
func NewQueue(inner Notifier, log *slog.Logger) *Queue {
	return newQueue(inner, log, time.Now, realSleep)
}

func realSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func newQueue(inner Notifier, log *slog.Logger, now func() time.Time, sleep func(context.Context, time.Duration) bool) *Queue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{
		inner: inner, log: log, now: now, sleep: sleep,
		ctx: ctx, cancel: cancel,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
	go q.run()
	return q
}

// Notify enqueues text and returns immediately; ctx is ignored. After Close it
// does nothing and returns nil. When the backlog is full the oldest pending
// message is dropped.
func (q *Queue) Notify(_ context.Context, text string) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil
	}
	q.pending = append(q.pending, text)
	if over := len(q.pending) - maxPending; over > 0 {
		q.pending = append([]string(nil), q.pending[over:]...)
		q.dropped += over
		if !q.overflowLogged {
			q.overflowLogged = true
			q.log.Warn("notification backlog full, dropping oldest messages", "limit", maxPending)
		}
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return nil
}

// Close stops accepting messages, tries to send what is pending for at most
// timeout, then stops the worker. It is safe to call more than once.
func (q *Queue) Close(timeout time.Duration) {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-q.done:
		return
	case <-t.C:
	}
	q.cancel() // aborts the in-flight request and any wait
	<-q.done
	q.mu.Lock()
	n := len(q.pending)
	q.mu.Unlock()
	if n > 0 {
		q.log.Warn("notification queue closed with undelivered messages", "pending", n)
	}
}

func (q *Queue) run() {
	defer close(q.done)
	defer q.cancel()
	for {
		if !q.waitPending() {
			return
		}
		// Messages arriving during this wait are merged into the batch.
		if !q.space() {
			return
		}
		parts := q.takeBatch()
		if len(parts) == 0 {
			continue
		}
		if !q.deliver(parts) {
			return
		}
	}
}

// space waits until the minimum interval since the last request has elapsed.
// It returns false when the context ended first.
func (q *Queue) space() bool {
	q.mu.Lock()
	last := q.last
	q.mu.Unlock()
	if last.IsZero() {
		return true
	}
	if wait := defaultMinInterval - q.now().Sub(last); wait > 0 {
		return q.sleep(q.ctx, wait)
	}
	return true
}

// waitPending blocks until a message is pending. It returns false when the
// worker must exit (shutdown requested and nothing left, or context ended).
func (q *Queue) waitPending() bool {
	for {
		q.mu.Lock()
		if len(q.pending) > 0 {
			q.mu.Unlock()
			return true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return false
		}
		select {
		case <-q.wake:
		case <-q.ctx.Done():
			return false
		}
	}
}

// takeBatch removes from the backlog the messages that fit in one request and
// returns them as parts (the overflow notice first, if any). The first message
// is always taken, truncated if needed so that it fits next to the notice, so
// the notice is always part of a batch that is actually sent.
func (q *Queue) takeBatch() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return nil
	}
	var parts []string
	size := 0
	if q.dropped > 0 {
		notice := fmt.Sprintf("⚠️ %d earlier notifications were dropped (backlog limit)", q.dropped)
		parts = append(parts, notice)
		size = unitLen(notice)
		q.dropped = 0
		q.overflowLogged = false
	}
	taken := 0
	for i, m := range q.pending {
		if i == 0 {
			room := maxMessageUnits
			if size > 0 {
				room -= size + 2
			}
			m = truncateTo(m, room)
		} else {
			m = truncateTo(m, maxMessageUnits)
			if size+2+unitLen(m) > maxMessageUnits {
				break
			}
		}
		if size > 0 {
			size += 2
		}
		size += unitLen(m)
		parts = append(parts, m)
		taken++
	}
	q.pending = append([]string(nil), q.pending[taken:]...)
	return parts
}

// deliver sends one batch (parts joined by a blank line). If the merged text is
// rejected with HTTP 400 the parts are sent one by one, so that a single
// invalid message does not take the others down. It returns false when the
// queue is shutting down hard (context ended).
func (q *Queue) deliver(parts []string) bool {
	alive, split := q.send(strings.Join(parts, "\n\n"), len(parts), len(parts) > 1)
	if !alive || !split {
		return alive
	}
	q.log.Warn("merged notification rejected, sending messages one by one", "messages", len(parts))
	for _, p := range parts {
		if !q.space() {
			return false
		}
		if alive, _ := q.send(p, 1, false); !alive {
			return false
		}
	}
	return true
}

// callInner calls the inner notifier; a panic becomes an error (only the panic
// value's type is reported, never message text).
func (q *Queue) callInner(text string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("notifier panicked (%T)", r)
		}
	}()
	return q.inner.Notify(q.ctx, text)
}

// send performs the attempts for one request. alive is false on hard shutdown;
// split is true when the caller should retry the parts individually.
func (q *Queue) send(text string, n int, canSplit bool) (alive, split bool) {
	attempts, rateLimited := 0, 0
	for {
		err := q.callInner(text)
		q.mu.Lock()
		q.last = q.now()
		q.mu.Unlock()
		if err == nil {
			return true, false
		}
		if q.ctx.Err() != nil {
			q.log.Warn("notification abandoned at shutdown", "messages", n)
			return false, false
		}
		var rl *RateLimitError
		var se *StatusError
		var wait time.Duration
		switch {
		case errors.As(err, &rl):
			rateLimited++
			if rateLimited > maxRateLimitRetries {
				q.log.Warn("notification dropped, still rate limited", "messages", n, "error", err)
				return true, false
			}
			wait = rl.RetryAfter + rateLimitMargin
			q.log.Warn("telegram rate limit, waiting before retry", "wait", wait.String())
		case !IsRetryable(err):
			if canSplit && errors.As(err, &se) && se.Code == http.StatusBadRequest {
				return true, true
			}
			q.log.Warn("notification dropped, not retryable", "messages", n, "error", err)
			return true, false
		default:
			attempts++
			if attempts >= maxAttempts {
				q.log.Warn("notification dropped after repeated failures", "messages", n, "attempts", attempts, "error", err)
				return true, false
			}
			wait = retryBackoff[attempts-1]
			q.log.Warn("notification failed, will retry", "attempt", attempts, "wait", wait.String(), "error", err)
		}
		if !q.sleep(q.ctx, wait) {
			q.log.Warn("notification abandoned at shutdown", "messages", n)
			return false, false
		}
	}
}

var tagRe = regexp.MustCompile(`<(/?)([a-zA-Z][a-zA-Z0-9-]*)[^>]*>`)

// unitLen is the length of s in UTF-16 code units, the unit of Telegram's limit.
func unitLen(s string) int {
	n := 0
	for _, r := range s {
		if l := utf16.RuneLen(r); l > 0 {
			n += l
		} else {
			n++
		}
	}
	return n
}

// truncateTo shortens s to at most limit UTF-16 code units, cutting on a rune
// boundary. When it cuts, it ends with "…" and keeps the HTML valid: a trailing
// partial tag or entity is removed and the tags still open are closed.
func truncateTo(s string, limit int) string {
	if unitLen(s) <= limit {
		return s
	}
	if limit < 1 {
		return "…"
	}
	runes := []rune(s)
	cut, units := 0, 0
	for cut < len(runes) {
		l := utf16.RuneLen(runes[cut])
		if l < 1 {
			l = 1
		}
		if units+l > limit-1 {
			break
		}
		units += l
		cut++
	}
	for cut > 0 {
		prefix := trimPartial(string(runes[:cut]))
		out := prefix + "…" + closingTags(prefix)
		over := unitLen(out) - limit
		if over <= 0 {
			return out
		}
		cut -= over
	}
	return "…"
}

func trimPartial(p string) string {
	if i := strings.LastIndexByte(p, '<'); i >= 0 && !strings.Contains(p[i:], ">") {
		p = p[:i]
	}
	if i := strings.LastIndexByte(p, '&'); i >= 0 && !strings.Contains(p[i:], ";") {
		p = p[:i]
	}
	return p
}

func closingTags(p string) string {
	var stack []string
	for _, m := range tagRe.FindAllStringSubmatch(p, -1) {
		name := strings.ToLower(m[2])
		if m[1] == "" {
			stack = append(stack, name)
			continue
		}
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i] == name {
				stack = stack[:i]
				break
			}
		}
	}
	var b strings.Builder
	for i := len(stack) - 1; i >= 0; i-- {
		b.WriteString("</" + stack[i] + ">")
	}
	return b.String()
}
