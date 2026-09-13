package helm_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// Helm's offline lookup returns nothing. Inject an installed Deployment only
// into the temporary chart to exercise the actual upgrade rendering branch.
func TestDrainEnvironmentNamesAreUnique(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	chart, err := filepath.Abs("../../../deploy/helm/sandbox")
	require.NoError(t, err)
	for _, tc := range []struct {
		name, installedScope, expectedScope string
		installed                           bool
	}{
		{"offline", "", "aiadp-sandbox-fuse/sandbox-fuse", false},
		{"installed_scope", "original-runtime/sandbox-fuse", "original-runtime/sandbox-fuse", true},
		{"legacy_missing_scope", "", "aiadp-sandbox-fuse/sandbox-fuse", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempChart := t.TempDir()
			require.NoError(t, filepath.WalkDir(chart, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				rel, err := filepath.Rel(chart, path)
				if err != nil {
					return err
				}
				target := filepath.Join(tempChart, rel)
				if entry.IsDir() {
					return os.MkdirAll(target, 0o755)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if tc.installed && rel == "templates/pre-backend-change-drain.yaml" {
					env := []map[string]interface{}{{"name": "PRESERVED_SETTING", "value": "original-value"}}
					if tc.installedScope != "" {
						env = append(env, map[string]interface{}{"name": "SANDBOX_STATE_SCOPE", "value": tc.installedScope})
					}
					deployment := map[string]interface{}{"spec": map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{map[string]interface{}{"env": env}}}}}}
					fixture, err := json.Marshal(deployment)
					if err != nil {
						return err
					}
					lookup := `(lookup "apps/v1" "Deployment" .Release.Namespace (printf "%s-api" .Release.Name))`
					require.Contains(t, string(data), lookup)
					data = []byte(strings.Replace(string(data), lookup, "(fromJson `"+string(fixture)+"`)", 1))
				}
				return os.WriteFile(target, data, 0o644)
			}))
			for _, template := range []string{"pre-backend-change-drain.yaml", "pre-delete-drain.yaml", "deployment.yaml"} {
				output, err := exec.Command("helm", "template", "sandbox-fuse", tempChart, "--namespace", "aiadp-sandbox-fuse", "--show-only", "templates/"+template).CombinedOutput()
				require.NoError(t, err, "%s", output)
				var resource struct {
					Spec struct {
						Template struct {
							Spec struct {
								Containers []struct {
									Env []struct{ Name, Value string }
								}
							}
						}
					}
				}
				require.NoError(t, yaml.Unmarshal(output, &resource))
				require.NotEmpty(t, resource.Spec.Template.Spec.Containers)
				for _, container := range resource.Spec.Template.Spec.Containers {
					names := make(map[string]string)
					for _, env := range container.Env {
						_, duplicate := names[env.Name]
						require.False(t, duplicate, "%s duplicates env %s", template, env.Name)
						names[env.Name] = env.Value
					}
					expected := "aiadp-sandbox-fuse/sandbox-fuse"
					if template == "pre-backend-change-drain.yaml" {
						expected = tc.expectedScope
						if tc.installed {
							require.Equal(t, "original-value", names["PRESERVED_SETTING"])
						}
					}
					require.Equal(t, expected, names["SANDBOX_STATE_SCOPE"], template)
				}
			}
		})
	}
}
