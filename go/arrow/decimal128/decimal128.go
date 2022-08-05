// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package decimal128

import (
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/apache/arrow/go/v10/arrow/internal/debug"
)

var (
	MaxDecimal128 = New(542101086242752217, 687399551400673280-1)
)

// Num represents a signed 128-bit integer in two's complement.
// Calculations wrap around and overflow is ignored.
//
// For a discussion of the algorithms, look at Knuth's volume 2,
// Semi-numerical Algorithms section 4.3.1.
//
// Adapted from the Apache ORC C++ implementation
type Num struct {
	lo uint64 // low bits
	hi int64  // high bits
}

// New returns a new signed 128-bit integer value.
func New(hi int64, lo uint64) Num {
	return Num{lo: lo, hi: hi}
}

// FromU64 returns a new signed 128-bit integer value from the provided uint64 one.
func FromU64(v uint64) Num {
	return New(0, v)
}

// FromI64 returns a new signed 128-bit integer value from the provided int64 one.
func FromI64(v int64) Num {
	switch {
	case v > 0:
		return New(0, uint64(v))
	case v < 0:
		return New(-1, uint64(v))
	default:
		return Num{}
	}
}

// FromBigInt will convert a big.Int to a Num, if the value in v has a
// BitLen > 128, this will panic.
func FromBigInt(v *big.Int) (n Num) {
	bitlen := v.BitLen()
	if bitlen > 127 {
		panic("arrow/decimal128: cannot represent value larger than 128bits")
	} else if bitlen == 0 {
		// if bitlen is 0, then the value is 0 so return the default zeroed
		// out n
		return
	}

	// if the value is negative, then get the high and low bytes from
	// v, and then negate it. this is because Num uses a two's compliment
	// representation of values and big.Int stores the value as a bool for
	// the sign and the absolute value of the integer. This means that the
	// raw bytes are *always* the absolute value.
	b := v.Bits()
	n.lo = uint64(b[0])
	if len(b) > 1 {
		n.hi = int64(b[1])
	}
	if v.Sign() < 0 {
		return n.Negate()
	}
	return
}

// Negate returns a copy of this Decimal128 value but with the sign negated
func (n Num) Negate() Num {
	n.lo = ^n.lo + 1
	n.hi = ^n.hi
	if n.lo == 0 {
		n.hi += 1
	}
	return n
}

func fromPositiveFloat64(v float64, prec, scale int32) (Num, error) {
	var pscale float64
	if scale >= -38 && scale <= 38 {
		pscale = float64PowersOfTen[scale+38]
	} else {
		pscale = math.Pow10(int(scale))
	}

	v *= pscale
	v = math.RoundToEven(v)
	maxabs := float64PowersOfTen[prec+38]
	if v <= -maxabs || v >= maxabs {
		return Num{}, fmt.Errorf("cannot convert %f to decimal128(precision=%d, scale=%d): overflow", v, prec, scale)
	}

	hi := math.Floor(math.Ldexp(float64(v), -64))
	low := v - math.Ldexp(hi, 64)
	return Num{hi: int64(hi), lo: uint64(low)}, nil
}

// FromFloat32 returns a new decimal128.Num constructed from the given float32
// value using the provided precision and scale. Will return an error if the
// value cannot be accurately represented with the desired precision and scale.
func FromFloat32(v float32, prec, scale int32) (Num, error) {
	return FromFloat64(float64(v), prec, scale)
}

// FromFloat64 returns a new decimal128.Num constructed from the given float64
// value using the provided precision and scale. Will return an error if the
// value cannot be accurately represented with the desired precision and scale.
func FromFloat64(v float64, prec, scale int32) (Num, error) {
	if v < 0 {
		dec, err := fromPositiveFloat64(-v, prec, scale)
		if err != nil {
			return dec, err
		}
		return dec.Negate(), nil
	}
	return fromPositiveFloat64(v, prec, scale)
}

// ToFloat32 returns a float32 value representative of this decimal128.Num,
// but with the given scale.
func (n Num) ToFloat32(scale int32) float32 {
	return float32(n.ToFloat64(scale))
}

func (n Num) tofloat64Positive(scale int32) float64 {
	const twoTo64 float64 = 1.8446744073709552e+19
	x := float64(n.hi) * twoTo64
	x += float64(n.lo)
	if scale >= -38 && scale <= 38 {
		return x * float64PowersOfTen[-scale+38]
	}

	return x * math.Pow10(-int(scale))
}

