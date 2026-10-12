// Exact sums of numeric reliability aggregates read in bounded statements.
//
// A bounded reliability pass reads one numeric SUM(1.0/valid_client_count) per
// client from each short read-only statement and adds the reads in memory.
// Numeric addition is exact, so the total equals the SUM one statement over
// every row returns, and the writer converts it to float8 in SQL where that
// statement would. Not safe for concurrent use.
package model

import (
	"errors"
	"math/big"

	"github.com/jackc/pgx/v5/pgtype"
)

var errReliabilityExactSumNotFinite = errors.New("reliability sum read a numeric that is not a finite value")

// An exact decimal value*10^exp. The zero value is zero.
type reliabilityExactSum struct {
	value *big.Int
	exp   int32
}

func reliabilityExactSumPow10(exp int32) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exp)), nil)
}

// Adds a finite numeric exactly, aligning both to the smaller exponent.
func (self *reliabilityExactSum) add(n pgtype.Numeric) {
	if !n.Valid || n.NaN || n.InfinityModifier != pgtype.Finite {
		panic(errReliabilityExactSumNotFinite)
	}
	addend := new(big.Int)
	if n.Int != nil {
		addend.Set(n.Int)
	}
	if self.value == nil {
		self.value = addend
		self.exp = n.Exp
		return
	}
	if n.Exp < self.exp {
		self.value.Mul(self.value, reliabilityExactSumPow10(self.exp-n.Exp))
		self.exp = n.Exp
	} else if self.exp < n.Exp {
		addend.Mul(addend, reliabilityExactSumPow10(n.Exp-self.exp))
	}
	self.value.Add(self.value, addend)
}

// The sum as a numeric parameter.
func (self *reliabilityExactSum) numeric() pgtype.Numeric {
	if self.value == nil {
		return pgtype.Numeric{Int: big.NewInt(0), Valid: true}
	}
	return pgtype.Numeric{Int: new(big.Int).Set(self.value), Exp: self.exp, Valid: true}
}
