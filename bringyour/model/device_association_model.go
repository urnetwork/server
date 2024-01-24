package model

import (
	"fmt"
	"net/url"
	"strings"
	"crypto/rand"
	"encoding/hex"
	"image/color"

	qrcode "github.com/skip2/go-qrcode"

	// TODO replace this with fewer works from a larger dictionary
	bip39 "github.com/tyler-smith/go-bip39"


	"bringyour.com/bringyour/session"
    "bringyour.com/bringyour"
)


// TODO basic implementation


type CodeType = string
const (
	CodeTypeAdopt CodeType = "adopt"
	CodeTypeShare CodeType = "share"
)


type DeviceAddArgs struct {
	Code string `json:"code"`
}

type DeviceAddResult struct {
	CodeType CodeType `json:"code_type,omitempty"`
	Code string `json:"code,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
	NetworkName string `json:"network_name,omitempty"`
	ClientId bringyour.Id `json:"client_id,omitempty"`
	Error *DeviceAddError `json:"error,omitempty"`
}

type DeviceAddError struct {
	Message string `json:"message"`
}

func DeviceAdd(
	add *DeviceAddArgs,
	clientSession *session.ClientSession,
) (*DeviceAddResult, error) {
	result := &DeviceAddResult{
		CodeType: CodeTypeShare,
		Code: add.Code,
		DeviceName: "A test device",
		NetworkName: "test",
		ClientId: bringyour.NewId(),
	}
	return result, nil
}


type DeviceCreateShareCodeArgs struct {
	ClientId bringyour.Id `json:"client_id"`
	DeviceName string `json:"device_name,omitempty"`
}

type DeviceCreateShareCodeResult struct {
	ShareCode string `json:"share_code,omitempty"`
	Error *DeviceCreateShareCodeError `json:"error,omitempty"`
}

type DeviceCreateShareCodeError struct {
	Message string `json:"message"`
}

func DeviceCreateShareCode(
	createShareCode *DeviceCreateShareCodeArgs,
	clientSession *session.ClientSession,
) (*DeviceCreateShareCodeResult, error) {
	// TODO: use fewer words from a larger dictionary
	// use 6 words from bip39 as the code (64 bits)
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return nil, err
	}
  	mnemonic, err := bip39.NewMnemonic(entropy)
  	if err != nil {
		return nil, err
	}

	words := strings.Split(mnemonic, " ")
	shareCode := strings.Join(words[0:6], " ")

	return &DeviceCreateShareCodeResult{
		ShareCode: shareCode,
	}, err
}


type DeviceShareCodeQRArgs struct {
	ShareCode string `json:"share_code"`
}

type DeviceShareCodeQRResult struct {
	PngBytes []byte `json:"png_bytes"`
}

func DeviceShareCodeQR(
	shareCodeQR *DeviceShareCodeQRArgs,
	clientSession *session.ClientSession,
) (*DeviceShareCodeQRResult, error) {
	addUrl := fmt.Sprintf(
		"https://app.bringyour.com/?add=%s",
		url.QueryEscape(shareCodeQR.ShareCode),
	)

	pngBytes, err := qrPngBytes(addUrl)
	if err != nil {
		return nil, err
	}

	return &DeviceShareCodeQRResult{
		PngBytes: pngBytes,
	}, err
}


type DeviceShareStatusArgs struct {
	ShareCode string `json:"share_code"`
}

type DeviceShareStatusResult struct {
	Pending bool `json:"pending,omitempty"`
	AssociatedNetworkName string `json:"associated_network_name,omitempty"`
	Error *DeviceShareStatusError `json:"error,omitempty"`
}

type DeviceShareStatusError struct {
	Message string `json:"message"`
}

func DeviceShareStatus(
	shareStatus *DeviceShareStatusArgs,
	clientSession *session.ClientSession,
) (*DeviceShareStatusResult, error) {

	result := &DeviceShareStatusResult{
		Pending: true,
		AssociatedNetworkName: "test",
	}

	return result, nil
}


type DeviceConfirmShareArgs struct {
	ShareCode string `json:"share_code"`
	Confirm bool `json:"confirm"`
}

type DeviceConfirmShareResult struct {
	Complete bool `json:"complete,omitempty"`
	AssociatedNetworkName string `json:"associated_network_name,omitempty"`
	Error *DeviceConfirmShareError `json:"error,omitempty"`
}

type DeviceConfirmShareError struct {
	Message string `json:"message"`
}

func DeviceConfirmShare(
	confirmShare *DeviceConfirmShareArgs,
	clientSession *session.ClientSession,
) (*DeviceConfirmShareResult, error) {

	result := &DeviceConfirmShareResult{
		Complete: confirmShare.Confirm,
		AssociatedNetworkName: "test",
	}

	return result, nil
}


type DeviceCreateAdoptCodeArgs struct {
	DeviceName string `json:"device_name"`
}

type DeviceCreateAdoptCodeResult struct {
	AdoptCode string `json:"adopt_code,omitempty"`
	AdoptSecret string `json:"share_code,omitempty"`
	DurationMinutes int `json:"duration_minutes,omitempty"`
	Error *DeviceCreateAdoptCodeResult `json:"error,omitempty"`
}

type DeviceCreateAdoptCodeError struct {
	Message string `json:"message"`
}

func DeviceCreateAdoptCode(
	createAdoptCode *DeviceCreateAdoptCodeArgs,
	clientSession *session.ClientSession,
) (*DeviceCreateAdoptCodeResult, error) {
	// TODO: use fewer words from a larger dictionary
	// use 6 words from bip39 as the code (64 bits)
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return nil, err
	}
  	mnemonic, err := bip39.NewMnemonic(entropy)
  	if err != nil {
		return nil, err
	}

	words := strings.Split(mnemonic, " ")
	adoptCode := strings.Join(words[0:6], " ")

	// 128 bits
	adoptSecretBytes := make([]byte, 128)
	if _, err := rand.Read(adoptSecretBytes); err != nil {
		return nil, err
	}
	adoptSecret := hex.EncodeToString(adoptSecretBytes)

	result := &DeviceCreateAdoptCodeResult{
		AdoptCode: adoptCode,
		AdoptSecret: adoptSecret,
		DurationMinutes: 60,
	}

	return result, nil
}


type DeviceAdoptCodeQRArgs struct {
	AdoptCode string `json:"share_code"`
}

type DeviceAdoptCodeQRResult struct {
	PngBytes []byte `json:"png_bytes"`
}

func DeviceAdoptCodeQR(
	adoptCodeQR *DeviceAdoptCodeQRArgs,
	clientSession *session.ClientSession,
) (*DeviceAdoptCodeQRResult, error) {
	addUrl := fmt.Sprintf(
		"https://app.bringyour.com/?add=%s",
		url.QueryEscape(adoptCodeQR.AdoptCode),
	)

	pngBytes, err := qrPngBytes(addUrl)
	if err != nil {
		return nil, err
	}

	return &DeviceAdoptCodeQRResult{
		PngBytes: pngBytes,
	}, err
}


type DeviceAdoptStatusArgs struct {
	AdoptCode string `json:"adopt_code"`
}

type DeviceAdoptStatusResult struct {
	Pending bool `json:"pending,omitempty"`
	AssociatedNetworkName string `json:"associated_network_name,omitempty"`
	Error *DeviceAdoptStatusError `json:"error,omitempty"`
}

type DeviceAdoptStatusError struct {
	Message string `json:"message"`
}

func DeviceAdoptStatus(
	adoptStatus *DeviceAdoptStatusArgs,
	clientSession *session.ClientSession,
) (*DeviceAdoptStatusResult, error) {

	result := &DeviceAdoptStatusResult{
		Pending: false,
		AssociatedNetworkName: "test",
	}

	return result, nil
}


type DeviceConfirmAdoptArgs struct {
	AdoptCode string `json:"adopt_code"`
	AdoptSecret string `json:"adopt_secret"`
	Confirm bool `json:"confirm"`
}

type DeviceConfirmAdoptResult struct {
	Complete bool `json:"complete,omitempty"`
	AssociatedNetworkName string `json:"associated_network_name,omitempty"`
	ByClientJwt string `json:"by_client_id,omitempty"`
	Error *DeviceConfirmAdoptError `json:"error,omitempty"`
}

type DeviceConfirmAdoptError struct {
	Message string `json:"message"`
}

func DeviceConfirmAdopt(
	confirmAdopt *DeviceConfirmAdoptArgs,
	clientSession *session.ClientSession,
) (*DeviceConfirmAdoptResult, error) {

	result := &DeviceConfirmAdoptResult{
		Complete: confirmAdopt.Confirm,
		AssociatedNetworkName: "test",
		ByClientJwt: "",
	}

	return result, nil
}


type DeviceRemoveAdoptCodeArgs struct {
	AdoptCode string `json:"adopt_code"`
	AdoptSecret string `json:"adopt_secret"`
}

type DeviceRemoveAdoptCodeResult struct {
	Error *DeviceRemoveAdoptCodeError `json:"error,omitempty"`
}

type DeviceRemoveAdoptCodeError struct {
	Message string `json:"message"`
}

func DeviceRemoveAdoptCode(
	removeAdoptCode *DeviceRemoveAdoptCodeArgs,
	clientSession *session.ClientSession,
) (*DeviceRemoveAdoptCodeResult, error) {

	result := &DeviceRemoveAdoptCodeResult{
	}

	return result, nil
}


type DeviceAssociationsResult struct {
	PendingAdoptionDevices []*DeviceAssociation `json:"pending_adoption_devices"`
	IncomingSharedDevices []*DeviceAssociation `json:"incoming_shared_devices"`
	OutgoingSharedDevices []*DeviceAssociation `json:"outgoing_shared_devices"`
}

type DeviceAssociation struct {
	Pending bool `json:"pending,omitempty"`
	Code string `json:"code,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
	ClientId string `json:"client_id,omitempty"`
	NetworkName string `json:"network_name,omitempty"`
}

