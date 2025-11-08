package model

import (
	"fmt"
)

type Blockchain int

const (
	SOL Blockchain = iota
	MATIC
)

func (b Blockchain) String() string {
	return [...]string{"SOL", "MATIC"}[b]
}

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
