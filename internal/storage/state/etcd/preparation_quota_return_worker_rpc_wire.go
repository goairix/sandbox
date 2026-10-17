package etcd

type coldQuotaWorkerRPCWireKind uint8

const (
	coldQuotaWireTxn coldQuotaWorkerRPCWireKind = iota
	coldQuotaWireOp
	coldQuotaWireRange
	coldQuotaWirePut
	coldQuotaWireDelete
	coldQuotaWireKV
	coldQuotaWireHeader
	coldQuotaWireGrant
	coldQuotaWireRevoke
)

type coldQuotaWorkerRPCWireCounts struct {
	objects int
	ops     int
}

// This is a fixed finite schema, not an untrusted recursive protobuf decoder:
// Txn -> ResponseOp -> point Range -> KV/header. ResponseOp's Txn variant is
// rejected without descending. At most 64 ops, one KV per Range and 258 total
// objects (root+header+64*(op+range+header+KV)) reach generated Unmarshal.
// The existing primitive varint parser is shared without changing Watch policy.
func coldQuotaWorkerRPCWire(wire []byte, kind coldQuotaWorkerRPCWireKind, counts *coldQuotaWorkerRPCWireCounts) error {
	counts.objects++
	if counts.objects > 258 {
		return ErrCorruptRecord
	}
	var seen uint64
	points := 0
	for offset := 0; offset < len(wire); {
		tag, err := dueWatchVarint(wire, &offset)
		if err != nil {
			return err
		}
		field, wireType := tag>>3, tag&7
		if field == 0 || field > 63 {
			return ErrCorruptRecord
		}
		repeated := kind == coldQuotaWireTxn && field == 3 || kind == coldQuotaWireRange && field == 2
		if !repeated && seen&(uint64(1)<<field) != 0 {
			return ErrCorruptRecord
		}
		seen |= uint64(1) << field
		message := coldQuotaWorkerRPCWireKind(255)
		maxBytes, scalar := -1, false
		switch kind {
		case coldQuotaWireGrant:
			switch field {
			case 1:
				message = coldQuotaWireHeader
			case 2, 3:
				scalar = true
			case 4:
				maxBytes = 1024 // Grant.Error has no domain schema bound.
			default:
				return ErrCorruptRecord
			}
		case coldQuotaWireRevoke, coldQuotaWirePut:
			if field != 1 {
				return ErrCorruptRecord // Put's PrevKv is never requested.
			}
			message = coldQuotaWireHeader
		case coldQuotaWireTxn:
			switch field {
			case 1:
				message = coldQuotaWireHeader
			case 2:
				scalar = true
			case 3:
				counts.ops++
				if counts.ops > 64 {
					return ErrCorruptRecord
				}
				message = coldQuotaWireOp
			default:
				return ErrCorruptRecord
			}
		case coldQuotaWireOp:
			// Exactly one fixed response oneof is allowed, even when two
			// distinct fields would be accepted as last-one-wins by protobuf.
			if seen&(seen-1) != 0 {
				return ErrCorruptRecord
			}
			switch field {
			case 1:
				message = coldQuotaWireRange
			case 2:
				message = coldQuotaWirePut
			case 3:
				message = coldQuotaWireDelete
			default:
				return ErrCorruptRecord
			}
		case coldQuotaWireRange:
			switch field {
			case 1:
				message = coldQuotaWireHeader
			case 2:
				points++
				if points > 1 {
					return ErrCorruptRecord
				}
				message = coldQuotaWireKV
			case 3, 4:
				scalar = true
			default:
				return ErrCorruptRecord
			}
		case coldQuotaWireDelete:
			switch field {
			case 1:
				message = coldQuotaWireHeader
			case 2:
				scalar = true
			default:
				return ErrCorruptRecord // PrevKvs would allocate an extra graph.
			}
		case coldQuotaWireKV:
			switch field {
			case 1:
				maxBytes = maxNamespaceKeyBytes
			case 5:
				maxBytes = 16384
			case 2, 3, 4, 6:
				scalar = true
			default:
				return ErrCorruptRecord
			}
		case coldQuotaWireHeader:
			if field > 4 {
				return ErrCorruptRecord
			}
			scalar = true
		default:
			return ErrCorruptRecord
		}
		if scalar {
			if wireType != 0 {
				return ErrCorruptRecord
			}
			value, err := dueWatchVarint(wire, &offset)
			if err != nil {
				return err
			}
			if kind == coldQuotaWireTxn && field == 2 && value > 1 || kind == coldQuotaWireRange && (field == 3 && value != 0 || field == 4 && value > 1) || kind == coldQuotaWireDelete && field == 2 && value > 1 {
				return ErrCorruptRecord
			}
			continue
		}
		if wireType != 2 {
			return ErrCorruptRecord
		}
		size, err := dueWatchVarint(wire, &offset)
		if err != nil || size > uint64(len(wire)-offset) || maxBytes >= 0 && size > uint64(maxBytes) {
			return ErrCorruptRecord
		}
		end := offset + int(size)
		if message != coldQuotaWorkerRPCWireKind(255) {
			if err := coldQuotaWorkerRPCWire(wire[offset:end], message, counts); err != nil {
				return err
			}
		}
		offset = end
	}
	if kind == coldQuotaWireOp && seen == 0 {
		return ErrCorruptRecord
	}
	return nil
}
