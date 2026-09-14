package redisbootstrap

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
)

const maximumLocalINFOBytes = 64 << 10

var errLocalObservation = errors.New("local Redis identity observation is unconfirmed")

// LocalObserverOptions fixes the trusted PVC and local Redis credentials.
type LocalObserverOptions struct {
	Directory  string
	Cluster    ClusterState
	Member     Member
	MasterName string
	Password   string
}

// NewLocalObserver constructs a read-only inventory/live observation provider.
// Construction does not open the PVC or connect Redis. Live uses only this
// Pod's Redis loopback endpoint, never Sentinel or a business request proxy.
func NewLocalObserver(options LocalObserverOptions) (ObservationProvider, error) {
	return newLocalObserver(options, ReadLocalVolume, func(ctx context.Context, password string) (string, error) {
		return readLocalRedisRunID(ctx, "127.0.0.1:6379", password)
	})
}

// These private dependencies separate retained-file and authenticated process
// reads for deterministic failure coverage. There is no exported endpoint seam.
func newLocalObserver(options LocalObserverOptions, read func(context.Context, string, ClusterState, Member, string) (LocalVolumeSnapshot, error), runID func(context.Context, string) (string, error)) (ObservationProvider, error) {
	if options.Member.Validate(options.Cluster) != nil || !filepath.IsAbs(options.Directory) || filepath.Clean(options.Directory) == string(filepath.Separator) || !safeMasterName(options.MasterName) || ValidateBuiltinSentinelPassword(options.Password) != nil || read == nil || runID == nil {
		return nil, errLocalObservation
	}
	return func(ctx context.Context, purpose ProofPurpose) (LocalObservation, error) {
		if err := ctx.Err(); err != nil {
			return LocalObservation{}, err
		}
		if !validPurpose(purpose) {
			return LocalObservation{}, errLocalObservation
		}
		before, err := read(ctx, options.Directory, options.Cluster, options.Member, options.MasterName)
		if err != nil {
			return LocalObservation{}, localObservationError(ctx)
		}
		observation := LocalObservation{Volume: before.Volume, Snapshot: before.Snapshot, ConfigDigest: before.ConfigDigest}
		if purpose == InventoryProof {
			if validateObservation(options.Cluster, options.Member, purpose, observation) != nil {
				return LocalObservation{}, errLocalObservation
			}
			if err := ctx.Err(); err != nil {
				return LocalObservation{}, err
			}
			return observation, nil
		}
		if before.Volume.Identity == nil || before.Volume.Identity.InitialConfig != Configured || before.Snapshot == nil || before.Volume.Persisted == nil {
			return LocalObservation{}, errLocalObservation
		}
		id, err := runID(ctx, options.Password)
		if err != nil || ctx.Err() != nil {
			return LocalObservation{}, localObservationError(ctx)
		}
		after, err := read(ctx, options.Directory, options.Cluster, options.Member, options.MasterName)
		// Only small fixed structs and bounded strings are compared, not data files.
		if err != nil || !reflect.DeepEqual(before, after) {
			return LocalObservation{}, localObservationError(ctx)
		}
		observation.RunID = id
		if validateObservation(options.Cluster, options.Member, purpose, observation) != nil {
			return LocalObservation{}, errLocalObservation
		}
		if err := ctx.Err(); err != nil {
			return LocalObservation{}, err
		}
		return observation, nil
	}, nil
}

func safeMasterName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for _, b := range []byte(name) {
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == '-' {
			continue
		}
		return false
	}
	return true
}

func localObservationError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errLocalObservation
}

func readLocalRedisRunID(ctx context.Context, address, password string) (string, error) {
	return readLocalRedisRunIDWithDialer(ctx, address, password, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, address)
	})
}

