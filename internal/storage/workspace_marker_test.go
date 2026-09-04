package storage

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeWorkspaceObjects struct {
	data    map[string][]byte
	options map[string]RootMarkerOptions
	head    bool
	putErr  error
	headErr error
}

func newFakeWorkspaceObjects() *fakeWorkspaceObjects {
	return &fakeWorkspaceObjects{
		data:    map[string][]byte{},
		options: map[string]RootMarkerOptions{},
		head:    true,
	}
}

func (f *fakeWorkspaceObjects) PutEmptyObject(_ context.Context, key string, options RootMarkerOptions) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.data[key] = []byte{}
	f.options[key] = options
	return nil
}

func (f *fakeWorkspaceObjects) HeadObject(_ context.Context, _ string) (bool, error) {
	return f.head, f.headErr
}

func TestRootMarkerProfilesHaveIndependentImmutableOptions(t *testing.T) {
	profileIDs := []string{
		RootMarkerProfileMinIO,
		RootMarkerProfileHuaweiOBSPublic,
		RootMarkerProfileHuaweiOBSPrivate2023,
	}

	for _, profileID := range profileIDs {
		t.Run(profileID, func(t *testing.T) {
			profile, err := RootMarkerProfileByID(profileID)
			require.NoError(t, err)
			assert.Equal(t, profileID, profile.ID)
			assert.Equal(t, RootMarkerTrailingSlash, profile.Style)
			assert.Equal(t, RootMarkerOptions{
				ContentType: "application/x-directory",
				Metadata:    map[string]string{},
			}, profile.MarkerOptions())

			options := profile.MarkerOptions()
			options.Metadata["caller-mutation"] = "must-not-stick"
			assert.Equal(t, map[string]string{}, profile.MarkerOptions().Metadata)
			again, err := RootMarkerProfileByID(profileID)
			require.NoError(t, err)
			assert.Equal(t, map[string]string{}, again.MarkerOptions().Metadata)
		})
	}

	assert.NotEqual(t, RootMarkerProfileHuaweiOBSPublic, RootMarkerProfileHuaweiOBSPrivate2023)
	_, err := RootMarkerProfileByID("unverified-profile")
	require.Error(t, err)
}

func TestPrepareWorkspacePrefixWritesAndVerifiesExactRootMarker(t *testing.T) {
	objects := newFakeWorkspaceObjects()
	profile, err := RootMarkerProfileByID(RootMarkerProfileMinIO)
	require.NoError(t, err)

	err = PrepareWorkspacePrefix(context.Background(), objects, "workspaces/team-a/", profile)
	require.NoError(t, err)
	assert.Equal(t, []byte{}, objects.data["workspaces/team-a/"])
	assert.Equal(t, RootMarkerOptions{
		ContentType: "application/x-directory",
		Metadata:    map[string]string{},
	}, objects.options["workspaces/team-a/"])
	assert.False(t, IsUserVisibleWorkspaceObject("workspaces/team-a/", "workspaces/team-a/"))
	assert.True(t, IsUserVisibleWorkspaceObject("workspaces/team-a/", "workspaces/team-a/file.txt"))
	assert.True(t, IsUserVisibleWorkspaceObject("workspaces/team-a/", "workspaces/team-a/subdir/"))
}

func TestPrepareWorkspacePrefixRejectsNonCanonicalPrefix(t *testing.T) {
	profile, err := RootMarkerProfileByID(RootMarkerProfileMinIO)
	require.NoError(t, err)

	for _, prefix := range []string{"workspaces/team-a", "workspaces/team-a//", "/workspaces/team-a/", "workspaces/../team-a/"} {
		t.Run(prefix, func(t *testing.T) {
			objects := newFakeWorkspaceObjects()
			err := PrepareWorkspacePrefix(context.Background(), objects, prefix, profile)
			require.ErrorIs(t, err, ErrInvalidWorkspacePrefix)
			assert.Empty(t, objects.data)
		})
	}
}

