package etcd

// cleanupManagedSourceBound is checked before installing the serial workspace.
// These are owned business/protocol allocations, not process RSS. Backend TLS,
// original caller contexts and the fixed worker infrastructure are shared roots.
// In particular this phase never constructs a TLS destination or parses DER.
//
// Raw responses: initial4=40960, full14=147456, snapshot/placement=69632,
// intent=4096; local-v2 fallback may reread snapshot/placement once. Fixed typed
// decodes contribute 299008 bytes and explicit signed-payload clones 110592.
// Doubling plus headers/keys bounds allocator capacities below the 2MiB owned
// source allowance. The independent temporary terms below cover simultaneous
// parser/verifier frames rather than charging only the final decoded DTO.
func cleanupManagedSourceBound() uint64 {
	const raw = 40960 + 147456 + 2*(65536+4096) + 4096
	const typed = 299008
	const clones = 110592
	const bodies = 2*(raw+typed+clones) + 2*3*20*1024
	const oldBundle = 2 * (13*4096 + 12*1024)
	const objectAndCapacityHeaders = 32 << 10
	const owned = bodies + oldBundle + objectAndCapacityHeaders
	// Go1.25 JSON: typed depth<=8, <=64 fields/object. Decoder and RawMessage
	// backing D*(4N+1024), two maps/layer 128KiB, <=17 scanner stacks (depth
	// 10000, capacity16384), and one escaped encoder with 4*(6N+4096)
	// backing sum below 6MiB for N=65536. RawMessage contents stay opaque.
	const metadataJSON = 6 << 20
	// The v1 snapshot's one-level RawMessage map has <=ceil((N+1)/5)
	// entries; 256B/entry+3N+4096 is below 4MiB. No recursive any tree.
	const snapshotMap = 4 << 20
	// Historical Ed25519 verification has <16 fixed DTO frames: 16*16KiB*4
	// plus 1024*128B nodes, one 400KiB encoder and 128KiB fixed maps <2MiB.
	const verifier = 2 << 20
	return owned + metadataJSON + snapshotMap + verifier
}
