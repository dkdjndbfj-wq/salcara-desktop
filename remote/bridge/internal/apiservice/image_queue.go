package apiservice

import (
	"context"
	"errors"
	"sync"
)

var errImageQueueFull = errors.New("image queue full")

const maxQueuedImages = 64

type imageWaiter struct {
	done       chan struct{}
	candidates []string
	granted    bool
	key        string
}
type imageBudget struct {
	inflight int
	limit    int
}

// imageQueue adds a separate queue and image limit to ordinary admission. Keys
// are credential identities, so duplicated cards/clients cannot multiply an
// upstream account's image capacity. The global waiter order preserves FIFO
// whenever candidate sets overlap, while unrelated credentials can progress.
type imageQueue struct {
	mu      sync.Mutex
	budgets map[string]*imageBudget
	waiters []*imageWaiter
}

func (q *imageQueue) acquire(ctx context.Context, key string, limit int) (func(), error) {
	_, release, err := q.acquireAny(ctx, []string{key}, limit)
	return release, err
}

// acquireAny selects the candidate with the fewest active image jobs. Ties use
// the caller's explicit member order. It has no conversation/session affinity.
func (q *imageQueue) acquireAny(ctx context.Context, keys []string, limit int) (string, func(), error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	seen := make(map[string]struct{}, len(keys))
	candidates := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		candidates = append(candidates, key)
	}
	if len(candidates) == 0 {
		return "", nil, errImageQueueFull
	}
	if limit < 1 || limit > 16 {
		limit = 1
	}

	q.mu.Lock()
	if q.budgets == nil {
		q.budgets = make(map[string]*imageBudget)
	}
	for _, key := range candidates {
		b := q.budgets[key]
		if b == nil {
			q.budgets[key] = &imageBudget{limit: limit}
		} else if limit < b.limit {
			// An older captured request cannot raise a ceiling already tightened
			// by newer settings. Relaxation takes effect after complete drain.
			b.limit = limit
		}
	}
	if len(q.waiters) >= maxQueuedImages {
		q.pruneAllLocked()
		q.mu.Unlock()
		return "", nil, errImageQueueFull
	}
	w := &imageWaiter{done: make(chan struct{}), candidates: candidates}
	q.waiters = append(q.waiters, w)
	q.grantLocked()
	q.mu.Unlock()

	select {
	case <-ctx.Done():
		q.mu.Lock()
		if w.granted {
			if b := q.budgets[w.key]; b != nil && b.inflight > 0 {
				b.inflight--
			}
		} else {
			q.removeWaiterLocked(w)
		}
		q.grantLocked()
		q.pruneAllLocked()
		q.mu.Unlock()
		return "", nil, ctx.Err()
	case <-w.done:
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			q.mu.Lock()
			if b := q.budgets[w.key]; b != nil && b.inflight > 0 {
				b.inflight--
			}
			q.grantLocked()
			q.pruneAllLocked()
			q.mu.Unlock()
		})
	}
	if err := ctx.Err(); err != nil {
		release()
		return "", nil, err
	}
	return w.key, release, nil
}

func (q *imageQueue) removeWaiterLocked(target *imageWaiter) {
	for i, waiter := range q.waiters {
		if waiter == target {
			q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
			return
		}
	}
}

func (q *imageQueue) grantLocked() {
	for i := 0; i < len(q.waiters); {
		waiter := q.waiters[i]
		key := ""
		least := int(^uint(0) >> 1)
		for _, candidate := range waiter.candidates {
			budget := q.budgets[candidate]
			if budget == nil || budget.inflight >= budget.limit {
				continue
			}
			if budget.inflight < least {
				key, least = candidate, budget.inflight
			}
		}
		if key == "" {
			i++
			continue
		}
		q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
		waiter.key, waiter.granted = key, true
		q.budgets[key].inflight++
		close(waiter.done)
	}
}

func (q *imageQueue) pruneAllLocked() {
	queued := make(map[string]bool)
	for _, waiter := range q.waiters {
		for _, key := range waiter.candidates {
			queued[key] = true
		}
	}
	for key, budget := range q.budgets {
		if budget.inflight == 0 && !queued[key] {
			delete(q.budgets, key)
		}
	}
}

func (q *imageQueue) snapshot() (inflight, queued int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, b := range q.budgets {
		inflight += b.inflight
	}
	return inflight, len(q.waiters)
}
