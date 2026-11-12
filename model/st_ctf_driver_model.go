package model

// st_ctf_driver_model.go — the capture the flag driver's st_publish kinds (sn
// CAPTURETHEFLAG.md §3.3, §4.3, §4.6, §11.2). The driver has no table of its
// own: its CTF pool P = 2^32 + n is a vault2 pool, so the event mirror records
// its funding, roots and demands in the v2 tables under no_id P, and its
// finalized roots' accounts and amounts are recorded there as the provider
// pool's are; what it writes for readers (commitments, draws, each epoch's
// fixed result, ledgers, returned carry) is in the capture the flag tables
// (ctf_model.go).
const (
	// the census commitment, commitCtfCensus
	StPublishKindCtfCensus = "ctfcensus"
	// the CTF pool's close, commit, finalize and demand answers
	StPublishKindCtfClose    = "ctfclose"
	StPublishKindCtfCommit   = "ctfcommit"
	StPublishKindCtfFinalize = "ctffinalize"
	StPublishKindCtfAnswer   = "ctfanswer"
	// the carry contract's recycle of a final root's carry account
	StPublishKindCtfRecycle = "ctfrecycle"
	// a finalized root's accounts and amounts recorded for readers
	StPublishKindCtfRecord = "ctfrecord"
	// a shadow run's unsigned report, its reference in the reason
	StPublishKindCtfShadow = "ctfshadow"
)