func TestPrepareWorkspacePrefixRequiresHeadVerification(t *testing.T) {
	objects := newFakeWorkspaceObjects()
	objects.head = false
	profile, err := RootMarkerProfileByID(RootMarkerProfileMinIO)
	require.NoError(t, err)

	err = PrepareWorkspacePrefix(context.Background(), objects, "workspaces/team-a/", profile)
	require.ErrorContains(t, err, "verification")
}

func TestNewWorkspaceObjectClientSelectsNativeProvider(t *testing.T) {
	credentials := FileSystemCredentials{AccessKey: []byte("access"), SecretKey: []byte("secret")}

	minioClient, err := NewWorkspaceObjectClient(config.FileSystemConfig{
		Provider: "minio", Bucket: "bucket", Endpoint: "127.0.0.1:9000",
	}, credentials)
	require.NoError(t, err)
	assert.IsType(t, &minioWorkspaceObjectClient{}, minioClient)

	obsClient, err := NewWorkspaceObjectClient(config.FileSystemConfig{
		Provider: "obs", Bucket: "bucket", Endpoint: "https://obs.example.test",
	}, credentials)
	require.NoError(t, err)
	assert.IsType(t, &obsWorkspaceObjectClient{}, obsClient)
	assert.NotSame(t, minioClient.(*minioWorkspaceObjectClient).transport, obsClient.(*obsWorkspaceObjectClient).transport)

	_, err = NewWorkspaceObjectClient(config.FileSystemConfig{
		Provider: "s3", Bucket: "bucket", Endpoint: "s3.example.test",
	}, credentials)
	require.ErrorContains(t, err, "unsupported")
}

func TestNewWorkspaceObjectClientRejectsInvalidCustomCA(t *testing.T) {
	caFile := t.TempDir() + "/ca.pem"
	require.NoError(t, os.WriteFile(caFile, []byte("not a certificate"), 0o600))

	_, err := NewWorkspaceObjectClient(config.FileSystemConfig{
		Provider: "minio", Bucket: "bucket", Endpoint: "127.0.0.1:9000", CAFile: caFile,
	}, FileSystemCredentials{AccessKey: []byte("access"), SecretKey: []byte("secret")})
	require.ErrorContains(t, err, "no certificates")
}

