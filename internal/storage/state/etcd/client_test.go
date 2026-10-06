package etcd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	clientv3 "go.etcd.io/etcd/client/v3"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

func validOptions(t *testing.T) Options {
	t.Helper()
	n, err := NewNamespace("/test", "authority", "cell")
	if err != nil {
		t.Fatal(err)
	}
	return Options{Endpoints: []string{"http://127.0.0.1:2379"}, Namespace: n, Identity: Identity{SchemaVersion: 1, Prefix: "/test", AuthorityID: "authority", Cell: "cell", ClusterID: 1, StorageID: "store", RuntimeID: "runtime", RestoreEpoch: "epoch"}, AllowInsecureLoopback: true}
}

func TestOptionsValidation(t *testing.T) {
	cases := map[string]func(*Options){
		"no endpoints":     func(o *Options) { o.Endpoints = nil },
		"no namespace":     func(o *Options) { o.Namespace = Namespace{} },
		"no opt-in":        func(o *Options) { o.AllowInsecureLoopback = false },
		"remote plain":     func(o *Options) { o.Endpoints = []string{"http://example.com:2379"} },
		"bare loopback":    func(o *Options) { o.Endpoints = []string{"127.0.0.1:2379"} },
		"userinfo":         func(o *Options) { o.Endpoints = []string{"http://a:b@localhost:2379"} },
		"path":             func(o *Options) { o.Endpoints = []string{"http://localhost:2379/path"} },
		"negative dial":    func(o *Options) { o.DialTimeout = -time.Second },
		"negative request": func(o *Options) { o.RequestTimeout = -time.Second },
		"schema":           func(o *Options) { o.Identity.SchemaVersion = 2 },
		"prefix":           func(o *Options) { o.Identity.Prefix = "/other" },
		"authority":        func(o *Options) { o.Identity.AuthorityID = "other" },
		"cell":             func(o *Options) { o.Identity.Cell = "other" },
		"cluster":          func(o *Options) { o.Identity.ClusterID = 0 },
		"storage":          func(o *Options) { o.Identity.StorageID = "" },
		"runtime":          func(o *Options) { o.Identity.RuntimeID = "" },
		"epoch":            func(o *Options) { o.Identity.RestoreEpoch = "" },
		"tls skip verify": func(o *Options) {
			o.Endpoints = []string{"https://localhost:2379"}
			o.TLS = &tls.Config{InsecureSkipVerify: true}
		},
		"tls no roots": func(o *Options) {
			o.Endpoints = []string{"https://localhost:2379"}
			o.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{{1}}}}}
		},
		"tls no certificate": func(o *Options) {
			o.Endpoints = []string{"https://localhost:2379"}
			o.TLS = &tls.Config{RootCAs: x509.NewCertPool()}
		},
		"tls on plain": func(o *Options) { o.TLS = &tls.Config{} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			o := validOptions(t)
			change(&o)
			b, err := New(context.Background(), o)
			if b != nil {
				b.Close()
				t.Fatal("invalid options returned backend")
			}
			if !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("got %v, want invalid configuration", err)
			}
		})
	}
}

func TestIdentityDecoderRejectsInvalidMetadata(t *testing.T) {
	o := validOptions(t)
	valid := `{"schema_version":1,"prefix":"/test","authority_id":"authority","cell":"cell","cluster_id":1,"storage_id":"store","runtime_id":"runtime"}`
	if got, err := decodeIdentity(valid, o.Namespace); err != nil || got.RestoreEpoch != "" {
		t.Fatalf("valid metadata = %+v, %v", got, err)
	}
	for _, value := range []string{"", "null", "[]", `{}`, valid + ` {}`, valid[:len(valid)-1] + `,"unknown":true}`, valid[:len(valid)-1] + `,"restore_epoch":"epoch"}`, strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1), strings.Replace(valid, `"cluster_id":1`, `"cluster_id":0`, 1)} {
		if _, err := decodeIdentity(value, o.Namespace); !errors.Is(err, ErrIdentityMismatch) {
			t.Errorf("metadata %q accepted: %v", value, err)
		}
	}
}

