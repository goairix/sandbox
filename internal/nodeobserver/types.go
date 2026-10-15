package nodeobserver

import (
	"crypto/ed25519"
	"crypto/tls"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"k8s.io/client-go/rest"
)

// Options configures only bounded concrete authority and transport clients.
// Kernel facts always come from fixed /proc, /sys/fs/cgroup and native syscalls.
type Options struct {
	Binding             controlprotocol.TrustBinding
	Roots               []ed25519.PublicKey
	NodeName            string
	NodeUID             string
	Certificate         []byte
	PrivateKey          ed25519.PrivateKey
	Clock               controlprotocol.AuthorityClock
	EtcdEndpoints       []string
	EtcdTLS             *tls.Config
	EtcdClusterID       uint64
	EtcdIdentity        []byte
	KubernetesConfig    *rest.Config
	KubernetesNamespace string
	JournalDirectory    string
	MaxSlots            uint32
	MaxActive           uint32
	RequestTimeout      time.Duration
	TerminationTimeout  time.Duration
}
