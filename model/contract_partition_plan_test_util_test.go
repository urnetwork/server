package model

import (
	"github.com/urnetwork/server"
)

// The table a plan's relation reads: once the contract tables are partitioned
// a scan names a partition, which stands for its contract table.
func planContractTable(relation string) string {
	if table, _, _, ok := server.ParseContractPartitionName(relation); ok {
		return table
	}
	return relation
}

// The table of a JSON plan node's relation.
func planNodeContractTable(node map[string]any) string {
	relation, _ := node["Relation Name"].(string)
	return planContractTable(relation)
}
