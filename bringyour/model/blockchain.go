package model

import (
	"fmt"
)

// Step 1: Define a custom type
type Blockchain int

// Step 2: Define constants
const (
	SOL Blockchain = iota
	MATIC
)

// Step 3: Implement the Stringer interface
func (b Blockchain) String() string {
	return [...]string{"SOL", "MATIC"}[b]
}

// Optional: Function to parse a string to the enum type
func ParseBlockchain(s string) (Blockchain, error) {
	switch s {
	case "SOL":
		return SOL, nil
	case "MATIC":
		return MATIC, nil
	default:
		return -1, fmt.Errorf("invalid Blockchain: %s", s)
	}
}
