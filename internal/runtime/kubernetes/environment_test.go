package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Report only the injected test variables, never the host's environment.
func TestSandboxEnvironmentChild(t *testing.T) {
	if os.Getenv("SANDBOX_ENVIRONMENT_TEST_CHILD") != "1" {
		return
	}
	result := map[string]string{}
	for _, name := range []string{"KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT", "KUBERNETES_PORT", "KUBERNETES_PORT_443_TCP_ADDR", "KUBERNETES_PORT_443_TCP_PORT", "KUBERNETES_PORT_443_TCP_PROTO", "KUBERNETES_PORT_443_TCP", "KUBERNETES_SERVICE_PORT_HTTPS", "PATH", "SANDBOX_POD_UID", "BUSINESS_ENV"} {
		if value, ok := os.LookupEnv(name); ok {
			result[name] = value
		}
	}
	if json.NewEncoder(os.Stdout).Encode(result) != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func assertSandboxEnvironmentClean(t *testing.T, prefix []string) {
	t.Helper()
	for _, inherited := range []string{"", "cluster-address"} {
		argv := append(append([]string(nil), prefix...), os.Args[0], "-test.run=^TestSandboxEnvironmentChild$")
		command := exec.Command(argv[0], argv[1:]...)
		command.Env = []string{"PATH=/usr/bin:/bin", "SANDBOX_ENVIRONMENT_TEST_CHILD=1", "SANDBOX_POD_UID=uid-a", "BUSINESS_ENV=keep-me"}
		for _, name := range []string{"KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT", "KUBERNETES_PORT", "KUBERNETES_PORT_443_TCP_ADDR", "KUBERNETES_PORT_443_TCP_PORT", "KUBERNETES_PORT_443_TCP_PROTO", "KUBERNETES_PORT_443_TCP", "KUBERNETES_SERVICE_PORT_HTTPS"} {
			command.Env = append(command.Env, name+"="+inherited)
		}
		output, err := command.Output()
		require.NoError(t, err)
		var actual map[string]string
		require.NoError(t, json.Unmarshal(output, &actual))
		require.Equal(t, map[string]string{"PATH": "/usr/bin:/bin", "SANDBOX_POD_UID": "uid-a", "BUSINESS_ENV": "keep-me"}, actual)
	}
}

func TestSandboxStartupDoesNotInheritKubernetesVariables(t *testing.T) {
	for _, fuse := range []bool{false, true} {
		name := "ordinary"
		if fuse {
			name = "fuse"
		}
		t.Run(name, func(t *testing.T) {
			spec := runtime.SandboxSpec{ID: "ordinary-a", Image: "runtime:test"}
			target := "sleep"
			if fuse {
				spec = preparedFUSESpecForTest()
				target = fuseprotocol.ProbeBinary
			}
			var pod *corev1.Pod
			var err error
			if fuse {
				pod, err = buildPreparedFUSEPod("runtime", spec)
			} else {
				pod, err = buildOrdinaryPod("runtime", spec)
			}
			require.NoError(t, err)
			require.NotNil(t, pod.Spec.EnableServiceLinks)
			require.False(t, *pod.Spec.EnableServiceLinks)
			command := pod.Spec.Containers[0].Command
			targetIndex := -1
			for i, arg := range command {
				if arg == target {
					targetIndex = i
					break
				}
			}
			require.NotEqual(t, -1, targetIndex)
			// Replace the long-lived target with an environment-reporting child while
			// executing the actual launch prefix produced by the Pod builder.
			assertSandboxEnvironmentClean(t, command[:targetIndex])
		})
	}
}

func TestSandboxExecWireDoesNotInheritKubernetesVariables(t *testing.T) {
	type invoke func(context.Context, *kubernetes.Clientset, *rest.Config) error
	cases := map[string]invoke{
		"private_mounter": func(ctx context.Context, c *kubernetes.Clientset, r *rest.Config) error {
			e := spdyPodCommandExecutor{client: c, restConfig: r, namespace: "runtime"}
			_, err := e.Exec(ctx, "pod-a", workspaceMounterContainer, []string{mounterBinary, "health", "prepared"}, nil)
			return err
		},
		"download_file": func(ctx context.Context, c *kubernetes.Clientset, r *rest.Config) error {
			reader, err := downloadFileFromPod(ctx, c, r, "runtime", "pod-a", "/workspace/data")
			if err == nil {
				_, _ = io.Copy(io.Discard, reader)
				_ = reader.Close()
			}
			return err
		},
		"read_content": func(ctx context.Context, c *kubernetes.Clientset, r *rest.Config) error {
			rt := &Runtime{client: c, restConfig: r, namespace: "runtime"}
			reader, err := rt.ReadFileContent(ctx, "pod-a", "/workspace/data")
			if err == nil {
				_, _ = io.Copy(io.Discard, reader)
				_ = reader.Close()
			}
			return err
		},
		"sync": func(ctx context.Context, c *kubernetes.Clientset, r *rest.Config) error {
			_, err := execInPod(ctx, c, r, "runtime", "pod-a", runtime.ExecRequest{Command: "true"})
			return err
		},
		"stream": func(ctx context.Context, c *kubernetes.Clientset, r *rest.Config) error {
			events, err := execStreamInPod(ctx, c, r, "runtime", "pod-a", runtime.ExecRequest{Command: "true"})
			if err == nil {
				for range events {
				}
			}
			return err
		},
		"stream_tty": func(ctx context.Context, c *kubernetes.Clientset, r *rest.Config) error {
			events, err := execStreamInPod(ctx, c, r, "runtime", "pod-a", runtime.ExecRequest{Command: "true", LineBuffered: true})
			if err == nil {
				for range events {
				}
			}
			return err
		},
		"upload_stream": func(ctx context.Context, c *kubernetes.Clientset, r *rest.Config) error {
			return consumePodUploadStream(ctx, c, r, "runtime", "pod-a", "true", strings.NewReader("payload"))
		},
		"upload_archive": func(ctx context.Context, c *kubernetes.Clientset, r *rest.Config) error {
			return uploadArchiveToPod(ctx, c, r, "runtime", "pod-a", "/workspace", bytes.NewReader(nil))
		},
		"download_dir": func(ctx context.Context, c *kubernetes.Clientset, r *rest.Config) error {
			reader, err := downloadDirFromPod(ctx, c, r, "runtime", "pod-a", "/workspace")
			if err == nil {
				_, _ = io.Copy(io.Discard, reader)
				_ = reader.Close()
			}
			return err
		},
		"exec_pipe": func(ctx context.Context, c *kubernetes.Clientset, r *rest.Config) error {
			return execPipeInPod(ctx, c, r, "runtime", "pod-a", []string{"true"}, strings.NewReader("payload"))
		},
		"glob": func(ctx context.Context, c *kubernetes.Clientset, r *rest.Config) error {
			rt := &Runtime{client: c, restConfig: r, namespace: "runtime"}
			_, err := rt.GlobInfo(ctx, "pod-a", "/workspace/*")
			return err
		},
		"private_probe": func(ctx context.Context, c *kubernetes.Clientset, r *rest.Config) error {
			e := spdyPodCommandExecutor{client: c, restConfig: r, namespace: "runtime"}
			_, err := e.Exec(ctx, "pod-a", "sandbox", []string{fuseprotocol.ProbeBinary, "self-check"}, nil)
			return err
		},
	}
	for name, invoke := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "download_file" || name == "read_content" {
				original := probePodFile
				probePodFile = func(context.Context, kubernetes.Interface, *rest.Config, string, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
					return &runtime.ExecResult{Stdout: "regular file"}, nil
				}
				t.Cleanup(func() { probePodFile = original })
			}
			observed := make(chan []string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed <- r.URL.Query()["command"]
				http.Error(w, "test stops after exec payload capture", http.StatusBadRequest)
			}))
			defer server.Close()
			config := &rest.Config{Host: server.URL}
			client, err := kubernetes.NewForConfig(config)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			// A rejected upgrade is sufficient to inspect the real client-go exec wire
			// payload. No mock computes or installs the environment-cleaning command.
			_ = invoke(ctx, client, config)
			var command []string
			select {
			case command = <-observed:
			case <-ctx.Done():
				t.Fatal("no exec request was sent")
			}
			if name == "private_mounter" {
				require.Equal(t, []string{mounterBinary, "health", "prepared"}, command)
				return
			}
			// Commands may be sh, tar, cat or probe. The common prefix must sanitize
			// before any of them starts, including shells' initial /proc environment.
			index := -1
			for i, arg := range command {
				if arg == "sh" || arg == "tar" || arg == "cat" || arg == "true" || arg == "find" || arg == fuseprotocol.ProbeBinary {
					index = i
					break
				}
			}
			require.NotEqual(t, -1, index)
			assertSandboxEnvironmentClean(t, command[:index])
		})
	}
}
