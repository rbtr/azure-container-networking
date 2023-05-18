package v2

import "testing"

func TestCalculateTargetIPCount(t *testing.T) {
	tests := []struct {
		name   string
		demand int64
		batch  int64
		buffer float64
		want   int64
	}{
		{
			name:   "base case",
			demand: 0,
			batch:  16,
			buffer: .5,
			want:   16,
		},
		{
			name:   "2x demand",
			demand: 32,
			batch:  16,
			buffer: .5,
			want:   48,
		},
		{
			name:   "min batch",
			demand: 10,
			batch:  1,
			buffer: .5,
			want:   11,
		},
		{
			name:   "no minfree",
			demand: 10,
			batch:  16,
			buffer: 0,
			want:   16,
		},
		{
			name:   "no overhead",
			demand: 13,
			batch:  1,
			buffer: 0,
			want:   13,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := calculateTargetIPCount(tt.demand, tt.batch, tt.buffer); got != tt.want {
				t.Errorf("calculateTargetIPCount() = %v, want %v", got, tt.want)
			}
		})
	}
}
