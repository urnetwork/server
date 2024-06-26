package controller

import (

    "bringyour.com/bringyour/session"
    // "bringyour.com/bringyour/model"
)


type HelloResult struct {
    ClientAddress string `json:"client_address,omitempty"`
}


func Hello(
    session *session.ClientSession,
) (*HelloResult, error) {
    result := &HelloResult{
        ClientAddress: session.ClientAddress,
    }
    return result, nil
}
