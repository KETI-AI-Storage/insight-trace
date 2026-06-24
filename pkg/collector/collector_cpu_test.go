package collector

import (
	"math"
	"testing"
	"time"
)

// computeCPUPercent must turn the *cumulative* cgroup CPU counter (in seconds)
// into a usage percentage by taking a delta over wall-clock time. The old code
// returned the raw cumulative counter as a "percent", which grows without bound.
func TestComputeCPUPercent(t *testing.T) {
	base := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name           string
		prevCPUSeconds float64
		prevTime       time.Time
		currCPUSeconds float64
		currTime       time.Time
		want           float64
	}{
		{
			name:           "first sample has no previous reading, cannot compute a rate",
			prevCPUSeconds: 0,
			prevTime:       time.Time{}, // zero value
			currCPUSeconds: 100.0,       // large cumulative counter
			currTime:       base,
			want:           0,
		},
		{
			name:           "half a core busy for one second is 50 percent",
			prevCPUSeconds: 100.0,
			prevTime:       base,
			currCPUSeconds: 100.5,
			currTime:       base.Add(1 * time.Second),
			want:           50,
		},
		{
			name:           "two full cores for one second is 200 percent",
			prevCPUSeconds: 10.0,
			prevTime:       base,
			currCPUSeconds: 12.0,
			currTime:       base.Add(1 * time.Second),
			want:           200,
		},
		{
			name:           "idle workload reports zero, not the cumulative counter",
			prevCPUSeconds: 500.0,
			prevTime:       base,
			currCPUSeconds: 500.0,
			currTime:       base.Add(2 * time.Second),
			want:           0,
		},
		{
			name:           "counter reset clamps to zero instead of going negative",
			prevCPUSeconds: 100.0,
			prevTime:       base,
			currCPUSeconds: 1.0,
			currTime:       base.Add(1 * time.Second),
			want:           0,
		},
		{
			name:           "non-positive elapsed time yields zero",
			prevCPUSeconds: 10.0,
			prevTime:       base,
			currCPUSeconds: 12.0,
			currTime:       base, // same instant
			want:           0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeCPUPercent(tc.prevCPUSeconds, tc.prevTime, tc.currCPUSeconds, tc.currTime)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("computeCPUPercent() = %v, want %v", got, tc.want)
			}
		})
	}
}
