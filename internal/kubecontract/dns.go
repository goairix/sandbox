package kubecontract

import (
	"errors"
	"slices"
	"strconv"
	"strings"
)

// NodeLocalDNSInjectionLabel is the explicit Pod-level NodeLocal admission opt-out.
const NodeLocalDNSInjectionLabel = "node-local-dns-injection"

// DNSOption is an administrator-pinned resolver option, not a tenant API setting.
type DNSOption struct {
	Name  string `json:"name" yaml:"name" mapstructure:"name"`
	Value string `json:"value" yaml:"value" mapstructure:"value"`
}

// ValidateDNSOptions returns a name-sorted, independently owned validated list.
func ValidateDNSOptions(input []DNSOption) ([]DNSOption, error) {
	if len(input) == 0 {
		return nil, nil
	}
	if len(input) > 2 {
		return nil, errors.New("dns_options exceeds two entries")
	}
	result := slices.Clone(input)
	slices.SortFunc(result, func(a, b DNSOption) int { return strings.Compare(a.Name, b.Name) })
	for i, option := range result {
		if i > 0 && result[i-1].Name == option.Name {
			return nil, errors.New("dns_options contains duplicate names")
		}
		switch option.Name {
		case "single-request-reopen":
			if option.Value != "" {
				return nil, errors.New("dns_options switch value must be empty")
			}
		case "timeout":
			value, err := strconv.Atoi(option.Value)
			if err != nil || value < 1 || value > 30 || strconv.Itoa(value) != option.Value {
				return nil, errors.New("dns_options timeout must be a canonical integer from 1 to 30")
			}
		default:
			return nil, errors.New("dns_options contains an unsupported name")
		}
	}
	return result, nil
}
