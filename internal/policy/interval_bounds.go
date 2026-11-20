package policy

import (
	"fmt"
	"math"
)

// MaximumCheckpointIntervalSeconds is a policy safety invariant. A low hazard
// estimate is not a guarantee against an abrupt interruption. Operator settings
// may tighten this bound, but cannot disable or raise it.
const MaximumCheckpointIntervalSeconds int64 = 3600

func checkpointIntervalBounds(cp CheckpointPolicy) (int64, int64, error) {
	lower := maxInt64(1, cp.MinIntervalSeconds)
	upper := cp.MaxIntervalSeconds
	if cp.MinIntervalSeconds < 0 || cp.MaxIntervalSeconds < 0 || upper > math.MaxInt32 {
		return 0, 0, fmt.Errorf("invalid checkpoint bounds")
	}
	if upper == 0 || upper > MaximumCheckpointIntervalSeconds {
		upper = MaximumCheckpointIntervalSeconds
	}
	if lower > upper {
		return 0, 0, fmt.Errorf("checkpoint bounds must satisfy 1 <= min <= effective max <= %d", MaximumCheckpointIntervalSeconds)
	}
	return lower, upper, nil
}
