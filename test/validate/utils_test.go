package validate

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

const testIPv4 = "10.224.0.55"

func makeCiliumEndpointJSON(t *testing.T, endpoints []CiliumEndpointStatus) []byte {
	t.Helper()
	b, err := json.Marshal(endpoints)
	if err != nil {
		t.Fatalf("failed to marshal endpoints: %v", err)
	}
	return b
}

func TestParseCiliumIngressIPs(t *testing.T) {
	tests := []struct {
		name     string
		output   []byte
		expected []string
	}{
		{
			name: "single IP",
			output: makeCiliumEndpointJSON(t, []CiliumEndpointStatus{
				{Status: NetworkingStatus{
					Labels:     EndpointLabels{SecurityRelevant: []string{reservedIngressLabel}},
					Networking: NetworkingAddressing{Addresses: []Address{{IPv4: testIPv4}}},
				}},
			}),
			expected: []string{testIPv4},
		},
		{
			name: "multiple IPs",
			output: makeCiliumEndpointJSON(t, []CiliumEndpointStatus{
				{Status: NetworkingStatus{
					Labels:     EndpointLabels{SecurityRelevant: []string{reservedIngressLabel}},
					Networking: NetworkingAddressing{Addresses: []Address{{IPv4: testIPv4}, {IPv4: "10.224.0.60"}}},
				}},
			}),
			expected: []string{testIPv4, "10.224.0.60"},
		},
		{
			name:     "empty output",
			output:   []byte(""),
			expected: nil,
		},
		{
			name:     "empty JSON array",
			output:   []byte("[]"),
			expected: nil,
		},
		{
			name: "non-ingress endpoint ignored",
			output: makeCiliumEndpointJSON(t, []CiliumEndpointStatus{
				{Status: NetworkingStatus{
					Labels:     EndpointLabels{SecurityRelevant: []string{"reserved:host"}},
					Networking: NetworkingAddressing{Addresses: []Address{{IPv4: testIPv4}}},
				}},
			}),
			expected: nil,
		},
		{
			name: "dualstack IPs",
			output: makeCiliumEndpointJSON(t, []CiliumEndpointStatus{
				{Status: NetworkingStatus{
					Labels:     EndpointLabels{SecurityRelevant: []string{reservedIngressLabel}},
					Networking: NetworkingAddressing{Addresses: []Address{{IPv4: testIPv4, IPv6: "fd00::1"}}},
				}},
			}),
			expected: []string{testIPv4, "fd00::1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCiliumIngressIPs(tt.output)
			if len(got) != len(tt.expected) {
				t.Fatalf("expected %d IPs, got %d: %v", len(tt.expected), len(got), got)
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("IP[%d]: expected %q, got %q", i, tt.expected[i], got[i])
				}
			}
		})
	}
}

func TestCompareIPsDetailed_Match(t *testing.T) {
	expected := map[string]string{
		"10.0.0.1": "pod-a",
		"10.0.0.2": "pod-b",
	}
	actual := []string{"10.0.0.1", "10.0.0.2"}

	result := compareIPsDetailed(expected, actual)
	assert.Equal(t, 2, result.ExpectedCount)
	assert.Equal(t, 2, result.ActualCount)
	assert.Empty(t, result.MissingIPs)
	assert.Empty(t, result.UnexpectedIPs)
	assert.Empty(t, result.DuplicateIPs)
	assert.False(t, result.HasMismatch())
}

func TestCompareIPsDetailed_MissingUnexpectedDuplicate(t *testing.T) {
	expected := map[string]string{
		"10.0.0.1": "pod-a",
		"10.0.0.2": "pod-b",
	}
	actual := []string{"10.0.0.1", "10.0.0.9", "10.0.0.9"}

	result := compareIPsDetailed(expected, actual)
	assert.Equal(t, 2, result.ExpectedCount)
	assert.Equal(t, 3, result.ActualCount)
	assert.Equal(t, []string{"10.0.0.2"}, result.MissingIPs)
	assert.Equal(t, []string{"10.0.0.9"}, result.UnexpectedIPs)
	assert.Equal(t, []string{"10.0.0.9"}, result.DuplicateIPs)
	assert.True(t, result.HasMismatch())
}
