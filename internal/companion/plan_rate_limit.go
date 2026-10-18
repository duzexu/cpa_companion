package companion

import (
	"sync"
	"time"
)

// planRateLimiter counts admitted API requests in a rolling 60-second window.
// The companion runs as one Docker service, so all requests share this gate.
type planRateLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time
}

func (l *planRateLimiter) allow(planID string, rpm int, now time.Time) (bool, int) {
	if rpm == 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.requests == nil {
		l.requests = make(map[string][]time.Time)
	}
	times := l.requests[planID]
	cutoff := now.Add(-time.Minute)
	first := 0
	for first < len(times) && !times[first].After(cutoff) {
		first++
	}
	if first > 0 {
		copy(times, times[first:])
		times = times[:len(times)-first]
	}
	if len(times) >= rpm {
		l.requests[planID] = times
		retry := times[0].Add(time.Minute).Sub(now)
		seconds := int((retry + time.Second - 1) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		return false, seconds
	}
	l.requests[planID] = append(times, now)
	return true, 0
}

func (l *planRateLimiter) forget(planID string) {
	l.mu.Lock()
	delete(l.requests, planID)
	l.mu.Unlock()
}
