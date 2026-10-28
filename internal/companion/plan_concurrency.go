package companion

import "sync"

// planConcurrencyLimiter tracks requests currently being served by each
// upstream credential across plan time-window handoffs. A zero limit means
// that credential is unlimited.
type planConcurrencyLimiter struct {
	mu     sync.Mutex
	active map[string]int
}

func (l *planConcurrencyLimiter) tryAcquire(accountID string, limit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active == nil {
		l.active = make(map[string]int)
	}
	if limit > 0 && l.active[accountID] >= limit {
		return false
	}
	l.active[accountID]++
	return true
}

func (l *planConcurrencyLimiter) release(accountID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if active := l.active[accountID]; active <= 1 {
		delete(l.active, accountID)
	} else {
		l.active[accountID] = active - 1
	}
}
