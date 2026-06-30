package agenthttp

import (
	"context"
	"sync"
)

// sessionLockStore 按 sessionId 维护互斥锁，保证同一个 session 的并发请求串行执行。
type sessionLockStore struct {
	// mu 保护 locks map 的并发读写。
	mu sync.Mutex
	// locks 保存每个 sessionId 对应的通道信号量。
	locks map[string]*sessionLock
}

// sessionLock 是容量为 1 的 channel，用作单 session 互斥信号量。
type sessionLock struct {
	// token 容量为 1，获取锁 = 从 channel 读取，释放 = 写回。
	token chan struct{}
}

// newSessionLockStore 创建空的 session 锁存储。
func newSessionLockStore() *sessionLockStore {
	return &sessionLockStore{
		locks: make(map[string]*sessionLock),
	}
}

// acquire 获取 session 锁，返回的 release 函数必须在调用方退出前执行。
// ctx 超时或取消时返回错误，不会持有锁。
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
