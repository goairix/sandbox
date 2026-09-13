package kubernetes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"strings"
)

func TestFileExistsDoesNotClassifyPodOrTransportErrorsAsMissingFile(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			config := &rest.Config{Host: server.URL}
			client, err := kubernetes.NewForConfig(config)
			require.NoError(t, err)
			err = fileExistsInPod(context.Background(), client, config, "runtime", "pod-a", "/workspace/a")
			require.Error(t, err)
			require.False(t, errors.Is(err, runtime.ErrFileNotFound), "pod/API failures are not missing files: %v", err)
		})
	}
}

func TestFileExistsClassifiesOnlyConfirmedStatENOENT(t *testing.T) {
	original := probePodFile
	t.Cleanup(func() { probePodFile = original })
	for _, tc := range []struct {
		name    string
		result  *runtime.ExecResult
		missing bool
		valid   bool
	}{
		{name: "missing", result: &runtime.ExecResult{ExitCode: 1, Stderr: "stat: cannot statx '/workspace/a': No such file or directory\n"}, missing: true},
		{name: "permission", result: &runtime.ExecResult{ExitCode: 1, Stderr: "stat: cannot statx '/workspace/a': Permission denied\n"}},
		{name: "forged path", result: &runtime.ExecResult{ExitCode: 1, Stderr: "stat: cannot statx 'a: No such file or directory\n': Permission denied\n"}},
		{name: "bad exit", result: &runtime.ExecResult{ExitCode: 2, Stderr: "stat: invalid argument: No such file or directory\n"}},
		{name: "file", result: &runtime.ExecResult{Stdout: "regular file\n"}, valid: true},
		{name: "empty file", result: &runtime.ExecResult{Stdout: "regular empty file\n"}, valid: true},
		{name: "directory", result: &runtime.ExecResult{Stdout: "directory\n"}},
		{name: "nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probePodFile = func(_ context.Context, _ kubernetes.Interface, _ *rest.Config, _, _ string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
				require.Equal(t, "LC_ALL=C stat -L -c '%F' -- '-danger'", req.Command)
				return tc.result, nil
			}
			err := fileExistsInPod(context.Background(), nil, nil, "runtime", "pod-a", "-danger")
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Equal(t, tc.missing, errors.Is(err, runtime.ErrFileNotFound))
			}
		})
	}
}

func TestDownloadFileRejectsMissingPathBeforeStartingTarStream(t *testing.T) {
	original := probePodFile
	t.Cleanup(func() { probePodFile = original })
	probePodFile = func(context.Context, kubernetes.Interface, *rest.Config, string, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
		return &runtime.ExecResult{ExitCode: 1, Stderr: "stat: cannot statx '/workspace/a': No such file or directory\n"}, nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		require.False(t, strings.Contains(req.URL.RawQuery, "command=tar"), "tar must not start for a known missing file")
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	config := &rest.Config{Host: server.URL}
	client, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)
	reader, err := downloadFileFromPod(context.Background(), client, config, "runtime", "pod-a", "/workspace/a")
	if reader != nil {
		require.NoError(t, reader.Close())
	}
	require.ErrorIs(t, err, runtime.ErrFileNotFound)
}
