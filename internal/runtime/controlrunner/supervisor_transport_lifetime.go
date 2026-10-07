package controlrunner

import (
	"context"
	"errors"
	"net"
	"sync"
)

// transportLifetime owns resource borrowers, not execution authority. Closing
// seals registration before stopping IO/executions and joining the borrowers.
// Callers supply only the owner's fixed shutdown and destruction operations.
type transportLifetime struct {
	listener net.Listener
	cancel   context.CancelFunc
	mu       sync.Mutex
	users    map[*transportBorrow]struct{}
	workers  sync.WaitGroup
	done     chan struct{}
	err      error
}
type transportBorrow struct {
	owner  *transportLifetime
	conn   net.Conn
	cancel context.CancelFunc
}

// One protected listener owns this Supervisor's transport. Registration is
// sealed by the same lock as Close, including direct concurrent public Close.
func (l *transportLifetime) listen(listener net.Listener, cancel context.CancelFunc) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done != nil || l.listener != nil {
		return false
	}
	l.listener, l.cancel = listener, cancel
	return true
}
func (l *transportLifetime) borrow(ctx context.Context, conn net.Conn) (*transportBorrow, context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done != nil {
		return nil, nil
	}
	if l.users == nil {
		l.users = make(map[*transportBorrow]struct{})
	}
	ctx, cancel := context.WithCancel(ctx)
	b := &transportBorrow{owner: l, conn: conn, cancel: cancel}
	l.users[b] = struct{}{}
	l.workers.Add(1)
	return b, ctx
}
func (b *transportBorrow) release() {
	b.cancel()
	b.owner.mu.Lock()
	delete(b.owner.users, b)
	b.owner.mu.Unlock()
	b.owner.workers.Done()
}
func (l *transportLifetime) close(stop func() (bool, error), destroy func() error) error {
	l.mu.Lock()
	if l.done != nil {
		done := l.done
		l.mu.Unlock()
		<-done
		return l.err
	}
	l.done = make(chan struct{})
	users := make([]*transportBorrow, 0, len(l.users))
	for b := range l.users {
		users = append(users, b)
	}
	listener, cancel := l.listener, l.cancel
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if listener != nil {
		_ = listener.Close()
	}
	for _, b := range users {
		b.cancel()
		_ = b.conn.Close()
	}
	joined, err := stop()
	if joined {
		// Stream borrowers can need the execution cancellation above to finish.
		l.workers.Wait()
		err = errors.Join(err, destroy())
	}
	l.err = err
	close(l.done)
	return err
}
