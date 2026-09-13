package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/stretchr/testify/require"
)

func TestOwnerInspectionDoesNotInitializeObjectStorage(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests = append(requests, req.Method+" "+req.URL.Path)
		w.Header().Set("Content-Type", "application/xml")
		switch req.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		case http.MethodGet:
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`))
		}
	}))
	defer server.Close()
	cfg := config.FileSystemConfig{Provider: "minio", Endpoint: strings.TrimPrefix(server.URL, "http://"), Bucket: "inspection-test", AccessKey: "test-access", SecretKey: "test-secret"}
	filesystem, meta, err := newFilesystemForMode(cfg, "identity", true)
	require.NoError(t, err)
	require.Nil(t, filesystem)
	require.Equal(t, storage.ProviderMinIO, meta.Provider)
	require.Empty(t, requests, "owner inspection must not create a bucket or initialize storage")
}
