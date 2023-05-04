package client


type Event struct {
}


type ClientKernel struct {
}

func NewClientKernel() *ClientKernel {
	return nil
}


// returns a delay to process the next event
func (self *ClientKernel) Next(events []Event) (nativeEvents []Event, timeout int64) {

	return []Event{}, 0
}

