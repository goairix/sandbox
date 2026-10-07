package controlrunner

import (
	"testing"
	"time"
)

func TestTaskQuiesceShutdownDeadlineEarlier(t *testing.T) {
	expired := make(chan struct{})
	d := newIsolationDeadline(time.Now().Add(time.Hour), func() { close(expired) })
	d.shorten(time.Now().Add(20 * time.Millisecond))
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("earlier deadline ignored")
	}
	<-d.done
	if d.succeed() {
		t.Fatal("expired owner revived")
	}
}
func TestTaskQuiesceShutdownDeadlineJoin(t *testing.T) {
	expired := make(chan struct{}, 1)
	d := newIsolationDeadline(time.Now().Add(time.Second), func() { expired <- struct{}{} })
	if !d.succeed() {
		t.Fatal("healthy owner could not finish")
	}
	select {
	case <-d.done:
	default:
		t.Fatal("success exposed before timer owner joined")
	}
	d.shorten(time.Now().Add(-time.Second))
	select {
	case <-expired:
		t.Fatal("stopped owner called callback")
	default:
	}
}
func TestTaskQuiesceShutdownDeadlineCallbackJoin(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	d := newIsolationDeadline(time.Now().Add(-time.Second), func() { close(entered); <-release })
	defer close(release)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("deadline never entered")
	}
	if d.succeed() {
		t.Fatal("success during callback")
	}
	select {
	case <-d.done:
		t.Fatal("callback reported joined early")
	default:
	}
}
