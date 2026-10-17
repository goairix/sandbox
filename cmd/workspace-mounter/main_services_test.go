package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/mounter"
	"github.com/stretchr/testify/require"
)

func TestMounterServicesCancelAndJoinOriginalWork(t *testing.T) {
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	want := errors.New("readiness server failed")
	joined := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	var released sync.Once
	consumed := false
	defer func() {
		cancel()
		released.Do(func() { close(release) })
		if !consumed {
			select {
			case <-joined:
			case <-time.After(time.Second):
				t.Error("service group cleanup did not join")
			}
		}
	}()
	go func() {
		joined <- serveMounterServices(ctx,
			func(context.Context) error {
				<-started
				return want
			},
			func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				close(canceled)
				<-release
				return nil
			},
		)
	}()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("server failure did not cancel siblings")
	}
	select {
	case <-joined:
		consumed = true
		t.Fatal("service group returned before original work exited")
	case <-time.After(30 * time.Millisecond):
	}
	released.Do(func() { close(release) })
	result := <-joined
	consumed = true
	require.ErrorIs(t, result, want)
}

func TestMounterServicesRetainEveryServerError(t *testing.T) {
	started := make(chan struct{})
	first, second := errors.New("HTTP failed"), errors.New("observer close uncertain")
	err := serveMounterServices(context.Background(),
		func(context.Context) error {
			<-started
			return first
		},
		func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return second
		},
	)
	require.ErrorIs(t, err, first)
	require.ErrorIs(t, err, second)
}

func TestMounterServicesReadinessListenFailureCancelsControl(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()
	server := &mounter.ReadinessServer{
		Supervisor: mounter.NewSupervisor(mounter.Config{}, nil),
		Address:    occupied.Addr().String(),
	}
	canceled := make(chan struct{})
	err = serveMounterServices(context.Background(), server.Serve, func(ctx context.Context) error {
		<-ctx.Done()
		close(canceled)
		return nil
	})
	require.Error(t, err)
	select {
	case <-canceled:
	default:
		t.Fatal("readiness startup failure left control serving")
	}
}
