// A compact review projection keeps raw signatures and connection locations out
// of standard output. The private archive remains the full provenance record.
package strecovery

// Source row/file counts distinguish empty-but-complete sources from omissions.
type SourceCount struct {
	Source   string `json:"source"`
	Intents  int    `json:"intents,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	Files    int    `json:"files,omitempty"`
}

// Every missing store membership identifies the exact retained EVM signature.
type MissingSignature struct {
	Hash   string   `json:"hash"`
	Stores []string `json:"stores"`
}

// False receipt/fee/authority fields are deliberate: source custody is the only
// completed phase, even when a database labels a transaction finalized.
type Inspection struct {
	CensusHash                  string             `json:"census_hash"`
	Databases                   []SourceCount      `json:"databases"`
	Stores                      []SourceCount      `json:"stores"`
	SignedTransactions          int                `json:"signed_transactions"`
	UnsignedIntents             int                `json:"unsigned_intents"`
	Excluded                    []ExcludedRecord   `json:"excluded"`
	OpaqueNativeFiles           int                `json:"opaque_native_files"`
	Missing                     []MissingSignature `json:"missing_from_stores"`
	Fees                        []FeeEnvelope      `json:"fee_envelopes"`
	CanonicalReceiptsReconciled bool               `json:"canonical_receipts_reconciled"`
	ActualFeesReconciled        bool               `json:"actual_fees_reconciled"`
	SpendingAuthorized          bool               `json:"spending_authorized"`
}

// Call only after collection or archive validation; this projection does not
// itself admit or authenticate a caller-supplied archive.
func (self *Archive) Inspect() Inspection {
	result := Inspection{CensusHash: self.CensusHash, Databases: []SourceCount{}, Stores: []SourceCount{}, SignedTransactions: len(self.Transactions), UnsignedIntents: len(self.Unsigned),
		OpaqueNativeFiles: self.OpaqueNativeFiles, Missing: []MissingSignature{}, Fees: self.Fees, Excluded: self.Excluded}
	for _, source := range self.Databases {
		result.Databases = append(result.Databases, SourceCount{Source: source.Source, Intents: len(source.Snapshot.Intents), Attempts: len(source.Snapshot.Attempts)})
	}
	for _, source := range self.Stores {
		result.Stores = append(result.Stores, SourceCount{Source: source.Source, Files: len(source.Files)})
	}
	for _, tx := range self.Transactions {
		if len(tx.MissingFromStores) > 0 {
			result.Missing = append(result.Missing, MissingSignature{Hash: tx.Hash, Stores: tx.MissingFromStores})
		}
	}
	return result
}
