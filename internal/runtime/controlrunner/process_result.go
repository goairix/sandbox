package controlrunner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
)

type monitorStart struct {
	Request             controlprotocol.ExecutionRequest
	AuthorityDeadlineNS int64
	CommandDeadlineNS   int64
	StdinLength         int
}
type monitorCompletion struct {
	RootPID        int    `json:"root_pid"`
	RootWaitStatus uint32 `json:"root_wait_status"`
	DrainConfirmed bool   `json:"drain_confirmed"`
	Reason         string `json:"reason"`
}

func completionFromRoot(o launcher.RootExitObservation) monitorCompletion {
	return monitorCompletion{RootPID: o.RootPID, RootWaitStatus: o.RootWaitStatus, DrainConfirmed: o.RootPID > 1 && o.Drain.MonitorPID > 1, Reason: o.Reason}
}
func decodeMonitorJSON(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing monitor JSON")
	}
	return nil
}
func writeMonitorRequest(w io.Writer, r monitorStart) error {
	input := r.Request.Stdin
	r.Request.Stdin = nil
	r.StdinLength = len(input)
	if r.StdinLength > 1048576 {
		return fmt.Errorf("stdin exceeds bound")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err = writeMonitorFrame(w, monitorRequest, data); err != nil {
		return err
	}
	for len(input) > 0 {
		n := min(len(input), 32768)
		if err = writeMonitorFrame(w, monitorStdin, input[:n]); err != nil {
			return err
		}
		input = input[n:]
	}
	return writeMonitorFrame(w, monitorInputEnd, nil)
}
func readMonitorRequest(r io.Reader) (monitorStart, error) {
	var start monitorStart
	kind, data, err := readMonitorFrame(r)
	if err != nil {
		return start, err
	}
	if kind != monitorRequest {
		return start, fmt.Errorf("missing monitor request")
	}
	if err = decodeMonitorJSON(data, &start); err != nil {
		return start, err
	}
	if len(start.Request.Stdin) != 0 || start.StdinLength < 0 || start.StdinLength > 1048576 {
		return start, fmt.Errorf("invalid stdin declaration")
	}
	start.Request.Stdin = make([]byte, 0, start.StdinLength)
	for {
		kind, data, err = readMonitorFrame(r)
		if err != nil {
			return start, err
		}
		if kind == monitorInputEnd {
			if len(start.Request.Stdin) != start.StdinLength {
				return start, fmt.Errorf("stdin length mismatch")
			}
			break
		}
		if kind != monitorStdin || len(data) == 0 || len(data) > start.StdinLength-len(start.Request.Stdin) {
			return start, fmt.Errorf("invalid stdin frame")
		}
		start.Request.Stdin = append(start.Request.Stdin, data...)
	}
	if _, err = controlprotocol.NewExecutionDescriptor(start.Request); err != nil {
		return start, err
	}
	return start, nil
}

type monitorFD struct {
	FD     int    `json:"fd"`
	Target string `json:"target"`
	Flags  int    `json:"flags"`
}

type monitorReady struct {
	FDs           []monitorFD `json:"fds"`
	Completed     bool        `json:"completed"`
	RootPID       int         `json:"root_pid"`
	MonitorPID    int         `json:"monitor_pid"`
	FDCount       int         `json:"fd_count"`
	Status        string      `json:"status"`
	Cgroup        string      `json:"cgroup"`
	MemoryCurrent string      `json:"memory_current"`
	MemoryPeak    string      `json:"memory_peak"`
	PidsCurrent   string      `json:"pids_current"`
	ElapsedNS     int64       `json:"elapsed_ns"`
}
