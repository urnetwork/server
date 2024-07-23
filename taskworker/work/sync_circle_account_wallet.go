package work

import (
	"fmt"
	"time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/task"
)

type PopulateAccountWalletsArgs struct {
}

type PopulateAccountWalletsResult struct {
}

func SchedulePopulateAccountWallets(clientSession *session.ClientSession, tx bringyour.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		PopulateAccountWallets,
		&PopulateAccountWalletsArgs{},
		clientSession,
		task.RunOnce("export_stats"),
		task.RunAt(time.Now().Add(30 * time.Second)),
	)
}

func PopulateAccountWallets(
	populateAccountWallet *PopulateAccountWalletsArgs,
	clientSession *session.ClientSession,
) (*PopulateAccountWalletsResult, error) {

	circleUCUsers := model.GetCircleUCUsers(clientSession.Ctx)

	if len(circleUCUsers) == 0 {
			return nil, fmt.Errorf("no users found")
	}


	for _, user := range circleUCUsers {
		fmt.Println("User ID: ", user.UserId.String())
		fmt.Println("Network ID: ", user.NetworkId.String())
		fmt.Println("Circle User ID: ", user.CircleUCUserId)

		fmt.Println("User[0] ID: ", user.UserId.String())
		fmt.Println("Network[0] ID: ", user.NetworkId.String())
		fmt.Println("Circle User ID[0]: ", user.CircleUCUserId.String())
	
		userSession := session.NewLocalClientSession(clientSession.Ctx, "0.0.0.0:0", &jwt.ByJwt{
				NetworkId: user.NetworkId,
				UserId: user.UserId,
		})
	
		walletInfo, err := controller.FindCircleWallets(userSession)
		if err != nil {
				return nil, err
		}
	
		for i, wallet := range walletInfo {
			fmt.Println("Wallet ID: ", wallet.WalletId)
			fmt.Println("Token ID: ", wallet.TokenId)
			fmt.Println("Blockchain: ", wallet.Blockchain)
			fmt.Println("Blockchain Symbol: ", wallet.BlockchainSymbol)
			fmt.Println("Create Date: ", wallet.CreateDate)
			fmt.Println("Balance: ", wallet.BalanceUsdcNanoCents)
			fmt.Println("Address: ", wallet.Address)
			fmt.Println("###")

			walletId, err := bringyour.ParseId(wallet.WalletId)
			if err != nil {
					fmt.Printf("Error for wallet id %s: %v \n", wallet.WalletId, err)
					continue
			}

			accountWallet := &model.AccountWallet{
				WalletId: walletId,
				NetworkId: user.NetworkId,
				WalletType: model.WalletTypeCircleUserControlled,
				Blockchain: wallet.Blockchain,
				WalletAddress: wallet.Address,
				DefaultTokenType: "USDC",
				CreateTime: wallet.CreateDate,
			}
			model.CreateAccountWallet(clientSession.Ctx, accountWallet)
		
			// set the payout wallet
			if i == 0 {
					model.SetPayoutWallet(clientSession.Ctx, user.NetworkId, walletId)
			}
		}
	}

	return &PopulateAccountWalletsResult{}, nil

}

func PopulateAccountWalletsPost(
	populateAccountWallets *PopulateAccountWalletsArgs,
	populateAccountWalletsResult *PopulateAccountWalletsResult,
	clientSession *session.ClientSession,
	tx bringyour.PgTx,
) error {
	SchedulePopulateAccountWallets(clientSession, tx)
	return nil
}