// ToFloat64 returns a float64 value representative of this decimal128.Num,
// but with the given scale.
func (n Num) ToFloat64(scale int32) float64 {
	if n.hi < 0 {
		return -n.Negate().tofloat64Positive(scale)
	}
	return n.tofloat64Positive(scale)
}

// LowBits returns the low bits of the two's complement representation of the number.
func (n Num) LowBits() uint64 { return n.lo }

// HighBits returns the high bits of the two's complement representation of the number.
func (n Num) HighBits() int64 { return n.hi }

// Sign returns:
//
// -1 if x <  0
//  0 if x == 0
// +1 if x >  0
func (n Num) Sign() int {
	if n == (Num{}) {
		return 0
	}
	return int(1 | (n.hi >> 63))
}

func toBigIntPositive(n Num) *big.Int {
	return (&big.Int{}).SetBits([]big.Word{big.Word(n.lo), big.Word(n.hi)})
}

// while the code would be simpler to just do lsh/rsh and add
// it turns out from benchmarking that calling SetBits passing
// in the words and negating ends up being >2x faster
func (n Num) BigInt() *big.Int {
	if n.Sign() < 0 {
		b := toBigIntPositive(n.Negate())
		return b.Neg(b)
	}
	return toBigIntPositive(n)
}

// Less returns true if the value represented by n is < other
func (n Num) Less(other Num) bool {
	return n.hi < other.hi || (n.hi == other.hi && n.lo < other.lo)
}

// IncreaseScaleBy returns a new decimal128.Num with the value scaled up by
// the desired amount. Must be 0 <= increase <= 38. Any data loss from scaling
// is ignored. If you wish to prevent data loss, use Rescale which will
// return an error if data loss is detected.
func (n Num) IncreaseScaleBy(increase int32) Num {
	debug.Assert(increase >= 0, "invalid increase scale for decimal128")
	debug.Assert(increase <= 38, "invalid increase scale for decimal128")

	v := scaleMultipliers[increase].BigInt()
	return FromBigInt(v.Mul(n.BigInt(), v))
}

// ReduceScaleBy returns a new decimal128.Num with the value scaled down by
// the desired amount and, if 'round' is true, the value will be rounded
// accordingly. Assumes 0 <= reduce <= 38. Any data loss from scaling
// is ignored. If you wish to prevent data loss, use Rescale which will
// return an error if data loss is detected.
func (n Num) ReduceScaleBy(reduce int32, round bool) Num {
	debug.Assert(reduce >= 0, "invalid reduce scale for decimal128")
	debug.Assert(reduce <= 38, "invalid reduce scale for decimal128")

	if reduce == 0 {
		return n
	}

	divisor := scaleMultipliers[reduce].BigInt()
	result, remainder := divisor.QuoRem(n.BigInt(), divisor, (&big.Int{}))
	if round {
		divisorHalf := scaleMultipliersHalf[reduce]
		if remainder.Abs(remainder).Cmp(divisorHalf.BigInt()) != -1 {
			result.Add(result, big.NewInt(int64(n.Sign())))
		}
	}
	return FromBigInt(result)
}

func (n Num) rescaleWouldCauseDataLoss(deltaScale int32, multiplier Num) (out Num, loss bool) {
	var (
		value, result, remainder *big.Int
	)
	value = n.BigInt()
	if deltaScale < 0 {
		debug.Assert(multiplier.lo != 0 || multiplier.hi != 0, "multiplier needs to not be zero")
		result, remainder = (&big.Int{}).QuoRem(value, multiplier.BigInt(), (&big.Int{}))
		return FromBigInt(result), remainder.Cmp(big.NewInt(0)) != 0
	}

	result = (&big.Int{}).Mul(value, multiplier.BigInt())
	out = FromBigInt(result)
	cmp := result.Cmp(value)
	if n.Sign() < 0 {
		loss = cmp == 1
	} else {
		loss = cmp == -1
	}
	return
}

// Rescale returns a new decimal128.Num with the value updated assuming
// the current value is scaled to originalScale with the new value scaled
// to newScale. If rescaling this way would cause data loss, an error is
// returned instead.
func (n Num) Rescale(originalScale, newScale int32) (out Num, err error) {
	if originalScale == newScale {
		return n, nil
	}

	deltaScale := newScale - originalScale
	absDeltaScale := int32(math.Abs(float64(deltaScale)))

	multiplier := scaleMultipliers[absDeltaScale]
	var wouldHaveLoss bool
	out, wouldHaveLoss = n.rescaleWouldCauseDataLoss(deltaScale, multiplier)
	if wouldHaveLoss {
		err = errors.New("rescale data loss")
	}
	return
}

