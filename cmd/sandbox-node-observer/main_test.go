package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeObserverConfigStrict(t *testing.T) {
	c := config{Version: 1, ClockSocket: "/run/observer/clock.sock", Kubeconfig: "/etc/observer/kubeconfig", JournalDirectory: "/var/lib/observer", EnrollmentSocket: "/run/observer/enroll.sock", RequestTimeoutMillis: 5000, TerminationTimeoutMillis: 30000}
	wire, err := json.Marshal(c)
	require.NoError(t, err)
	_, err = decodeConfig(wire)
	require.NoError(t, err)
	_, err = decodeConfig(append(wire, ' '))
	require.Error(t, err)
	c.RequestTimeoutMillis = 5001
	wire, err = json.Marshal(c)
	require.NoError(t, err)
	_, err = decodeConfig(wire)
	require.Error(t, err)
}