func TestMinIONativeAdapterPutHeadAndNotFound(t *testing.T) {
	var mu sync.Mutex
	objects := map[string]http.Header{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			objects[r.URL.Path] = r.Header.Clone()
			w.Header().Set("ETag", `"marker"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			if _, ok := objects[r.URL.Path]; !ok {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", `"marker"`)
			w.Header().Set("Last-Modified", "Thu, 04 Sep 2026 10:00:00 GMT")
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	client := newWorkspaceClientForServer(t, "minio", server.URL, "")
	exists, err := client.HeadObject(context.Background(), "missing/")
	require.NoError(t, err)
	assert.False(t, exists)

	options := RootMarkerOptions{ContentType: "application/x-directory", Metadata: map[string]string{"marker": "root"}}
	require.NoError(t, client.PutEmptyObject(context.Background(), "workspaces/team-a/", options))
	exists, err = client.HeadObject(context.Background(), "workspaces/team-a/")
	require.NoError(t, err)
	assert.True(t, exists)

	mu.Lock()
	headers := objects["/bucket/workspaces/team-a/"]
	mu.Unlock()
	assert.Equal(t, "application/x-directory", headers.Get("Content-Type"))
	assert.Equal(t, "root", headers.Get("X-Amz-Meta-Marker"))
}

func TestOBSNativeAdapterPutHeadAndNotFound(t *testing.T) {
	var mu sync.Mutex
	objects := map[string]http.Header{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			objects[r.URL.Path] = r.Header.Clone()
			w.Header().Set("ETag", `"marker"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			if _, ok := objects[r.URL.Path]; !ok {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`))
				return
			}
			w.Header().Set("ETag", `"marker"`)
			w.Header().Set("Last-Modified", "Thu, 04 Sep 2026 10:00:00 GMT")
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	client := newWorkspaceClientForServer(t, "obs", server.URL, "")
	exists, err := client.HeadObject(context.Background(), "missing/")
	require.NoError(t, err)
	assert.False(t, exists)

	options := RootMarkerOptions{ContentType: "application/x-directory", Metadata: map[string]string{"marker": "root"}}
	require.NoError(t, client.PutEmptyObject(context.Background(), "workspaces/team-a/", options))
	exists, err = client.HeadObject(context.Background(), "workspaces/team-a/")
	require.NoError(t, err)
	assert.True(t, exists)

	mu.Lock()
	var headers http.Header
	for path, candidate := range objects {
		if strings.HasSuffix(path, "/workspaces/team-a/") {
			headers = candidate
			break
		}
	}
	mu.Unlock()
	require.NotNil(t, headers)
	assert.Equal(t, "application/x-directory", headers.Get("Content-Type"))
	assert.Equal(t, "root", firstNonEmpty(headers.Get("X-Obs-Meta-Marker"), headers.Get("X-Amz-Meta-Marker")))
}

func TestWorkspaceObjectClientCustomCAControlsTLSOperations(t *testing.T) {
	objects := map[string]bool{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			objects[r.URL.Path] = true
			w.Header().Set("ETag", `"marker"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			if !objects[r.URL.Path] {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", `"marker"`)
			w.Header().Set("Last-Modified", "Thu, 04 Sep 2026 10:00:00 GMT")
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	cert, err := x509.ParseCertificate(server.Certificate().Raw)
	require.NoError(t, err)
	caFile := t.TempDir() + "/ca.pem"
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600))

	for _, provider := range []string{"minio", "obs"} {
		t.Run(provider, func(t *testing.T) {
			withoutCA := newWorkspaceClientForServer(t, provider, server.URL, "")
			err := withoutCA.PutEmptyObject(context.Background(), "workspaces/"+provider+"-no-ca/", RootMarkerOptions{ContentType: "application/x-directory"})
			require.Error(t, err)

			withCA := newWorkspaceClientForServer(t, provider, server.URL, caFile)
			require.NoError(t, withCA.PutEmptyObject(context.Background(), "workspaces/"+provider+"-with-ca/", RootMarkerOptions{ContentType: "application/x-directory"}))
			exists, err := withCA.HeadObject(context.Background(), "workspaces/"+provider+"-with-ca/")
			require.NoError(t, err)
			assert.True(t, exists)

			var transport *http.Transport
			switch concrete := withCA.(type) {
			case *minioWorkspaceObjectClient:
				transport = concrete.transport
			case *obsWorkspaceObjectClient:
				transport = concrete.transport
			default:
				t.Fatalf("unexpected client type %T", withCA)
			}
			require.NotNil(t, transport.TLSClientConfig)
			assert.False(t, transport.TLSClientConfig.InsecureSkipVerify)
		})
	}
}

func newWorkspaceClientForServer(t *testing.T, provider, rawURL, caFile string) WorkspaceObjectClient {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	require.NoError(t, err)
	endpoint := rawURL
	useSSL := parsed.Scheme == "https"
	if provider == "minio" {
		endpoint = parsed.Host
	}
	client, err := NewWorkspaceObjectClient(config.FileSystemConfig{
		Provider: provider,
		Bucket:   "bucket",
		Region:   "us-east-1",
		Endpoint: endpoint,
		UseSSL:   useSSL,
		CAFile:   caFile,
	}, FileSystemCredentials{AccessKey: []byte("access"), SecretKey: []byte("secret")})
	require.NoError(t, err, fmt.Sprintf("construct %s workspace client", provider))
	return client
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
