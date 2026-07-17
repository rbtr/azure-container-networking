//go:build windows

// Copyright 2017 Microsoft. All rights reserved.
// MIT License

package platform

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestBootIDNativeQueryBoundary(t *testing.T) {
	const unsuccessfulNTStatus = 0xc0000001

	expected := windows.GUID{
		Data1: 0x00112233,
		Data2: 0x4455,
		Data3: 0x6677,
		Data4: [8]byte{0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
	}
	tests := []struct {
		name       string
		status     uint32
		want       string
		wantErr    string
		populateID bool
	}{
		{
			name:       "success",
			want:       expected.String(),
			populateID: true,
		},
		{
			name:    "native query failure",
			status:  unsuccessfulNTStatus,
			wantErr: "querying Windows boot ID: NTSTATUS 0xc0000001",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			query := func(info *bootEnvironmentInformation) uint32 {
				calls++
				if test.populateID {
					info.BootIdentifier = expected
				}
				return test.status
			}

			got, err := bootID(query)
			if test.wantErr != "" {
				require.EqualError(t, err, test.wantErr)
				require.Empty(t, got)
			} else {
				require.NoError(t, err)
				require.Equal(t, test.want, got)
			}
			require.Equal(t, 1, calls)
		})
	}
}
