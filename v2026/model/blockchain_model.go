package model

import (
	"fmt"
	"strings"
)

type Blockchain int

const (
	SOL Blockchain = iota
	MATIC
	ETHEREUM
)

func (b Blockchain) String() string {
	return [...]string{"SOL", "MATIC", "ETHEREUM"}[b]
}

func ParseBlockchain(s string) (Blockchain, error) {
	s = strings.ToUpper(s)
	switch s {
	case "SOL":
		return SOL, nil
	case "SOLANA":
		return SOL, nil
	case "MATIC":
		return MATIC, nil
	case "POLY":
		return MATIC, nil
	case "POLYGON":
		return MATIC, nil
	case "ETH":
		return ETHEREUM, nil
	case "ETHEREUM":
		return ETHEREUM, nil
	default:
		return -1, fmt.Errorf("invalid Blockchain: %s", s)
	}
}
