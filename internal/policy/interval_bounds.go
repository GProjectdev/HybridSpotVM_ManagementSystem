package policy

import (
	"fmt"
	"math"
)

// An omitted operational maximum does not cap the cost optimum at 600 seconds.
// The remaining upper bound is the member scheduler's int32 representation.
func checkpointIntervalBounds(cp CheckpointPolicy) (int64, int64, error) {
	lower := maxInt64(1, cp.MinIntervalSeconds)
	upper := cp.MaxIntervalSeconds
	if upper <= 0 {
		upper = math.MaxInt32
	}
	if cp.MinIntervalSeconds < 0 || cp.MaxIntervalSeconds < 0 || lower > upper || upper > math.MaxInt32 {
		return 0, 0, fmt.Errorf("checkpoint bounds must satisfy 1 <= min <= max <= %d", math.MaxInt32)
	}
	return lower, upper, nil
}
