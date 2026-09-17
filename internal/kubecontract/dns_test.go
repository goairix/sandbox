package kubecontract

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDNSOptionsValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input []DNSOption
		valid bool
	}{
		{"empty", nil, true},
		{"flag", []DNSOption{{Name: "single-request-reopen"}}, true},
		{"min", []DNSOption{{Name: "timeout", Value: "1"}}, true},
		{"max", []DNSOption{{Name: "timeout", Value: "30"}}, true},
		{"both", []DNSOption{{Name: "timeout", Value: "2"}, {Name: "single-request-reopen"}}, true},
		{"duplicate", []DNSOption{{Name: "timeout", Value: "2"}, {Name: "timeout", Value: "2"}}, false},
		{"unknown", []DNSOption{{Name: "ndots", Value: "5"}}, false},
		{"case", []DNSOption{{Name: "TIMEOUT", Value: "2"}}, false},
		{"flag value", []DNSOption{{Name: "single-request-reopen", Value: "1"}}, false},
		{"zero", []DNSOption{{Name: "timeout", Value: "0"}}, false},
		{"large", []DNSOption{{Name: "timeout", Value: "31"}}, false},
		{"padding", []DNSOption{{Name: "timeout", Value: "02"}}, false},
		{"negative", []DNSOption{{Name: "timeout", Value: "-2"}}, false},
		{"missing", []DNSOption{{Name: "timeout"}}, false},
		{"overflow", []DNSOption{{Name: "timeout", Value: "999999999999999999999"}}, false},
		{"too many", []DNSOption{{Name: "timeout", Value: "2"}, {Name: "single-request-reopen"}, {Name: "timeout", Value: "2"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateDNSOptions(tc.input)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if len(tc.input) == 0 {
				require.Nil(t, got)
				return
			}
			original := tc.input[0]
			got[0].Value = "mutated"
			require.Equal(t, original, tc.input[0])
		})
	}
	got, err := ValidateDNSOptions([]DNSOption{{Name: "timeout", Value: "2"}, {Name: "single-request-reopen"}})
	require.NoError(t, err)
	require.Equal(t, "single-request-reopen", got[0].Name)
}