func readLocalRedisRunIDWithDialer(ctx context.Context, address, password string, dial func(context.Context, string, string) (net.Conn, error)) (id string, resultErr error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var attempted atomic.Bool
	client := redis.NewClient(&redis.Options{
		Addr: address, Password: password, Protocol: 2, DisableIdentity: true,
		MaxRetries: -1, DialerRetries: 1, DialerRetryTimeout: time.Nanosecond, PoolSize: 1, ContextTimeoutEnabled: true,
		DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
		Dialer: func(ctx context.Context, network, address string) (net.Conn, error) {
			// Pool dial retries/background probes are independent of command
			// MaxRetries. No later invocation may perform actual socket I/O.
			if !attempted.CompareAndSwap(false, true) || ctx.Err() != nil {
				return nil, errLocalObservation
			}
			connection, err := dial(ctx, network, address)
			if err != nil {
				return nil, errLocalObservation
			}
			return &localRESPConn{Conn: connection, reader: bufio.NewReaderSize(connection, 1024)}, nil
		},
	})
	defer func() {
		if err := client.Close(); err != nil {
			id, resultErr = "", localObservationError(ctx)
		}
	}()
	info, err := client.Info(ctx, "server").Result()
	if err != nil {
		return "", localObservationError(ctx)
	}
	id, err = parseLocalRunID(info)
	if err != nil {
		return "", errLocalObservation
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return id, nil
}

func parseLocalRunID(info string) (string, error) {
	if len(info) == 0 || len(info) > maximumLocalINFOBytes || !utf8.ValidString(info) {
		return "", errLocalObservation
	}
	for _, r := range info {
		if (r < 32 && r != '\r' && r != '\n') || (r >= 127 && r <= 159) {
			return "", errLocalObservation
		}
	}
	id := ""
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, "run_id:") {
			continue
		}
		if id != "" || !runIDPattern.MatchString(line[len("run_id:"):]) {
			return "", errLocalObservation
		}
		id = line[len("run_id:"):]
	}
	if id == "" {
		return "", errLocalObservation
	}
	return id, nil
}

// localRESPConn bounds frames before go-redis sees bulk/array lengths. Its
// ordinary bulk decoder otherwise allocates the peer-advertised length first.
// RESP2 HELLO may return a small nested array; RESP3 is not supported here.
type localRESPConn struct {
	net.Conn
	reader  *bufio.Reader
	pending *bytes.Reader
}

func (c *localRESPConn) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if c.pending == nil || c.pending.Len() == 0 {
		frame := &bytes.Buffer{}
		if err := readLocalRESPFrame(c.reader, frame, 0); err != nil {
			return 0, errLocalObservation
		}
		c.pending = bytes.NewReader(frame.Bytes())
	}
	return c.pending.Read(data)
}

func readLocalRESPFrame(reader *bufio.Reader, frame *bytes.Buffer, depth int) error {
	if depth > 8 {
		return errLocalObservation
	}
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) < 3 || len(line) > 1024 || line[len(line)-2] != '\r' || frame.Len()+len(line) > maximumLocalINFOBytes+1024 {
		return errLocalObservation
	}
	if _, err := frame.Write(line); err != nil {
		return errLocalObservation
	}
	switch line[0] {
	case '+', '-', ':':
		return nil
	case '$', '*':
		length, err := strconv.ParseInt(string(line[1:len(line)-2]), 10, 32)
		if err != nil || length < -1 {
			return errLocalObservation
		}
		if length == -1 {
			return nil
		}
		if line[0] == '*' {
			if length > 64 {
				return errLocalObservation
			}
			for i := int64(0); i < length; i++ {
				if err := readLocalRESPFrame(reader, frame, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		if length > maximumLocalINFOBytes || int64(frame.Len())+length+2 > maximumLocalINFOBytes+1024 {
			return errLocalObservation
		}
		data := make([]byte, int(length)+2)
		if _, err := io.ReadFull(reader, data); err != nil || data[len(data)-2] != '\r' || data[len(data)-1] != '\n' {
			return errLocalObservation
		}
		if _, err := frame.Write(data); err != nil {
			return errLocalObservation
		}
		return nil
	default:
		return errLocalObservation
	}
}
