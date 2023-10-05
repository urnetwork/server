package model


import (
	"bringyour.com/bringyour"
)

// FIXME just stub out, and focus on the connect server implementation


// create subscription (payment type, product type, date, duration months)

// get subscriptions for network

// move balance to escrow for contract (network_id, current_time)
// this means just creating a debit record

/*
// currently active subscriptions
func GetActiveSubscriptions() {

}



// purchase token, purchase signature
func UpdateGoogleSubscription() {

}
// if new, add a transfer balance
// if update, add a transfer balance for the new end time


// add a new prepay subscription
// prepay subs are valid for 12 months after payment
// add a balance
func AddPrepaySubscription() {

}


// subscription, start, end, balance
func AddTransferBalance() {

}


// contract_id, amount
// chooses the active subscription ordered by earliest end date with remaining balance to take balance from
func TransferEscrow() {

}


// the escrow might have been taken from multiple balances
func GetTransferEscrow(contractId) []*TransferEscrow {

}



// contract_id, payout
func SettleEscrow() {

}


// updates account balance and creates sweep records that can be used for a payment sweep
func EscrowSweep() {

}




// brings AccountBalance paid_balance up to date
func PaymentSweep() {
	// FIXME
}

*/



type TransferEscrow struct {
	// FIXME
	ContractId bringyour.Id
}


func CreateTransferEscrow(sourceId bringyour.Id, destinationId bringyour.Id, contractTransferBytes int) *TransferEscrow {
	// FIXME
	return nil
}


type TransferPair struct {
	// FIXME
}

func NewTransferPair(sourceId bringyour.Id, destinationId bringyour.Id) TransferPair {
	// FIXME
	return TransferPair{}
}

func NewUnorderedTransferPair(a bringyour.Id, b bringyour.Id) TransferPair {
	// FIXME sort, store in ascending
	return TransferPair{}
}


func GetOpenContractIds(sourceId bringyour.Id, destinationId bringyour.Id) []bringyour.Id {
	// FIXME
	return []bringyour.Id{}
}


func GetOpenContractIdsForSourceOrDestination(clientId bringyour.Id) map[TransferPair]map[bringyour.Id]bool {
	// FIXME
	return map[TransferPair]map[bringyour.Id]bool{}
}


/*
NewTransferPair

// ascending order
NewUnorderedTransferPair

GetOpenContractIds

// unordered transfer pairs as keys
// map[model.TransferPair]map[Id]bool
GetOpenContractIdsForSourceOrDestination

*/