func DeviceAssociations(
	clientSession *session.ClientSession,
) (*DeviceAssociationsResult, error) {

	result := &DeviceAssociationsResult{
		PendingAdoptionDevices: []*DeviceAssociation{},
		IncomingSharedDevices: []*DeviceAssociation{},
		OutgoingSharedDevices: []*DeviceAssociation{},
	}

	return result, nil
}


type DeviceRemoveAssociationArgs struct {
	Code string `json:"code"`
}

type DeviceRemoveAssociationResult struct {
	DeviceName string `json:"device_name,omitempty"`
	ClientId bringyour.Id `json:"client_id,omitempty"`
	NetworkName string `json:"network_name,omitempty"`
	Error *DeviceRemoveAssociationError `json:"error,omitempty"`
}

type DeviceRemoveAssociationError struct {
	Message string `json:"message"`
}

func DeviceRemoveAssociation(
	removeAssociation *DeviceRemoveAssociationArgs,
	clientSession *session.ClientSession,
) (*DeviceRemoveAssociationResult, error) {

	result := &DeviceRemoveAssociationResult{
		DeviceName: "test",
		ClientId: bringyour.NewId(),
		NetworkName: "test",
	}

	return result, nil
}


type DeviceSetAssociationNameArgs struct {
	Code string `json:"code"`
	DeviceName string `json:"device_name"`
}

type DeviceSetAssociationNameResult struct {
	Error *DeviceSetAssociationNameError `json:"error,omitempty"`
}

type DeviceSetAssociationNameError struct {
	Message string `json:"message"`
}

func DeviceSetAssociationName(
	setAssociationName *DeviceSetAssociationNameArgs,
	clientSession *session.ClientSession,
) (*DeviceSetAssociationNameResult, error) {

	result := &DeviceSetAssociationNameResult{
	}

	return result, nil
}


func qrPngBytes(url string) ([]byte, error) {
	q, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	q.ForegroundColor = color.RGBA{
		R: 29,
		G: 49,
		B: 80,
		A: 255,
	}
	q.BackgroundColor = color.RGBA{
		R: 255,
		G: 255,
		B: 255,
		A: 255,
	}

	pngBytes, err := q.PNG(256)
	if err != nil {
		return nil, err
	}

	// FIXME add bringyour logo to bottom right

	return pngBytes, err
}
