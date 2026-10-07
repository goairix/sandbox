package controlrunner

import (
	"sync"
	"time"
)

// isolationDeadline owns one timer and callback. Its lock protects scalar
// arbitration only; expiry never acquires a supervisor, execution or journal lock.
// Deadlines retain Go's monotonic component. They may only become earlier.
type isolationDeadline struct {
	mu       sync.Mutex
	deadline time.Time
	state    uint8 // 0 armed, 1 expired, 2 success
	wake     chan struct{}
	done     chan struct{}
	expire   func()
}

func newIsolationDeadline(at time.Time, expire func()) *isolationDeadline {
	d := &isolationDeadline{deadline: at, wake: make(chan struct{}, 1), done: make(chan struct{}), expire: expire}
	go d.run()
	return d
}
func (d *isolationDeadline) shorten(at time.Time) {
	d.mu.Lock()
	if d.state == 0 && at.Before(d.deadline) {
		d.deadline = at
		select {
		case d.wake <- struct{}{}:
		default:
		}
	}
	d.mu.Unlock()
}
func (d *isolationDeadline) run() {
	defer close(d.done)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		d.mu.Lock()
		if d.state != 0 {
			d.mu.Unlock()
			return
		}
		remaining := time.Until(d.deadline)
		if remaining <= 0 {
			d.state = 1
			d.mu.Unlock()
			d.expire()
			return
		}
		d.mu.Unlock()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(remaining)
		select {
		case <-timer.C:
		case <-d.wake:
		}
	}
}

// succeed stops and joins the original owner. A callback that won, or a join
// that did not complete inside the absolute deadline, cannot report success.
func (d *isolationDeadline) succeed() bool {
	d.mu.Lock()
	if d.state != 0 || !time.Now().Before(d.deadline) {
		d.mu.Unlock()
		return false
	}
	d.state = 2
	at := d.deadline
	select {
	case d.wake <- struct{}{}:
	default:
	}
	d.mu.Unlock()
	<-d.done
	return time.Now().Before(at)
}
