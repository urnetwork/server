package work

import (
	"fmt"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
)

func PopulateAccountWallets(clientSession *session.ClientSession,) {

	circleUCUsers := model.GetCircleUCUsers(clientSession.Ctx)

	if len(circleUCUsers) == 0 {
			fmt.Println("No users found")
			return
	}

	user := circleUCUsers[0]

	fmt.Println("User[0] ID: ", user.UserId.String())
	fmt.Println("Network[0] ID: ", user.NetworkId.String())
	fmt.Println("Circle User ID[0]: ", user.CircleUCUserId.String())

	userSession := session.NewLocalClientSession(clientSession.Ctx, "0.0.0.0:0", &jwt.ByJwt{
			NetworkId: user.NetworkId,
			UserId: user.UserId,
	})

	walletInfo, err := controller.FindCircleWallets(userSession)
	if err != nil {
			fmt.Println("Error: ", err)
			return
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