package docker

import (
	"context"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// dockerAPI is deliberately the smallest Docker Engine surface used by this
// runtime. Keeping it package-private makes lifecycle ambiguity and cleanup
// ordering testable without giving production code access to broader daemon
// operations.
type dockerAPI interface {
	Close() error
	ContainerCreate(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *ocispec.Platform, string) (container.CreateResponse, error)
	ContainerStart(context.Context, string, container.StartOptions) error
	ContainerStop(context.Context, string, container.StopOptions) error
	ContainerRemove(context.Context, string, container.RemoveOptions) error
	ContainerInspect(context.Context, string) (types.ContainerJSON, error)
	ContainerList(context.Context, container.ListOptions) ([]types.Container, error)
	ContainerRename(context.Context, string, string) error
	ContainerExecCreate(context.Context, string, container.ExecOptions) (types.IDResponse, error)
	ContainerExecStart(context.Context, string, container.ExecStartOptions) error
	ContainerExecAttach(context.Context, string, container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecInspect(context.Context, string) (container.ExecInspect, error)
	CopyFromContainer(context.Context, string, string) (io.ReadCloser, container.PathStat, error)
	CopyToContainer(context.Context, string, string, io.Reader, container.CopyToContainerOptions) error
	NetworkCreate(context.Context, string, network.CreateOptions) (network.CreateResponse, error)
	NetworkList(context.Context, network.ListOptions) ([]network.Summary, error)
	NetworkInspect(context.Context, string, network.InspectOptions) (network.Inspect, error)
	NetworkConnect(context.Context, string, string, *network.EndpointSettings) error
	NetworkDisconnect(context.Context, string, string, bool) error
	NetworkRemove(context.Context, string) error
	VolumeCreate(context.Context, volume.CreateOptions) (volume.Volume, error)
	VolumeInspect(context.Context, string) (volume.Volume, error)
	VolumeList(context.Context, volume.ListOptions) (volume.ListResponse, error)
	VolumeRemove(context.Context, string, bool) error
}
