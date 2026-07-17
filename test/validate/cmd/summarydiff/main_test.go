package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	testCheckName = "cns"
	testNodeName  = "node-1"
)

func TestAggregate(t *testing.T) {
	summary := validationSummary{
		Checks: []validationCheckEntry{
			{
				ExpectedCount:  3,
				ActualCount:    3,
				ValidationPass: true,
			},
			{
				ExpectedCount:  2,
				ActualCount:    3,
				MissingIPs:     []string{"10.0.0.2"},
				UnexpectedIPs:  []string{"10.0.0.9"},
				DuplicateIPs:   []string{"10.0.0.9"},
				ValidationPass: false,
			},
		},
	}

	stats := aggregate(summary)
	assert.Equal(t, 2, stats.TotalChecks)
	assert.Equal(t, 1, stats.FailedChecks)
	assert.Equal(t, 1, stats.MissingIPs)
	assert.Equal(t, 1, stats.UnexpectedIPs)
	assert.Equal(t, 1, stats.DuplicateIPs)
	assert.Equal(t, 5, stats.ExpectedIPsSum)
	assert.Equal(t, 6, stats.ActualIPsSum)
}

func TestCompareSummariesRejectsLostIPs(t *testing.T) {
	baseline := validationSummary{Checks: []validationCheckEntry{{
		CheckName:      testCheckName,
		NodeName:       testNodeName,
		ExpectedCount:  3,
		ActualCount:    3,
		ValidationPass: true,
	}}}
	candidate := validationSummary{Checks: []validationCheckEntry{{
		CheckName:      testCheckName,
		NodeName:       testNodeName,
		ExpectedCount:  0,
		ActualCount:    0,
		ValidationPass: true,
	}}}

	assert.Error(t, compareSummaries(baseline, candidate))
}

func TestCompareSummariesAllowsGrowth(t *testing.T) {
	baseline := validationSummary{Checks: []validationCheckEntry{{
		CheckName:      testCheckName,
		NodeName:       testNodeName,
		ExpectedCount:  3,
		ActualCount:    3,
		ValidationPass: true,
	}}}
	candidate := validationSummary{Checks: []validationCheckEntry{{
		CheckName:      testCheckName,
		NodeName:       testNodeName,
		ExpectedCount:  5,
		ActualCount:    5,
		ValidationPass: true,
	}}}

	assert.NoError(t, compareSummaries(baseline, candidate))
}

func TestCompareSummariesRejectsPersistentStateRegression(t *testing.T) {
	baseline := validationSummary{Checks: []validationCheckEntry{{
		CheckName:       testCheckName,
		NodeName:        testNodeName,
		ValidationPass:  true,
		StateBackend:    "bolt",
		DBFilePresent:   boolPointer(true),
		DBFileSizeBytes: int64Pointer(4096),
		Authority:       "bolt",
		SchemaVersion:   1,
		Generation:      10,
		EndpointCount:   3,
		AssignmentCount: 3,
		OwnerCount:      3,
	}}}
	candidate := validationSummary{Checks: []validationCheckEntry{{
		CheckName:       testCheckName,
		NodeName:        testNodeName,
		ValidationPass:  true,
		StateBackend:    "bolt",
		DBFilePresent:   boolPointer(true),
		DBFileSizeBytes: int64Pointer(4096),
		Authority:       "bolt",
		SchemaVersion:   1,
		Generation:      9,
		EndpointCount:   2,
		AssignmentCount: 2,
		OwnerCount:      2,
	}}}

	assert.Error(t, compareSummaries(baseline, candidate))
}

func TestCompareSummariesValidatesPersistentStorage(t *testing.T) {
	tests := []struct {
		name        string
		filePresent *bool
		fileSize    *int64
		wantErr     bool
	}{
		{
			name:     "missing presence metadata",
			fileSize: int64Pointer(4096),
			wantErr:  true,
		},
		{
			name:        "database file missing",
			filePresent: boolPointer(false),
			fileSize:    int64Pointer(4096),
			wantErr:     true,
		},
		{
			name:        "missing size metadata",
			filePresent: boolPointer(true),
			wantErr:     true,
		},
		{
			name:        "zero-sized database file",
			filePresent: boolPointer(true),
			fileSize:    int64Pointer(0),
			wantErr:     true,
		},
		{
			name:        "non-zero database file",
			filePresent: boolPointer(true),
			fileSize:    int64Pointer(4096),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline := validationSummary{Checks: []validationCheckEntry{{
				CheckName:      testCheckName,
				NodeName:       testNodeName,
				ValidationPass: true,
			}}}
			candidate := validationSummary{Checks: []validationCheckEntry{{
				CheckName:       testCheckName,
				NodeName:        testNodeName,
				ValidationPass:  true,
				StateBackend:    stateBackendBolt,
				DBFilePresent:   tt.filePresent,
				DBFileSizeBytes: tt.fileSize,
			}}}

			err := compareSummaries(baseline, candidate)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func boolPointer(value bool) *bool {
	return &value
}

func int64Pointer(value int64) *int64 {
	return &value
}
