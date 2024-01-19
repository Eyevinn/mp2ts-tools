package app

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCalculateStepsInSlice(t *testing.T) {
	testCases := []struct {
		name   string
		values []int64
		want   []int64
	}{
		{
			name:   "Monotonically increasing values",
			values: []int64{1, 2, 3, 4, 5},
			want:   []int64{1, 1, 1, 1},
		},
		{
			name:   "Negative values",
			values: []int64{-5, -4, -3, -2, -1},
			want:   []int64{1, 1, 1, 1},
		},
		{
			name:   "Mixed values",
			values: []int64{-3, -2, -1, 0, 1, 2, 3},
			want:   []int64{1, 1, 1, 1, 1, 1},
		},
		{
			name:   "Big values",
			values: []int64{int64(math.Pow(2, 33)) - 5, int64(math.Pow(2, 33)) - 3, int64(math.Pow(2, 33)) - 1, 2, 4, 6},
			want:   []int64{2, 2, 3, 2, 2},
		},
		{
			name:   "Big values 2",
			values: []int64{int64(math.Pow(2, 33)) - 4, int64(math.Pow(2, 33)) - 2, 1, 3, 5, 7},
			want:   []int64{2, 3, 2, 2, 2},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := calculateStepsInSlice(tc.values)
			require.Equal(t, tc.want, got)
		})
	}
}
