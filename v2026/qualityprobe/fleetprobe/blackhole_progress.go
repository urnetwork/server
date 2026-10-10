package fleetprobe

// BlackholeProgress is an identity-free event from an actual check worker.
// Completed means retained in this batch's memory, not measured or submitted.
type BlackholeProgress string

const (
	BlackholeStarted   BlackholeProgress = "started"
	BlackholeCompleted BlackholeProgress = "completed"
	BlackholeCanceled  BlackholeProgress = "canceled"
	BlackholeDiscarded BlackholeProgress = "discarded"
)
