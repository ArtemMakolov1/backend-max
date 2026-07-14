package api

import (
	"sync"
	"time"
)

type rateWindow struct {
	startedAt time.Time
	count     int
}

// keyedWindowLimiter limits one client independently and keeps a much higher
// process-wide ceiling for distributed abuse. Rejected requests for one client
// do not consume the global budget, avoiding a cheap login DoS for everyone.
type keyedWindowLimiter struct {
	mu          sync.Mutex
	perKeyLimit int
	globalLimit int
	maxKeys     int
	window      time.Duration
	global      rateWindow
	perKey      map[string]rateWindow
}

func newKeyedWindowLimiter(perKeyLimit, globalLimit int, window time.Duration, maxKeys int) *keyedWindowLimiter {
	return &keyedWindowLimiter{
		perKeyLimit: perKeyLimit,
		globalLimit: globalLimit,
		maxKeys:     maxKeys,
		window:      window,
		perKey:      make(map[string]rateWindow),
	}
}

func (l *keyedWindowLimiter) Allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.global = l.currentWindow(l.global, now)
	if l.global.count >= l.globalLimit {
		return false, l.retryAfter(l.global, now)
	}

	var clientWindow rateWindow
	tracked := false
	if l.perKeyLimit > 0 {
		clientWindow, tracked = l.perKey[key]
		if tracked {
			clientWindow = l.currentWindow(clientWindow, now)
			if clientWindow.count >= l.perKeyLimit {
				l.perKey[key] = clientWindow
				return false, l.retryAfter(clientWindow, now)
			}
		} else {
			l.pruneExpired(now)
			if len(l.perKey) < l.maxKeys {
				clientWindow = rateWindow{startedAt: now}
				tracked = true
			}
		}
	}

	l.global.count++
	if tracked {
		clientWindow.count++
		l.perKey[key] = clientWindow
	}
	return true, 0
}

func (l *keyedWindowLimiter) currentWindow(window rateWindow, now time.Time) rateWindow {
	if window.startedAt.IsZero() || now.Before(window.startedAt) || now.Sub(window.startedAt) >= l.window {
		return rateWindow{startedAt: now}
	}
	return window
}

func (l *keyedWindowLimiter) retryAfter(window rateWindow, now time.Time) time.Duration {
	retryAfter := l.window - now.Sub(window.startedAt)
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	return retryAfter.Round(time.Second)
}

func (l *keyedWindowLimiter) pruneExpired(now time.Time) {
	if len(l.perKey) < l.maxKeys {
		return
	}
	for key, window := range l.perKey {
		if now.Before(window.startedAt) || now.Sub(window.startedAt) >= l.window {
			delete(l.perKey, key)
		}
	}
}
