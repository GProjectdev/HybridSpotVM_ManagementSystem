package policy

import (
	"fmt"
	"math/big"
)

// optimalIntegerInterval minimizes a/x + max(0, b/x-d) + c*x on [lower, upper].
// Ties favor the smaller interval, except c == 0 uses the operational upper bound.
func optimalIntegerInterval(a, b, d, c float64, lower, upper int64) (int64, error) {
	const maxExactInteger int64 = 1<<53 - 1
	if !finite(a) || !finite(b) || !finite(d) || !finite(c) || a < 0 || b < 0 || d <= 0 || c < 0 {
		return 0, fmt.Errorf("interval coefficients must be finite with a,b,c >= 0 and d > 0")
	}
	if lower < 1 || upper < lower || upper > maxExactInteger {
		return 0, fmt.Errorf("interval bounds must satisfy 1 <= lower <= upper <= %d", maxExactInteger)
	}
	if c == 0 {
		return upper, nil
	}
	if (a == 0 && b == 0) || lower == upper {
		return lower, nil
	}

	// Exact binary-float rationals avoid overflow in a+b, root ratios, and costs,
	// and preserve comparisons even when adjacent large intervals look identical
	// at float64 precision. Work is independent of the interval's width.
	ar := new(big.Rat).SetFloat64(a)
	br := new(big.Rat).SetFloat64(b)
	dr := new(big.Rat).SetFloat64(d)
	cr := new(big.Rat).SetFloat64(c)
	rightRootSquared := new(big.Rat).Quo(ar, cr)
	leftRootSquared := new(big.Rat).Quo(new(big.Rat).Add(ar, br), cr)
	kink := new(big.Rat).Quo(br, dr)
	minimumSquared := new(big.Rat).Mul(kink, kink)

	// The convex continuous minimizer is sqrt((a+b)/c) below the kink,
	// sqrt(a/c) above it, or the kink itself. Clamp in squared coordinates
	// so no rounded square root can discard an adjacent integer candidate.
	if minimumSquared.Cmp(leftRootSquared) > 0 {
		minimumSquared.Set(leftRootSquared)
	} else if minimumSquared.Cmp(rightRootSquared) < 0 {
		minimumSquared.Set(rightRootSquared)
	}
	square := func(x int64) *big.Rat {
		r := new(big.Rat).SetInt64(x)
		return r.Mul(r, r)
	}
	if minimumSquared.Cmp(square(lower)) <= 0 {
		return lower, nil
	}
	if minimumSquared.Cmp(square(upper)) >= 0 {
		return upper, nil
	}

	// floor(sqrt(n/d)) == floor(sqrt(floor(n/d))) for nonnegative n/d.
	quotient := new(big.Int).Quo(minimumSquared.Num(), minimumSquared.Denom())
	floor := new(big.Int).Sqrt(quotient).Int64()
	if minimumSquared.Cmp(square(floor)) == 0 {
		return floor, nil
	}
	ceil := floor + 1
	cost := func(x int64) *big.Rat {
		xr := new(big.Rat).SetInt64(x)
		value := new(big.Rat).Quo(ar, xr)
		excess := new(big.Rat).Sub(new(big.Rat).Quo(br, xr), dr)
		if excess.Sign() > 0 {
			value.Add(value, excess)
		}
		return value.Add(value, new(big.Rat).Mul(cr, xr))
	}
	if cost(ceil).Cmp(cost(floor)) < 0 {
		return ceil, nil
	}
	return floor, nil
}
