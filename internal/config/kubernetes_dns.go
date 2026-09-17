package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

const dnsOptionsKey = "dns_options"
const dnsOptOutKey = "disable_node_local_dns_injection"

func dnsConfigError(key string) error {
	return fmt.Errorf("config: runtime.kubernetes.%s requires an explicit, strictly typed DNS admission configuration", key)
}

func readKubernetesDNSAdmission(path string, v *viper.Viper) (bool, []kubecontract.DNSOption, error) {
	var nodes [2]*yaml.Node
	var document yaml.Node
	var loaded bool
	for i, key := range []string{dnsOptOutKey, dnsOptionsKey} {
		if raw, present := os.LookupEnv("SANDBOX_RUNTIME_KUBERNETES_" + strings.ToUpper(key)); present {
			if (i == 0 && raw != "true" && raw != "false") || (i == 1 && (len(raw) > 1024 || !json.Valid([]byte(raw)))) {
				return false, nil, dnsConfigError(key)
			}
			var node yaml.Node
			if err := yaml.Unmarshal([]byte(raw), &node); err != nil || len(node.Content) != 1 {
				return false, nil, dnsConfigError(key)
			}
			nodes[i] = node.Content[0]
			continue
		}
		extension := strings.ToLower(filepath.Ext(path))
		if path != "" && (extension == ".yaml" || extension == ".yml" || extension == ".json") {
			if !loaded {
				data, err := os.ReadFile(path)
				if err != nil {
					return false, nil, dnsConfigError(key)
				}
				if err := yaml.Unmarshal(data, &document); err != nil {
					return false, nil, dnsConfigError(key)
				}
				loaded = true
			}
			node, found, unsafe := findDNSConfigNode(&document, []string{"runtime", "kubernetes", key}, 0)
			if found && unsafe {
				return false, nil, dnsConfigError(key)
			}
			if found {
				nodes[i] = node
			} else if v.InConfig("runtime.kubernetes." + key) {
				return false, nil, dnsConfigError(key)
			}
		} else if v.InConfig("runtime.kubernetes." + key) {
			var node yaml.Node
			if err := node.Encode(v.Get("runtime.kubernetes." + key)); err != nil {
				return false, nil, dnsConfigError(key)
			}
			nodes[i] = &node
		}
	}
	var disable bool
	if node := nodes[0]; node != nil {
		if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" || (node.Value != "true" && node.Value != "false") {
			return false, nil, dnsConfigError(dnsOptOutKey)
		}
		disable = node.Value == "true"
	}
	var options []kubecontract.DNSOption
	if node := nodes[1]; node != nil {
		if node.Kind != yaml.SequenceNode || len(node.Content) > 2 {
			return false, nil, dnsConfigError(dnsOptionsKey)
		}
		for _, item := range node.Content {
			if item.Kind != yaml.MappingNode || len(item.Content) != 4 {
				return false, nil, dnsConfigError(dnsOptionsKey)
			}
			option := kubecontract.DNSOption{}
			var nameSeen, valueSeen bool
			for j := 0; j < len(item.Content); j += 2 {
				key, value := item.Content[j], item.Content[j+1]
				if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
					return false, nil, dnsConfigError(dnsOptionsKey)
				}
				switch key.Value {
				case "name":
					if nameSeen {
						return false, nil, dnsConfigError(dnsOptionsKey)
					}
					nameSeen, option.Name = true, value.Value
				case "value":
					if valueSeen {
						return false, nil, dnsConfigError(dnsOptionsKey)
					}
					valueSeen, option.Value = true, value.Value
				default:
					return false, nil, dnsConfigError(dnsOptionsKey)
				}
			}
			if !nameSeen || !valueSeen {
				return false, nil, dnsConfigError(dnsOptionsKey)
			}
			options = append(options, option)
		}
	}
	options, err := kubecontract.ValidateDNSOptions(options)
	if err != nil {
		return false, nil, fmt.Errorf("config: runtime.kubernetes.%w", err)
	}
	return disable, options, nil
}

// findDNSConfigNode preserves old document semantics unless the requested new
// field is present. Any alias, merge, or duplicate on its path is then rejected.
func findDNSConfigNode(node *yaml.Node, path []string, depth int) (*yaml.Node, bool, bool) {
	if node == nil || depth > 32 {
		return nil, false, true
	}
	if len(path) == 0 {
		return node, true, false
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		return findDNSConfigNode(node.Content[0], path, depth+1)
	}
	if node.Kind == yaml.AliasNode {
		child, found, _ := findDNSConfigNode(node.Alias, path, depth+1)
		return child, found, true
	}
	if node.Kind == yaml.SequenceNode {
		for _, item := range node.Content {
			if child, found, _ := findDNSConfigNode(item, path, depth+1); found {
				return child, true, true
			}
		}
		return nil, false, true
	}
	if node.Kind != yaml.MappingNode {
		return nil, false, false
	}
	var child *yaml.Node
	var count int
	var merged bool
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Tag == "!!merge" {
			merged = true
		}
		if strings.EqualFold(node.Content[i].Value, path[0]) {
			child = node.Content[i+1]
			count++
		}
	}
	if child != nil {
		result, found, unsafe := findDNSConfigNode(child, path[1:], depth+1)
		return result, found, unsafe || merged || count > 1
	}
	if merged {
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Tag == "!!merge" {
				if result, found, _ := findDNSConfigNode(node.Content[i+1], path, depth+1); found {
					return result, true, true
				}
			}
		}
	}
	return nil, false, false
}

func (c *Config) normalizeKubernetesDNSAdmission() error {
	k := &c.Runtime.Kubernetes
	options, err := kubecontract.ValidateDNSOptions(k.DNSOptions)
	if err != nil {
		return fmt.Errorf("config: runtime.kubernetes.%w", err)
	}
	if c.Runtime.Type != "kubernetes" && (k.DisableNodeLocalDNSInjection || len(options) != 0) {
		return fmt.Errorf("config: Kubernetes DNS admission settings require runtime.type kubernetes")
	}
	k.DNSOptions = options
	return nil
}
