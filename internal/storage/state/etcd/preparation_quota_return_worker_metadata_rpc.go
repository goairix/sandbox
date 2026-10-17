package etcd

import pb "go.etcd.io/etcd/api/v3/etcdserverpb"

// coldQuotaWorkerMetadataRPC owns one pure-point Txn, mutually exclusive with
// Journal and the four-phase mutation cohort on this original worker call.
// Its codec binds the native receive limit to the actual request's remaining
// Range allowance and permits 64 KiB history values only in this phase. The
// reader retains paid admission, full W20, evidence validation and call.finish;
// transport alone grants no quota, mutation or physical-cleanup authority.
// The caller keeps the request graph unchanged until the actual native return.
func (b *Backend) coldQuotaWorkerMetadataRPC(call *workerCall, request *pb.TxnRequest) (*pb.TxnResponse, error) {
	return b.coldQuotaWorkerTxnRPC(call, coldQuotaRPCMetadata, request)
}
