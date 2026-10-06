// Public enrollment never selects the registration signer from submitted data.
package controller

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urnetwork/server/v2026"
)

// This owner requires a client-key root independently of optional whole-window
// roster publication; a missing or zero registration root cannot admit an SDK.
func TestProviderWorkOwnerPolicyRequiresIndependentRegistrationSigner(t *testing.T) {
	base := fmt.Sprintf("schema: %s\ndomain_hash: %x\nrequest_public_key: %x\n", ProviderWorkPolicySchema, [32]byte{131}, [32]byte{132})
	for _, signer := range []string{"", "not-an-address", "0x" + strings.Repeat("00", 20)} {
		undo := server.Vault.PushSimpleResource("provider_work.yml", []byte(base+"client_key_root_signer: "+signer+"\n"))
		_, _, err := LoadProviderWorkOwnerPolicy()
		undo()
		if err == nil {
			t.Fatal("enrollment lacked independent registration signer", signer)
		}
	}
	wanted := common.Address{133}
	undo := server.Vault.PushSimpleResource("provider_work.yml", []byte(base+"client_key_root_signer: "+wanted.Hex()+"\n"))
	defer undo()
	domain, signer, err := LoadProviderWorkOwnerPolicy()
	if err != nil || domain != ([32]byte{131}) || signer != wanted {
		t.Fatal("explicit registration owner changed", err)
	}
}
