


func NetworkCreate(create *NetworkCreate) (*NetworkCreateResult, error) {
	result, err := network_model.NetworkCreate(create, error)
	// fixme send validation email
	return result, err
}