func TestNewIntegrationRejectsEmptyNamespaceWithoutWriting(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx, cancel := b.requestContext(context.Background())
	defer cancel()
	if _, err := raw.Delete(ctx, b.namespace.Root(), clientv3.WithPrefix()); err != nil {
		t.Fatal(err)
	}
	if opened, err := New(ctx, integrationOptions(b, raw)); !errors.Is(err, ErrIdentityMismatch) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("empty namespace = %v", err)
	}
	response, err := raw.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Kvs) != 0 {
		t.Fatalf("New wrote %d metadata keys", len(response.Kvs))
	}
}

func integrationOptions(b *Backend, raw *clientv3.Client) Options {
	return Options{Endpoints: raw.Endpoints(), Namespace: b.namespace, Identity: Identity{SchemaVersion: 1, Prefix: b.namespace.prefix, AuthorityID: b.namespace.scope, Cell: b.namespace.cell, ClusterID: b.clusterID, StorageID: "test-storage", RuntimeID: "test-runtime", RestoreEpoch: b.restoreEpoch}, AllowInsecureLoopback: true}
}

func TestNewIntegrationChecksAllIdentityFieldsAndRestoreEpoch(t *testing.T) {
	b, raw := integrationBackend(t)
	o := integrationOptions(b, raw)
	changes := map[string]func(*Identity){
		"schema":    func(i *Identity) { i.SchemaVersion = 2 },
		"prefix":    func(i *Identity) { i.Prefix = "/changed" },
		"authority": func(i *Identity) { i.AuthorityID = "changed" },
		"cell":      func(i *Identity) { i.Cell = "changed" },
		"cluster":   func(i *Identity) { i.ClusterID++ },
		"storage":   func(i *Identity) { i.StorageID = "changed" },
		"runtime":   func(i *Identity) { i.RuntimeID = "changed" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			changed := o.Identity
			change(&changed)
			value, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := b.requestContext(context.Background())
			defer cancel()
			if _, err := raw.Put(ctx, b.identityKey, string(value)); err != nil {
				t.Fatal(err)
			}
			opened, err := New(ctx, o)
			if opened != nil {
				opened.Close()
				t.Fatal("changed identity returned backend")
			}
			if !errors.Is(err, ErrIdentityMismatch) {
				t.Fatalf("changed identity = %v", err)
			}
		})
	}
	ctx, cancel := b.requestContext(context.Background())
	defer cancel()
	if _, err := raw.Put(ctx, b.identityKey, b.identityValue); err != nil {
		t.Fatal(err)
	}
	for _, malformed := range []string{`{"schema_version":1}`, b.identityValue[:len(b.identityValue)-1] + `,"unexpected":"value"}`, b.identityValue[:len(b.identityValue)-1] + `,"restore_epoch":"test-epoch"}`} {
		if _, err := raw.Put(ctx, b.identityKey, malformed); err != nil {
			t.Fatal(err)
		}
		if opened, err := New(ctx, o); !errors.Is(err, ErrIdentityMismatch) {
			if opened != nil {
				opened.Close()
			}
			t.Fatalf("malformed identity = %v", err)
		}
	}
	if _, err := raw.Put(ctx, b.identityKey, b.identityValue); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Put(ctx, b.restoreKey, "changed-epoch"); err != nil {
		t.Fatal(err)
	}
	if opened, err := New(ctx, o); !errors.Is(err, ErrIdentityMismatch) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("changed epoch = %v", err)
	}
	if _, err := raw.Delete(ctx, b.restoreKey); err != nil {
		t.Fatal(err)
	}
	if opened, err := New(ctx, o); !errors.Is(err, ErrIdentityMismatch) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("missing epoch = %v", err)
	}
}

