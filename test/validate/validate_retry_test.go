package validate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWaitForValidationRetry(t *testing.T) {
	tests := []struct {
		name        string
		attempt     int
		maxAttempts int
		interval    time.Duration
		cancel      bool
		wantRetry   bool
		wantErr     error
	}{
		{
			name:        "attempts exhausted",
			attempt:     3,
			maxAttempts: 3,
		},
		{
			name:        "immediate retry",
			attempt:     1,
			maxAttempts: 3,
			wantRetry:   true,
		},
		{
			name:        "retry after interval",
			attempt:     1,
			maxAttempts: 3,
			interval:    time.Millisecond,
			wantRetry:   true,
		},
		{
			name:        "canceled",
			attempt:     1,
			maxAttempts: 3,
			interval:    time.Hour,
			cancel:      true,
			wantErr:     context.Canceled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			retry, err := waitForValidationRetry(ctx, test.attempt, test.maxAttempts, test.interval)
			require.ErrorIs(t, err, test.wantErr)
			require.Equal(t, test.wantRetry, retry)
		})
	}
}
