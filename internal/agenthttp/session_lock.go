package agenthttp

import (
	"context"
	"sync"
)

type sessionLockStore struct {
	mu    sync.Mutex
	locks map[string]*sessionLock
}

type sessionLock struct {
	token chan struct{}
}

func newSessionLockStore() *sessionLockStore {
	return &sessionLockStore{
		locks: make(map[string]*sessionLock),
	}
}

func (s *sessionLockStore) acquire(ctx context.Context, sessionID string) (func(), error) {
	lock := s.lockFor(sessionID)
	select {
	case <-lock.token:
		return func() {
			lock.token <- struct{}{}
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// lockFor 返回 session 级锁。锁对象当前不做回收，因为 session 数量通常很小；
// 如果后续出现大量短生命周期 session，再和 session 清理策略一起加回收。
func (s *sessionLockStore) lockFor(sessionID string) *sessionLock {
	s.mu.Lock()
	defer s.mu.Unlock()

	lock := s.locks[sessionID]
	if lock == nil {
		lock = &sessionLock{token: make(chan struct{}, 1)}
		lock.token <- struct{}{}
		s.locks[sessionID] = lock
	}
	return lock
}