func TestNewIntegrationPreservesMetadataBytesAndCloses(t *testing.T) {
	b, raw := integrationBackend(t)
	o := integrationOptions(b, raw)
	value := " \n" + b.identityValue + " \n"
	ctx, cancel := b.requestContext(context.Background())
	defer cancel()
	if _, err := raw.Put(ctx, b.identityKey, value); err != nil {
		t.Fatal(err)
	}
	opened, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if opened.identityValue != value {
		opened.Close()
		t.Fatal("backend changed operator metadata bytes")
	}
	response, err := raw.Txn(ctx).If(opened.baseComparisons()...).Then(clientv3.OpGet(opened.identityKey)).Commit()
	if err != nil || !response.Succeeded {
		opened.Close()
		t.Fatalf("base comparisons = %v, %v", response, err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOptionsTLSCertificateValidation(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "client"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	o := validOptions(t)
	o.Endpoints = []string{"https://localhost:2379"}
	o.TLS = &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	if err := validateOptions(o); err != nil {
		t.Fatalf("valid mTLS rejected: %v", err)
	}
	for name, mutate := range map[string]func(*tls.Config){
		"client certificate callback": func(c *tls.Config) {
			c.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				return &tls.Certificate{}, nil
			}
		},
		"TLS 1.0":             func(c *tls.Config) { c.MinVersion = tls.VersionTLS10 },
		"TLS 1.1":             func(c *tls.Config) { c.MinVersion = tls.VersionTLS11 },
		"empty roots":         func(c *tls.Config) { c.RootCAs = x509.NewCertPool() },
		"empty certificate":   func(c *tls.Config) { c.Certificates[0].Certificate = nil },
		"bad certificate":     func(c *tls.Config) { c.Certificates[0].Certificate = [][]byte{{1}} },
		"missing private key": func(c *tls.Config) { c.Certificates[0].PrivateKey = nil },
		"mismatched private key": func(c *tls.Config) {
			other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			c.Certificates[0].PrivateKey = other
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := o
			changed.TLS = o.TLS.Clone()
			changed.TLS.Certificates = append([]tls.Certificate(nil), o.TLS.Certificates...)
			mutate(changed.TLS)
			if err := validateOptions(changed); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("invalid mTLS accepted: %v", err)
			}
		})
	}
}

func TestRequestContextHonorsCallerDeadlineAndCancellation(t *testing.T) {
	b := &Backend{requestTimeout: time.Second}
	parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	child, childCancel := b.requestContext(parent)
	defer childCancel()
	parentDeadline, _ := parent.Deadline()
	childDeadline, _ := child.Deadline()
	if !parentDeadline.Equal(childDeadline) {
		t.Fatal("request extended caller deadline")
	}
	cancel()
	if !errors.Is(child.Err(), context.Canceled) {
		t.Fatalf("request ignored parent cancellation: %v", child.Err())
	}
}

func TestNewIntegrationRejectsExpectedClusterMismatch(t *testing.T) {
	b, raw := integrationBackend(t)
	o := integrationOptions(b, raw)
	o.Identity.ClusterID++
	opened, err := New(context.Background(), o)
	if opened != nil {
		opened.Close()
		t.Fatal("wrong expected cluster returned backend")
	}
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("wrong expected cluster = %v", err)
	}
}

