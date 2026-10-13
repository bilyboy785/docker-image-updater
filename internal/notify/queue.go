package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// defaultMinInterval is the minimum spacing between two requests
	// (Telegram allows about one message per second per chat).
	defaultMinInterval = 1200 * time.Millisecond
	// maxMessageRunes is the size cap of one request, in runes (Telegram's
	// limit is 4096).
	maxMessageRunes = 4000
	// maxPending is the backlog bound; the oldest messages are dropped beyond it.
	maxPending = 200
	// maxAttempts is the number of tries for a batch failing with a temporary error.
	maxAttempts = 5
	// maxRateLimitRetries bounds the retries of a batch that keeps being rate limited.
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
		q.mu.Lock()
		last := q.last
		q.mu.Unlock()
		if !last.IsZero() {
			if wait := defaultMinInterval - q.now().Sub(last); wait > 0 && !q.sleep(q.ctx, wait) {
				return
			}
		}
		text, n := q.takeBatch()
		if n == 0 {
			continue
		}
		if !q.deliver(text, n) {
			return
		}
	}
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
// returns them joined by a blank line, preceded by the overflow notice if any.
func (q *Queue) takeBatch() (string, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var parts []string
	size := 0
	add := func(s string) {
		if len(parts) > 0 {
			size += 2
		}
		size += utf8.RuneCountInString(s)
		parts = append(parts, s)
	}
	if q.dropped > 0 && len(q.pending) > 0 {
		add(fmt.Sprintf("⚠️ %d earlier notifications were dropped (backlog limit)", q.dropped))
		q.dropped = 0
		q.overflowLogged = false
	}
	taken := 0
	for _, m := range q.pending {
		m = truncateMessage(m)
		if len(parts) > 0 && size+2+utf8.RuneCountInString(m) > maxMessageRunes {
			break
		}
		add(m)
		taken++
	}
	q.pending = append([]string(nil), q.pending[taken:]...)
	return strings.Join(parts, "\n\n"), taken
}

// deliver sends one batch, retrying according to the error. It returns false
// when the queue is shutting down hard (context ended).
func (q *Queue) deliver(text string, n int) bool {
	attempts, rateLimited := 0, 0
	for {
		err := q.inner.Notify(q.ctx, text)
		q.mu.Lock()
		q.last = q.now()
		q.mu.Unlock()
		if err == nil {
			return true
		}
		if q.ctx.Err() != nil {
			q.log.Warn("notification abandoned at shutdown", "messages", n)
			return false
		}
		var rl *RateLimitError
		var wait time.Duration
		switch {
		case errors.As(err, &rl):
			rateLimited++
			if rateLimited > maxRateLimitRetries {
				q.log.Warn("notification dropped, still rate limited", "messages", n, "error", err)
				return true
			}
			wait = rl.RetryAfter + rateLimitMargin
			q.log.Warn("telegram rate limit, waiting before retry", "wait", wait.String())
		case !IsRetryable(err):
			q.log.Warn("notification dropped, not retryable", "messages", n, "error", err)
			return true
		default:
			attempts++
			if attempts >= maxAttempts {
				q.log.Warn("notification dropped after repeated failures", "messages", n, "attempts", attempts, "error", err)
				return true
			}
			wait = retryBackoff[attempts-1]
			q.log.Warn("notification failed, will retry", "attempt", attempts, "wait", wait.String(), "error", err)
		}
		if !q.sleep(q.ctx, wait) {
			q.log.Warn("notification abandoned at shutdown", "messages", n)
			return false
		}
	}
}

var tagRe = regexp.MustCompile(`<(/?)([a-zA-Z][a-zA-Z0-9-]*)[^>]*>`)

// truncateMessage shortens s to maxMessageRunes runes. When it cuts, it ends
// with "…" and keeps the HTML valid: a trailing partial tag or entity is
// removed and the tags still open are closed.
func truncateMessage(s string) string {
	if utf8.RuneCountInString(s) <= maxMessageRunes {
		return s
	}
	runes := []rune(s)
	cut := maxMessageRunes - 1
	for {
		prefix := trimPartial(string(runes[:cut]))
		out := prefix + "…" + closingTags(prefix)
		if over := utf8.RuneCountInString(out) - maxMessageRunes; over > 0 {
			cut -= over
			continue
		}
		return out
	}
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
