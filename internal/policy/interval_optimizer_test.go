package policy

import (
	"math"
	"math/big"
	"math/rand"
	"testing"
)

func TestOptimalIntegerInterval(t *testing.T) {
	const maxInterval int64 = 1<<53 - 1
	const largeRoot int64 = 1 << 52
	tiny := math.SmallestNonzeroFloat64
	tests := []struct {
		name       string
		a, b, d, c float64
		lower      int64
		upper      int64
		want       int64
	}{
		{"below kink", 4, 12, 1, 1, 1, 20, 4},
		{"at kink", 4, 6, 1, .25, 1, 20, 6},
		{"above kink", 100, 4, 1, 1, 1, 20, 10},
		{"left root at kink", 4, 12, 3, 1, 1, 20, 4},
		{"right root at kink", 16, 4, 1, 1, 1, 20, 4},
		{"root floor wins", 5, 0, 1, 1, 1, 20, 2},
		{"root ceil wins", 8, 0, 1, 1, 1, 20, 3},
		{"kink floor wins", 0, 5, 2, 1, 1, 20, 2},
		{"kink ceil wins", 0, 5, 2, .25, 1, 20, 3},
		{"integer tie", 6, 0, 1, 1, 1, 20, 2},
		{"lower clip", 4, 12, 1, 1, 8, 20, 8},
		{"upper clip", 100, 4, 1, 1, 1, 7, 7},
		{"kink lower clip", 4, 6, 1, .25, 7, 20, 7},
		{"kink upper clip", 4, 6, 1, .25, 1, 5, 5},
		{"singleton", 4, 6, 1, .25, 9, 9, 9},
		{"zero risk", 4, 6, 1, 0, 1, 20, 20},
		{"zero risk plateau", 0, 6, 1, 0, 1, 20, 20},
		{"all zero costs", 0, 0, 1, 0, 1, maxInterval, maxInterval},
		{"zero overhead", 0, 0, 1, 1, 3, 20, 3},
		{"overflowing sum", math.MaxFloat64, math.MaxFloat64, 1, math.MaxFloat64 / 32, 1, 20, 8},
		{"overflowing root and kink", math.MaxFloat64, math.MaxFloat64, tiny, tiny, 1, maxInterval, maxInterval},
		{"underflowing root and kink", tiny, tiny, math.MaxFloat64, math.MaxFloat64, 1, maxInterval, 1},
		{"subnormal costs", 16 * tiny, 0, tiny, tiny, 1, 20, 4},
		{"subnormal kink", 0, 5 * tiny, 2 * tiny, tiny, 1, 20, 2},
		{"near kink tiny risk", 0, math.Nextafter(1, 2), 1, tiny, 1, 20, 2},
		{"overflowing candidate costs", math.MaxFloat64, math.MaxFloat64, 1, math.MaxFloat64 * .75, 1, 20, 2},
		{"largest allowed interval", math.Ldexp(1, 106), 0, 1, 1, maxInterval - 1, maxInterval, maxInterval},
		{"large root tie", math.Nextafter(math.Ldexp(1, 104), math.Inf(1)), 0, 1, 1, 1, maxInterval, largeRoot},
		{"large root ceil", math.Nextafter(math.Nextafter(math.Ldexp(1, 104), math.Inf(1)), math.Inf(1)), 0, 1, 1, 1, maxInterval, largeRoot + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := optimalIntegerInterval(tt.a, tt.b, tt.d, tt.c, tt.lower, tt.upper)
			if err != nil || got != tt.want {
				t.Fatalf("got (%d, %v), want (%d, nil)", got, err, tt.want)
			}
		})
	}
}

func TestOptimalIntegerIntervalInvalid(t *testing.T) {
	for i := 0; i < 4; i++ {
		for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
			args := [4]float64{1, 1, 1, 1}
			args[i] = value
			if _, err := optimalIntegerInterval(args[0], args[1], args[2], args[3], 1, 10); err == nil {
				t.Errorf("coefficient %d = %v accepted", i, value)
			}
		}
	}
	if _, err := optimalIntegerInterval(1, 1, 0, 1, 1, 10); err == nil {
		t.Error("zero d accepted")
	}
	for _, bounds := range [][2]int64{{0, 10}, {-1, 10}, {10, 9}, {1, 1 << 53}, {1 << 53, 1 << 53}, {1, math.MaxInt64}} {
		if _, err := optimalIntegerInterval(0, 0, 1, 0, bounds[0], bounds[1]); err == nil {
			t.Errorf("invalid bounds %v accepted by zero-risk shortcut", bounds)
		}
	}
	if _, err := optimalIntegerInterval(math.NaN(), 0, 1, 0, 1, 1); err == nil {
		t.Error("invalid coefficient accepted by shortcut")
	}
}

func TestOptimalIntegerIntervalBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(20261008))
	for trial := 0; trial < 1000; trial++ {
		a := float64(rng.Intn(1001)) / 8
		b := float64(rng.Intn(1001)) / 8
		d := float64(1+rng.Intn(100)) / 8
		c := float64(rng.Intn(101)) / 16
		// Common power-of-two scaling preserves the optimum while exercising
		// subnormal and near-overflow arithmetic without rounding the inputs.
		shift := []int{0, -1070, 1000}[trial%3]
		a, b, d, c = math.Ldexp(a, shift), math.Ldexp(b, shift), math.Ldexp(d, shift), math.Ldexp(c, shift)
		lower := int64(1 + rng.Intn(30))
		upper := lower + int64(rng.Intn(50))
		want := lower
		best := intervalOracleCost(a, b, d, c, lower)
		for x := lower + 1; x <= upper; x++ {
			value := intervalOracleCost(a, b, d, c, x)
			if value.Cmp(best) < 0 {
				want, best = x, value
			}
		}
		if c == 0 {
			want = upper
		}
		got, err := optimalIntegerInterval(a, b, d, c, lower, upper)
		if err != nil || got != want {
			t.Fatalf("trial %d: a=%g b=%g d=%g c=%g bounds=[%d,%d]: got (%d,%v), want %d", trial, a, b, d, c, lower, upper, got, err, want)
		}
	}
}

// Compute (a + max(0, b-d*x) + c*x*x)/x independently of root selection.
func intervalOracleCost(a, b, d, c float64, x int64) *big.Rat {
	xr := new(big.Rat).SetInt64(x)
	excess := new(big.Rat).Sub(new(big.Rat).SetFloat64(b), new(big.Rat).Mul(new(big.Rat).SetFloat64(d), xr))
	if excess.Sign() < 0 {
		excess.SetInt64(0)
	}
	numerator := new(big.Rat).Add(new(big.Rat).SetFloat64(a), excess)
	risk := new(big.Rat).Mul(new(big.Rat).SetFloat64(c), new(big.Rat).Mul(xr, xr))
	return numerator.Quo(numerator.Add(numerator, risk), xr)
}
