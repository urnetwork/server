package endpoint

import (

	"bringyour.com/client"
)


type Endpoints struct {
	byClient *client.BringYourClient
	// fixme open/close endpoint types
}

func NewEndpoints(byClient *client.BringYourClient) *Endpoints {
	return &Endpoints{
		byClient: byClient,
	}
}
