package policy

import (
	"fmt"
	"math"
)

// The member scheduler stores intervalSeconds as int32. A finite default cap
// also makes the zero-preemption objective operationally well-defined.
func checkpointIntervalBounds(cp CheckpointPolicy) (int64, int64, error) {
	lower := maxInt64(1, cp.MinIntervalSeconds)
	upper := cp.MaxIntervalSeconds
	if upper <= 0 {
		upper = maxInt64(600, lower)
	}
	if cp.MinIntervalSeconds < 0 || cp.MaxIntervalSeconds < 0 || lower > upper || upper > math.MaxInt32 {
		return 0, 0, fmt.Errorf("checkpoint bounds must satisfy 1 <= min <= max <= %d", math.MaxInt32)
	}
	return lower, upper, nil
}
