package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
)

const maxSSEEventBytes = 1 << 20

// ErrMalformedStream means the server sent an invalid or oversized SSE event.
var ErrMalformedStream = errors.New("sandbox: malformed execution stream")

type streamResult struct {
	event SSEEvent
	err   error
}

type eventStream struct {
	items    chan streamResult
	body     io.ReadCloser
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
	closed   atomic.Bool
	terminal atomic.Bool
}

func newEventStream(ctx context.Context, cancel context.CancelFunc, body io.ReadCloser) *eventStream {
	stream := &eventStream{
		items: make(chan streamResult, 16), body: body, ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	go func() {
		defer close(stream.done)
		defer close(stream.items)
		defer body.Close()
		defer cancel()
		parseExecutionSSE(ctx, body, stream.items, &stream.terminal)
	}()
	return stream
}

func (s *eventStream) Recv() (SSEEvent, error) {
	item, ok := <-s.items
	if !ok {
		if !s.closed.Load() && !s.terminal.Load() && s.ctx.Err() != nil {
			return SSEEvent{}, s.ctx.Err()
		}
		return SSEEvent{}, io.EOF
	}
	return item.event, item.err
}

func (s *eventStream) Close() error {
	s.once.Do(func() {
		s.closed.Store(true)
		s.cancel()
		_ = s.body.Close()
	})
	<-s.done
	return nil
}

func parseExecutionSSE(ctx context.Context, body io.Reader, output chan<- streamResult, terminal *atomic.Bool) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), maxSSEEventBytes)
	var eventType string
	var data []string
	dataBytes := 0
	send := func(item streamResult) bool {
		select {
		case <-ctx.Done():
			return false
		case output <- item:
			return true
		}
	}
	fail := func(err error) {
		if send(streamResult{err: err}) {
			terminal.Store(true)
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if eventType == "" && dataBytes == 0 {
				continue
			}
			if eventType == "" || len(data) == 0 {
				fail(fmt.Errorf("%w: event type or data missing", ErrMalformedStream))
				return
			}
			event, err := decodeExecutionEvent(eventType, strings.Join(data, "\n"))
			if err != nil {
				fail(err)
				return
			}
			if !send(streamResult{event: event}) {
				return
			}
			if event.Type == SSEEventDone || event.Type == SSEEventError {
				terminal.Store(true)
				return
			}
			eventType, data, dataBytes = "", nil, 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE comment/heartbeat.
		}
		if raw, ok := strings.CutPrefix(line, "event:"); ok {
			eventType = strings.TrimSpace(raw)
		} else if raw, ok := strings.CutPrefix(line, "data:"); ok {
			dataBytes += len(raw)
			if dataBytes > maxSSEEventBytes {
				fail(fmt.Errorf("%w: event exceeds size limit", ErrMalformedStream))
				return
			}
			data = append(data, strings.TrimSpace(raw))
		}
	}
	if ctx.Err() != nil {
		return
	}
	if err := scanner.Err(); err != nil {
		fail(fmt.Errorf("%w: %v", ErrMalformedStream, err))
	} else {
		fail(io.ErrUnexpectedEOF)
	}
}

func decodeExecutionEvent(kind, data string) (SSEEvent, error) {
	malformed := func(err error) (SSEEvent, error) {
		return SSEEvent{}, fmt.Errorf("%w: invalid %s payload: %v", ErrMalformedStream, kind, err)
	}
	switch SSEEventType(kind) {
	case SSEEventStdout, SSEEventStderr:
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return malformed(err)
		}
		return SSEEvent{Type: SSEEventType(kind), Content: payload.Content}, nil
	case SSEEventDone:
		var payload struct {
			ExitCode *int    `json:"exit_code"`
			Elapsed  float64 `json:"elapsed"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return malformed(err)
		}
		if payload.ExitCode == nil {
			return malformed(errors.New("exit_code missing"))
		}
		return SSEEvent{Type: SSEEventDone, ExitCode: *payload.ExitCode, Elapsed: payload.Elapsed}, nil
	case SSEEventError:
		var payload struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return malformed(err)
		}
		return SSEEvent{Type: SSEEventError, Content: payload.Message}, nil
	case SSEEventPing:
		var payload struct {
			Timestamp int64 `json:"timestamp"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return malformed(err)
		}
		return SSEEvent{Type: SSEEventPing, Timestamp: payload.Timestamp}, nil
	default:
		return malformed(errors.New("unknown event type"))
	}
}
