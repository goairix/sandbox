package docker

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
)

type downloadErrorDockerAPI struct {
	dockerAPI
	err error
}

func (a *downloadErrorDockerAPI) CopyFromContainer(context.Context, string, string) (io.ReadCloser, container.PathStat, error) {
	return nil, container.PathStat{}, a.err
}

func TestDownloadFileNormalizesOnlyDockerMissingFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		missing bool
	}{
		{name: "file", err: errdefs.NotFound(errors.New("Could not find the file /workspace/a in container runtime-a")), missing: true},
		{name: "container", err: errdefs.NotFound(errors.New("No such container: runtime-a"))},
		{name: "forbidden", err: errors.New("permission denied")},
		{name: "transport", err: errors.New("connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, fake := newFakeDockerRuntime(t)
			fake.containers["runtime-a"] = &fakeContainer{config: &container.Config{User: "1000"}, name: "runtime-a", running: true}
			rt.cli = &downloadErrorDockerAPI{dockerAPI: fake, err: tc.err}
			_, err := rt.DownloadFile(context.Background(), "runtime-a", "/workspace/a")
			if tc.missing {
				require.ErrorIs(t, err, runtime.ErrFileNotFound)
			} else {
				require.ErrorIs(t, err, tc.err)
				require.NotErrorIs(t, err, runtime.ErrFileNotFound)
			}
		})
	}
}
