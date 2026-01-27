package main

// "context"
// "testing"
// "time"

// "github.com/go-playground/assert/v2"

// "github.com/urnetwork/server/v2026"
// "github.com/urnetwork/server/v2026/model"

/*
func TestCreateContract(t *testing.T) { server.DefaultTestEnv().Run(func() {
	ctx, cancel := context.WithCancel(context.Background())

	settings := DefaultExchangeSettings()

	contractTransferByteCount := ByteCount(128 * 1024 * 1024)

	initialTransferBalance := ByteCount(32 * 1024 * 1024 * 1024)
	startTime := time.Now()
	endTime := startTime.Add(30 * 24 * time.Hour)


    networkIdA := server.NewId()
    userIdA := server.NewId()
    clientIdA := server.NewId()
    deviceIdA := server.NewId()

    networkIdB := server.NewId()
    userIdB := server.NewId()
    clientIdB := server.NewId()
    deviceIdB := server.NewId()

    model.Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
	model.Testing_CreateDevice(
		ctx,
		networkIdA,
		deviceIdA,
		clientIdA,
		"a",
		"a",
	)

    model.Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)
    model.Testing_CreateDevice(
		ctx,
		networkIdB,
		deviceIdB,
		clientIdB,
		"b",
		"b",
	)

    for _, networkId := range []server.Id{networkIdA, networkIdB} {
	    success := model.AddBasicTransferBalance(
	        ctx,
	        networkId,
	        initialTransferBalance,
	        startTime,
	        endTime,
	    )
	    assert.Equal(t, true, success)
	}


	residentContractManagerA := newResidentContractManager(
	    ctx,
	    cancel,
	    clientIdA,
	    settings,
	)

	residentContractManagerB := newResidentContractManager(
	    ctx,
	    cancel,
	    clientIdB,
	    settings,
	)


    relationship := model.ProvideModePublic

	contractId, contractTransferByteCount_, err := residentContractManagerA.CreateContract(clientIdA, clientIdB, false, contractTransferByteCount, relationship)
	assert.Equal(t, nil, err)
	assert.Equal(t, contractTransferByteCount, contractTransferByteCount_)
	// check that balance is deducted
	activeTransferBalance := model.GetActiveTransferBalanceByteCount(ctx, networkIdA)
	assert.Equal(t, initialTransferBalance - contractTransferByteCount, activeTransferBalance)

	activeTransferBalance = model.GetActiveTransferBalanceByteCount(ctx, networkIdB)
	assert.Equal(t, initialTransferBalance, activeTransferBalance)


	active := residentContractManagerA.HasActiveContract(clientIdA, clientIdB)
	assert.Equal(t, true, active)

	residentContractManagerA.CloseContract(contractId, clientIdA, contractTransferByteCount)


	active = residentContractManagerA.HasActiveContract(clientIdA, clientIdB)
	assert.Equal(t, false, active)


	_, _, err = residentContractManagerA.CreateContract(clientIdA, clientIdB, true, contractTransferByteCount, relationship)
	// fail due to no origin
	assert.NotEqual(t, nil, err)


	contractId2, contractTransferByteCount_, err := residentContractManagerB.CreateContract(clientIdB, clientIdA, false, contractTransferByteCount, relationship)
	assert.Equal(t, nil, err)
	assert.Equal(t, contractTransferByteCount, contractTransferByteCount_)
	// check that balance is deducted
	activeTransferBalance = model.GetActiveTransferBalanceByteCount(ctx, networkIdB)
	assert.Equal(t, initialTransferBalance - contractTransferByteCount, activeTransferBalance)

	activeTransferBalance = model.GetActiveTransferBalanceByteCount(ctx, networkIdA)
	assert.Equal(t, initialTransferBalance - contractTransferByteCount, activeTransferBalance)

	active = residentContractManagerB.HasActiveContract(clientIdB, clientIdA)
	assert.Equal(t, true, active)


	contractId3, contractTransferByteCount_, err := residentContractManagerA.CreateContract(clientIdA, clientIdB, true, contractTransferByteCount, relationship)
	assert.Equal(t, nil, err)
	assert.Equal(t, contractTransferByteCount, contractTransferByteCount_)
	// check that balance is deducted from the destination network (companion)
	activeTransferBalance = model.GetActiveTransferBalanceByteCount(ctx, networkIdB)
	assert.Equal(t, initialTransferBalance - 2 * contractTransferByteCount, activeTransferBalance)

	activeTransferBalance = model.GetActiveTransferBalanceByteCount(ctx, networkIdA)
	assert.Equal(t, initialTransferBalance - contractTransferByteCount, activeTransferBalance)

	active = residentContractManagerA.HasActiveContract(clientIdA, clientIdB)
	assert.Equal(t, true, active)

	residentContractManagerB.CloseContract(contractId2, clientIdB, contractTransferByteCount)
	active = residentContractManagerB.HasActiveContract(clientIdB, clientIdA)
	assert.Equal(t, false, active)

	residentContractManagerA.CloseContract(contractId3, clientIdA, contractTransferByteCount)
	active = residentContractManagerA.HasActiveContract(clientIdA, clientIdB)
	assert.Equal(t, false, active)

})}
*/
