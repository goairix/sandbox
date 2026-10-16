package etcd

import (
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

// This codec is scoped to one original raw Watch call. The byte walk bounds
// protobuf object counts BEFORE generated Unmarshal allocates a response graph.
type dueWatchCodec struct {
	frames, events int
	bytes          uint64
	lastBytes      int
}

func (c *dueWatchCodec) Name() string { return "proto" }

func (c *dueWatchCodec) Marshal(value any) ([]byte, error) {
	r, ok := value.(*pb.WatchRequest)
	if !ok || r == nil || r.Size() > maxMutationBytes {
		return nil, ErrInvalidRecord
	}
	return r.Marshal()
}

func (c *dueWatchCodec) Unmarshal(wire []byte, value any) error {
	r, ok := value.(*pb.WatchResponse)
	if !ok || r == nil || len(wire) > maxMutationBytes {
		return ErrCorruptRecord
	}
	events, err := dueWatchWire(wire, 0)
	if err != nil || events > 118 || c.frames >= 512 || c.events+events > 10000 || c.bytes+uint64(len(wire)) > 8*1024*1024 {
		return ErrCorruptRecord
	}
	c.frames++
	c.events += events
	c.bytes += uint64(len(wire))
	c.lastBytes = len(wire)
	*r = pb.WatchResponse{}
	return r.Unmarshal(wire)
}

func dueWatchVarint(wire []byte, offset *int) (uint64, error) {
	var value uint64
	for shift := uint(0); shift < 70; shift += 7 {
		if *offset >= len(wire) {
			return 0, ErrCorruptRecord
		}
		b := wire[*offset]
		*offset = *offset + 1
		if shift == 63 && b > 1 {
			return 0, ErrCorruptRecord
		}
		value |= uint64(b&127) << shift
		if b < 128 {
			return value, nil
		}
	}
	return 0, ErrCorruptRecord
}

// Four fixed message kinds, not untrusted recursive descent: response ->
// event -> KV; response -> header. No maps, slices or event objects allocated.
func dueWatchWire(wire []byte, kind uint8) (int, error) {
	var seen uint64
	events := 0
	for offset := 0; offset < len(wire); {
		tag, err := dueWatchVarint(wire, &offset)
		if err != nil {
			return 0, err
		}
		field, wireType := tag>>3, tag&7
		if field == 0 || field > 63 {
			return 0, ErrCorruptRecord
		}
		repeated := kind == 0 && field == 11
		if !repeated && seen&(uint64(1)<<field) != 0 {
			return 0, ErrCorruptRecord
		}
		seen |= uint64(1) << field
		message, maxBytes, scalar := uint8(255), -1, false
		switch kind {
		case 0:
			switch field {
			case 1:
				message = 1
			case 2, 3, 4, 5, 7:
				scalar = true
			case 6:
				maxBytes = 256
			case 11:
				message = 2
				events++
			default:
				return 0, ErrCorruptRecord
			}
		case 1:
			if field > 4 {
				return 0, ErrCorruptRecord
			}
			scalar = true
		case 2:
			switch field {
			case 1:
				scalar = true
			case 2:
				message = 3
			default:
				return 0, ErrCorruptRecord // PrevKv is never requested.
			}
		case 3:
			switch field {
			case 1, 5:
				maxBytes = 1024
			case 2, 3, 4, 6:
				scalar = true
			default:
				return 0, ErrCorruptRecord
			}
		default:
			return 0, ErrCorruptRecord
		}
		if events > 118 {
			return 0, ErrCorruptRecord
		}
		if scalar {
			if wireType != 0 {
				return 0, ErrCorruptRecord
			}
			value, err := dueWatchVarint(wire, &offset)
			if err != nil {
				return 0, err
			}
			if (kind == 0 && (field == 3 || field == 4 || field == 7) || kind == 2 && field == 1) && value > 1 {
				return 0, ErrCorruptRecord
			}
			continue
		}
		if wireType != 2 {
			return 0, ErrCorruptRecord
		}
		size, err := dueWatchVarint(wire, &offset)
		if err != nil || size > uint64(len(wire)-offset) || maxBytes >= 0 && size > uint64(maxBytes) {
			return 0, ErrCorruptRecord
		}
		end := offset + int(size)
		if message != 255 {
			if _, err = dueWatchWire(wire[offset:end], message); err != nil {
				return 0, err
			}
		}
		offset = end
	}
	return events, nil
}