func TestNewIntegrationRejectsMixedEndpointClusters(t *testing.T) {
	foreignEndpoint := strings.TrimSpace(os.Getenv("TEST_ETCD_FOREIGN_ENDPOINT"))
	if foreignEndpoint == "" {
		t.Skip("set TEST_ETCD_FOREIGN_ENDPOINT to an independent etcd cluster")
	}
	b, raw := integrationBackend(t)
	o := integrationOptions(b, raw)
	foreign, err := clientv3.New(clientv3.Config{Endpoints: []string{foreignEndpoint}, DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreign.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := foreign.Status(ctx, foreignEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if status.Header.ClusterId == b.clusterID {
		t.Fatal("foreign fixture must be an independent cluster")
	}
	before, err := foreign.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Kvs) != 0 {
		t.Fatal("foreign fixture already has this test namespace")
	}
	o.Endpoints = append(o.Endpoints, foreignEndpoint)
	opened, err := New(ctx, o)
	if opened != nil {
		opened.Close()
		t.Fatal("mixed clusters returned backend")
	}
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("mixed clusters = %v", err)
	}
	after, err := foreign.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Kvs) != 0 {
		t.Fatalf("New wrote %d keys to the foreign cluster", len(after.Kvs))
	}
}

func TestNewIntegrationRequiresPermanentMetadata(t *testing.T) {
	b, raw := integrationBackend(t)
	o := integrationOptions(b, raw)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err := raw.Grant(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := raw.Revoke(ctx, lease.ID); err != nil {
			t.Errorf("cleanup metadata test lease: %v", err)
		}
	})
	for name, metadata := range map[string]struct{ key, value string }{
		"identity": {b.identityKey, b.identityValue},
		"restore":  {b.restoreKey, b.restoreEpoch},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := raw.Put(ctx, metadata.key, metadata.value, clientv3.WithLease(lease.ID)); err != nil {
				t.Fatal(err)
			}
			opened, err := New(ctx, o)
			if opened != nil {
				opened.Close()
				t.Error("leased metadata returned backend")
			}
			if !errors.Is(err, ErrIdentityMismatch) {
				t.Errorf("leased metadata = %v", err)
			}
			response, err := raw.Txn(ctx).If(b.baseComparisons()...).Then(clientv3.OpGet(b.identityKey)).Commit()
			if err != nil {
				t.Fatal(err)
			}
			if response.Succeeded {
				t.Error("base comparisons accepted same metadata value attached to a lease")
			}
			if _, err := raw.Put(ctx, metadata.key, metadata.value); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIdentityLengthLimits(t *testing.T) {
	o := validOptions(t)
	for name, change := range map[string]func(*Identity){
		"storage": func(i *Identity) { i.StorageID = strings.Repeat("s", 129) },
		"runtime": func(i *Identity) { i.RuntimeID = strings.Repeat("r", 129) },
		"restore": func(i *Identity) { i.RestoreEpoch = strings.Repeat("e", 129) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := o
			change(&changed.Identity)
			if err := validateOptions(changed); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("oversized expected identity accepted: %v", err)
			}
		})
	}
	o.Identity.StorageID = strings.Repeat("s", 128)
	o.Identity.RuntimeID = strings.Repeat("r", 128)
	o.Identity.RestoreEpoch = strings.Repeat("e", 128)
	if err := validateOptions(o); err != nil {
		t.Fatalf("128-byte identity rejected: %v", err)
	}
	value, err := json.Marshal(o.Identity)
	if err != nil {
		t.Fatal(err)
	}
	padded := string(value) + strings.Repeat(" ", 4096-len(value))
	if _, err := decodeIdentity(padded, o.Namespace); err != nil {
		t.Fatalf("4096-byte JSON rejected: %v", err)
	}
	if _, err := decodeIdentity(padded+" ", o.Namespace); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("oversized raw JSON accepted: %v", err)
	}
	o.Identity.StorageID += "s"
	value, err = json.Marshal(o.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeIdentity(string(value), o.Namespace); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("oversized stored field accepted: %v", err)
	}
}

