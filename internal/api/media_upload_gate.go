package api

import (
	"errors"
	"sync"
)

var errMediaUploadRateLimited = errors.New("media upload concurrency limit reached")

// mediaUploadGate bounds multipart bodies before the server starts reading
// them. The users map cannot grow beyond the global channel capacity because
// a global slot is acquired before a user entry is inserted.
type mediaUploadGate struct {
	global chan struct{}
	mu     sync.Mutex
	users  map[string]struct{}
}

func newMediaUploadGate(globalLimit int) *mediaUploadGate {
	if globalLimit <= 0 {
		globalLimit = 1
	}
	return &mediaUploadGate{
		global: make(chan struct{}, globalLimit),
		users:  make(map[string]struct{}, globalLimit),
	}
}

func (g *mediaUploadGate) tryAcquire(userID string) (func(), bool) {
	select {
	case g.global <- struct{}{}:
	default:
		return nil, false
	}

	g.mu.Lock()
	if _, exists := g.users[userID]; exists {
		g.mu.Unlock()
		<-g.global
		return nil, false
	}
	g.users[userID] = struct{}{}
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.users, userID)
			g.mu.Unlock()
			<-g.global
		})
	}, true
}
