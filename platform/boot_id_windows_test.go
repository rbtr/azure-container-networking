//go:build windows

// Copyright 2017 Microsoft. All rights reserved.
// MIT License

package platform

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBootIDRegistryQueryBoundary(t *testing.T) {
	tests := []struct {
		name     string
		id       uint64
		queryErr error
		want     string
		wantErr  string
	}{
		{
			name: "success",
			id:   9,
			want: "9",
		},
		{
			name:     "registry query failure",
			queryErr: errors.New("registry unavailable"),
			wantErr:  "querying Windows boot ID: registry unavailable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			query := func() (uint64, error) {
				calls++
				return test.id, test.queryErr
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