// Abs returns a new decimal128.Num that contains the absolute value of n
func (n Num) Abs() Num {
	switch n.Sign() {
	case -1:
		return n.Negate()
	}
	return n
}

// FitsInPrecision returns true or false if the value currently held by
// n would fit within precision (0 < prec <= 38) without losing any data.
func (n Num) FitsInPrecision(prec int32) bool {
	debug.Assert(prec > 0, "precision must be > 0")
	debug.Assert(prec <= 38, "precision must be <= 38")
	return n.Abs().Less(scaleMultipliers[prec])
}

var (
	scaleMultipliers = [...]Num{
		FromU64(1),
		FromU64(10),
		FromU64(100),
		FromU64(1000),
		FromU64(10000),
		FromU64(100000),
		FromU64(1000000),
		FromU64(10000000),
		FromU64(100000000),
		FromU64(1000000000),
		FromU64(10000000000),
		FromU64(100000000000),
		FromU64(1000000000000),
		FromU64(10000000000000),
		FromU64(100000000000000),
		FromU64(1000000000000000),
		FromU64(10000000000000000),
		FromU64(100000000000000000),
		FromU64(1000000000000000000),
		FromU64(10000000000000000000),
		New(0, 10000000000000000000),
		New(5, 7766279631452241920),
		New(54, 3875820019684212736),
		New(542, 1864712049423024128),
		New(5421, 200376420520689664),
		New(54210, 2003764205206896640),
		New(542101, 1590897978359414784),
		New(5421010, 15908979783594147840),
		New(54210108, 11515845246265065472),
		New(542101086, 4477988020393345024),
		New(5421010862, 7886392056514347008),
		New(54210108624, 5076944270305263616),
		New(542101086242, 13875954555633532928),
		New(5421010862427, 9632337040368467968),
		New(54210108624275, 4089650035136921600),
		New(542101086242752, 4003012203950112768),
		New(5421010862427522, 3136633892082024448),
		New(54210108624275221, 12919594847110692864),
		New(542101086242752217, 68739955140067328),
		New(5421010862427522170, 687399551400673280),
	}

	scaleMultipliersHalf = [...]Num{
		FromU64(0),
		FromU64(5),
		FromU64(50),
		FromU64(500),
		FromU64(5000),
		FromU64(50000),
		FromU64(500000),
		FromU64(5000000),
		FromU64(50000000),
		FromU64(500000000),
		FromU64(5000000000),
		FromU64(50000000000),
		FromU64(500000000000),
		FromU64(5000000000000),
		FromU64(50000000000000),
		FromU64(500000000000000),
		FromU64(5000000000000000),
		FromU64(50000000000000000),
		FromU64(500000000000000000),
		FromU64(5000000000000000000),
		New(2, 13106511852580896768),
		New(27, 1937910009842106368),
		New(271, 932356024711512064),
		New(2710, 9323560247115120640),
		New(27105, 1001882102603448320),
		New(271050, 10018821026034483200),
		New(2710505, 7954489891797073920),
		New(27105054, 5757922623132532736),
		New(271050543, 2238994010196672512),
		New(2710505431, 3943196028257173504),
		New(27105054312, 2538472135152631808),
		New(271050543121, 6937977277816766464),
		New(2710505431213, 14039540557039009792),
		New(27105054312137, 11268197054423236608),
		New(271050543121376, 2001506101975056384),
		New(2710505431213761, 1568316946041012224),
		New(27105054312137610, 15683169460410122240),
		New(271050543121376108, 9257742014424809472),
		New(2710505431213761085, 343699775700336640),
	}

	float64PowersOfTen = [...]float64{
		1e-38, 1e-37, 1e-36, 1e-35, 1e-34, 1e-33, 1e-32, 1e-31, 1e-30, 1e-29,
		1e-28, 1e-27, 1e-26, 1e-25, 1e-24, 1e-23, 1e-22, 1e-21, 1e-20, 1e-19,
		1e-18, 1e-17, 1e-16, 1e-15, 1e-14, 1e-13, 1e-12, 1e-11, 1e-10, 1e-9,
		1e-8, 1e-7, 1e-6, 1e-5, 1e-4, 1e-3, 1e-2, 1e-1, 1e0, 1e1,
		1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10, 1e11,
		1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18, 1e19, 1e20, 1e21,
		1e22, 1e23, 1e24, 1e25, 1e26, 1e27, 1e28, 1e29, 1e30, 1e31,
		1e32, 1e33, 1e34, 1e35, 1e36, 1e37, 1e38,
	}
)
