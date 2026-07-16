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
	global    chan struct{}
	userLimit int
	mu        sync.Mutex
	users     map[string]int
}

func newMediaUploadGate(globalLimit, userLimit int) *mediaUploadGate {
	if globalLimit <= 0 {
		globalLimit = 1
	}
	if userLimit <= 0 {
		userLimit = 1
	}
	if userLimit > globalLimit {
		userLimit = globalLimit
	}
	return &mediaUploadGate{
		global: make(chan struct{}, globalLimit), userLimit: userLimit,
		users: make(map[string]int, globalLimit),
	}
}

func (g *mediaUploadGate) tryAcquire(userID string) (func(), bool) {
	select {
	case g.global <- struct{}{}:
	default:
		return nil, false
	}

	g.mu.Lock()
	if g.users[userID] >= g.userLimit {
		g.mu.Unlock()
		<-g.global
		return nil, false
	}
	g.users[userID]++
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.users[userID] <= 1 {
				delete(g.users, userID)
			} else {
				g.users[userID]--
			}
			g.mu.Unlock()
			<-g.global
		})
	}, true
}
