package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

func TestDockerControlAllowsOnlyFixedMounterGrammar(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	_, err = rt.execControl(context.Background(), info.RuntimeID, []string{"sh", "-c", "id"}, nil)
	require.ErrorIs(t, err, ErrInvalidControlCommand)
	_, err = rt.execControl(context.Background(), info.RuntimeID, []string{fuseprotocol.MounterBinary, "health", "prepared", "extra"}, nil)
	require.ErrorIs(t, err, ErrInvalidControlCommand)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, call := range fake.execs {
		if len(call.options.Cmd) > 0 && call.options.Cmd[0] == fuseprotocol.MounterBinary {
			assert.Equal(t, "root", call.options.User)
			assert.False(t, call.options.Privileged)
		}
	}
}

func TestDockerControlRejectsOversizedInputBeforeExec(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	before := fake.nextExec
	_, err := rt.execControl(context.Background(), "container", []string{fuseprotocol.MounterBinary, "authorize"}, []byte(strings.Repeat("x", fuseprotocol.MaxJSONBytes+1)))
	require.Error(t, err)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Equal(t, before, fake.nextExec)
}

func TestDockerControlAttachClosesOnContextCancellation(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	fake.mu.Lock()
	fake.blockControlOutput = true
	fake.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := rt.execControl(ctx, "container", []string{fuseprotocol.MounterBinary, "health", "prepared"}, nil)
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("Docker control attach did not close after context cancellation")
	}
}
