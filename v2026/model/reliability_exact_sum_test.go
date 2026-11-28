package model

import (
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// Numerics of mixed scale, as SUM(1.0/n) returns them, add to the exact
// rational total; float addition of the same reads does not.
func TestReliabilityExactSumAddsMixedScalesExactly(t *testing.T) {
	reads := []struct {
		digits string
		exp    int32
	}{
		// 1/3 at twenty digits, and sums of several such terms
		{digits: "33333333333333333333", exp: -20},
		{digits: "133333333333333333332", exp: -20},
		{digits: "5", exp: -1},
		{digits: "7", exp: 0},
		{digits: "0", exp: 0},
		{digits: "-25", exp: -2},
		{digits: "142857142857142857143", exp: -21},
	}
	sum := reliabilityExactSum{}
	want := new(big.Rat)
	floatSum := 0.0
	for _, read := range reads {
		value, ok := new(big.Int).SetString(read.digits, 10)
		if !ok {
			t.Fatal("bad fixture digits", read.digits)
		}
		sum.add(pgtype.Numeric{Int: value, Exp: read.exp, Valid: true})
		term := new(big.Rat).SetInt(value)
		scale := new(big.Rat).SetInt(reliabilityExactSumPow10(-read.exp))
		if 0 < read.exp {
			term.Mul(term, new(big.Rat).SetInt(reliabilityExactSumPow10(read.exp)))
		} else {
			term.Quo(term, scale)
		}
		want.Add(want, term)
		termFloat, _ := term.Float64()
		floatSum += termFloat
	}
	got := sum.numeric()
	gotRat := new(big.Rat).SetInt(got.Int)
	gotRat.Quo(gotRat, new(big.Rat).SetInt(reliabilityExactSumPow10(-got.Exp)))
	if gotRat.Cmp(want) != 0 {
		t.Fatalf("exact sum = %s, want %s", gotRat.FloatString(25), want.FloatString(25))
	}
	if wantFloat, _ := want.Float64(); wantFloat == floatSum {
		t.Fatal("fixture does not distinguish exact from float addition")
	}

	var empty reliabilityExactSum
	if zero := empty.numeric(); !zero.Valid || zero.Int.Sign() != 0 {
		t.Fatalf("empty sum = %+v, want zero", zero)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a NaN read was added")
		}
	}()
	empty.add(pgtype.Numeric{NaN: true, Valid: true})
}
