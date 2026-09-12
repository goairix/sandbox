package kubecontract

const (
	MetadataPrefix = "goairix.github.io"

	BackendFingerprintAnnotation = MetadataPrefix + "/sandbox-backend-fingerprint"
	DrainProtocolAnnotation      = MetadataPrefix + "/sandbox-drain-protocol"
	CleanupProtocolAnnotation    = MetadataPrefix + "/sandbox-cleanup-protocol"
	FUSERuntimeCleanupFinalizer  = MetadataPrefix + "/sandbox-fuse-runtime-cleanup"
)
