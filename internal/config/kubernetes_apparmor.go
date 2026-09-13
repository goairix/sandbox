package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation"
)

var immutableLoaderProfile = regexp.MustCompile(`^sandbox-fuse-[a-f0-9]{64}$`)

func readKubernetesNodeSelector(path string, v *viper.Viper) (map[string]string, error) {
	const key = "runtime.kubernetes.node_selector"
	if text, present := os.LookupEnv("SANDBOX_RUNTIME_KUBERNETES_NODE_SELECTOR"); present {
		d := json.NewDecoder(bytes.NewBufferString(text))
		token, err := d.Token()
		if err != nil || token != json.Delim('{') {
			return nil, fmt.Errorf("config: node_selector env must be a JSON object")
		}
		result := map[string]string{}
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return nil, err
			}
			name, ok := token.(string)
			if !ok {
				return nil, fmt.Errorf("config: node_selector key must be a string")
			}
			if _, exists := result[name]; exists {
				return nil, fmt.Errorf("config: duplicate node_selector key")
			}
			var raw any
			if err := d.Decode(&raw); err != nil {
				return nil, err
			}
			value, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("config: node_selector values must be strings")
			}
			result[name] = value
		}
		if _, err := d.Token(); err != nil {
			return nil, err
		}
		if _, err := d.Token(); err != io.EOF {
			return nil, fmt.Errorf("config: trailing node_selector JSON")
		}
		return result, nil
	}
	// Read YAML/JSON keys from the original document, not Viper's lowercased map.
	extension := filepath.Ext(path)
	if path != "" && (extension == ".yaml" || extension == ".yml" || extension == ".json") {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var document yaml.Node
		if err := yaml.Unmarshal(data, &document); err != nil {
			return nil, err
		}
		node := &document
		if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
			node = node.Content[0]
		}
		for _, name := range []string{"runtime", "kubernetes", "node_selector"} {
			if node.Kind == yaml.AliasNode {
				node = node.Alias
			}
			var child *yaml.Node
			if node.Kind == yaml.MappingNode {
				for i := 0; i < len(node.Content); i += 2 {
					if node.Content[i].Tag == "!!merge" && len(v.GetStringMap(key)) != 0 {
						return nil, fmt.Errorf("config: YAML merge on node_selector structural path is unsupported; use an explicit selector mapping")
					}
					if strings.EqualFold(node.Content[i].Value, name) {
						child = node.Content[i+1]
					}
				}
			}
			if child == nil {
				return map[string]string{}, nil
			}
			node = child
		}
		if node.Kind == yaml.AliasNode {
			node = node.Alias
		}
		if node.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("config: node_selector YAML must be a mapping")
		}
		result := map[string]string{}
		for i := 0; i < len(node.Content); i += 2 {
			k, value := node.Content[i], node.Content[i+1]
			if k.Tag != "!!str" || value.Tag != "!!str" {
				return nil, fmt.Errorf("config: node_selector YAML keys and values must be strings")
			}
			if _, exists := result[k.Value]; exists {
				return nil, fmt.Errorf("config: duplicate node_selector YAML key")
			}
			result[k.Value] = value.Value
		}
		return result, nil
	}
	raw := v.Get(key)
	result := map[string]string{}
	switch values := raw.(type) {
	case map[string]string:
		for k, v := range values {
			result[k] = v
		}
	case map[string]any:
		for k, raw := range values {
			value, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("config: node_selector values must be strings")
			}
			result[k] = value
		}
	case nil:
	default:
		return nil, fmt.Errorf("config: node_selector must be a string mapping")
	}
	return result, nil
}

func (c *Config) normalizeKubernetesAppArmor() error {
	k := &c.Runtime.Kubernetes
	if len(k.NodeSelector) > 64 {
		return fmt.Errorf("config: Kubernetes node_selector exceeds 64 entries")
	}
	selector := make(map[string]string, len(k.NodeSelector)+1)
	for key, value := range k.NodeSelector {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return fmt.Errorf("config: invalid Kubernetes node_selector label")
		}
		selector[key] = value
	}
	if k.AppArmorLoaderName != "" {
		if c.Runtime.Type != "kubernetes" || !c.Workspace.MountModeEnabled("fuse") || c.Workspace.AllowMissingLSMForKind || !immutableLoaderProfile.MatchString(c.Workspace.Backend.LSMProfile) {
			return fmt.Errorf("config: AppArmor loader requires Kubernetes FUSE with immutable confined profile and no kind bypass")
		}
		if len(validation.IsDNS1123Subdomain(k.AppArmorLoaderName)) != 0 || k.AppArmorLoaderNamespace == "" || len(validation.IsDNS1123Label(k.AppArmorLoaderNamespace)) != 0 || k.AppArmorLoaderTimeoutSeconds <= 0 || k.AppArmorLoaderTimeoutSeconds > 600 {
			return fmt.Errorf("config: AppArmor loader name, namespace or timeout invalid")
		}
		if value, ok := selector["kubernetes.io/os"]; ok && value != "linux" {
			return fmt.Errorf("config: AppArmor loader node_selector conflicts with Linux")
		}
		selector["kubernetes.io/os"] = "linux"
		if len(selector) > 64 {
			return fmt.Errorf("config: effective Linux node_selector exceeds 64 entries")
		}
	}
	k.NodeSelector = selector
	return nil
}