func TestCloneTLSConfigOwnsMutableData(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "original"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	source := &tls.Config{RootCAs: roots, ClientCAs: roots, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf, OCSPStaple: []byte{1}, SignedCertificateTimestamps: [][]byte{{2}}, SupportedSignatureAlgorithms: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256}}}, NextProtos: []string{"h2"}, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}, CurvePreferences: []tls.CurveID{tls.CurveP256}, EncryptedClientHelloConfigList: []byte{3}, EncryptedClientHelloKeys: []tls.EncryptedClientHelloKey{{Config: []byte{4}, PrivateKey: []byte{5}}}}
	owned, err := cloneTLSConfig(source)
	if err != nil {
		t.Fatal(err)
	}
	source.Certificates[0].Certificate[0][0] ^= 255
	source.Certificates[0].Leaf.Subject.CommonName = "changed"
	source.Certificates[0].OCSPStaple[0] = 9
	source.Certificates[0].SignedCertificateTimestamps[0][0] = 9
	source.Certificates[0].SupportedSignatureAlgorithms[0] = tls.PSSWithSHA256
	source.NextProtos[0] = "changed"
	source.CipherSuites[0] = 0
	source.CurvePreferences[0] = 0
	source.EncryptedClientHelloConfigList[0] = 9
	source.EncryptedClientHelloKeys[0].Config[0] = 9
	source.EncryptedClientHelloKeys[0].PrivateKey[0] = 9
	template.SerialNumber = big.NewInt(2)
	template.Subject.CommonName = "second"
	secondDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	second, err := x509.ParseCertificate(secondDER)
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(second)
	cert := owned.Certificates[0]
	if cert.Leaf.Subject.CommonName != "original" || cert.Certificate[0][0] == source.Certificates[0].Certificate[0][0] || cert.OCSPStaple[0] != 1 || cert.SignedCertificateTimestamps[0][0] != 2 || cert.SupportedSignatureAlgorithms[0] != tls.ECDSAWithP256AndSHA256 {
		t.Fatal("cloned certificate aliases caller mutable data")
	}
	if len(owned.RootCAs.Subjects()) != 1 || len(owned.ClientCAs.Subjects()) != 1 {
		t.Fatal("cloned CA pools alias caller pools")
	}
	if owned.NextProtos[0] != "h2" || owned.CipherSuites[0] != tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 || owned.CurvePreferences[0] != tls.CurveP256 || owned.EncryptedClientHelloConfigList[0] != 3 || owned.EncryptedClientHelloKeys[0].Config[0] != 4 || owned.EncryptedClientHelloKeys[0].PrivateKey[0] != 5 {
		t.Fatal("cloned TLS config aliases caller slices")
	}
	if cert.PrivateKey != key {
		t.Fatal("signer ownership contract changed")
	}
}

func TestNewIntegrationRejectsOversizedStoredMetadata(t *testing.T) {
	b, raw := integrationBackend(t)
	o := integrationOptions(b, raw)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for name, metadata := range map[string]struct{ key, value, original string }{
		"identity JSON": {b.identityKey, b.identityValue + strings.Repeat(" ", 4097-len(b.identityValue)), b.identityValue},
		"restore epoch": {b.restoreKey, strings.Repeat("e", 129), b.restoreEpoch},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := raw.Put(ctx, metadata.key, metadata.value); err != nil {
				t.Fatal(err)
			}
			opened, err := New(ctx, o)
			if opened != nil {
				opened.Close()
				t.Fatal("oversized stored metadata returned backend")
			}
			if !errors.Is(err, ErrIdentityMismatch) {
				t.Fatalf("oversized stored metadata = %v", err)
			}
			if _, err := raw.Put(ctx, metadata.key, metadata.original); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOptionsRejectsInvalidRestoreEpochIdentifiers(t *testing.T) {
	for _, epoch := range []string{"invalid\xff", ".", "..", "epoch/escape", "epoch\\escape", "epoch\x00", "epoch\n", "epoch space", "纪元"} {
		t.Run(fmt.Sprintf("%q", epoch), func(t *testing.T) {
			o := validOptions(t)
			o.Identity.RestoreEpoch = epoch
			if err := validateOptions(o); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("invalid restore epoch accepted: %v", err)
			}
		})
	}
}
