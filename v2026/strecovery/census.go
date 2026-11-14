// The census joins every retained signature by exact bytes while keeping each
// database attempt, source status and store filename as separate provenance.
package strecovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// A database image has no credentials; the selection retains only their pin.
type DatabaseImage struct {
	Source   string           `json:"source"`
	Snapshot DatabaseSnapshot `json:"snapshot"`
}

// Native extrinsics are retained as opaque evidence, never interpreted as EVM
// transactions. Both kinds retain exact bytes and their original filenames.
type StoreFile struct {
	Name string `json:"name"`
	Raw  []byte `json:"raw"`
}

// Files are sorted by name after reading one stable directory image.
type StoreImage struct {
	Source string      `json:"source"`
	Files  []StoreFile `json:"files"`
}

// Multiple database owners and same-nonce alternatives stay visible; a matching
// hash never causes provenance or an attempt generation to be discarded.
type Origin struct {
	Source        string `json:"source"`
	IntentId      string `json:"intent_id,omitempty"`
	Attempt       int    `json:"attempt,omitempty"`
	Kind          string `json:"kind,omitempty"`
	IntentStatus  string `json:"intent_status,omitempty"`
	AttemptStatus string `json:"attempt_status,omitempty"`
	Filename      string `json:"filename,omitempty"`
}

// All fee values are upper envelopes. No receipt, actual fee or nonce authority
// can be inferred from this offline record or the archived database statuses.
type Transaction struct {
	Hash              string   `json:"hash"`
	Raw               []byte   `json:"raw"`
	Role              string   `json:"role"`
	Sender            string   `json:"sender"`
	Nonce             uint64   `json:"nonce"`
	Type              uint8    `json:"type"`
	To                *string  `json:"to"`
	Value             string   `json:"value"`
	CalldataHash      string   `json:"calldata_hash"`
	GasLimit          uint64   `json:"gas_limit"`
	MaximumGasPrice   string   `json:"maximum_gas_price"`
	MaximumFee        string   `json:"maximum_fee"`
	Origins           []Origin `json:"origins"`
	MissingFromStores []string `json:"missing_from_stores"`
}

// Unsigned reservations have no transaction hash or fee; they cannot fill a
// missing signed nonce in the independently supplied completeness interval.
type UnsignedIntent struct {
	Source   string `json:"source"`
	IntentId string `json:"intent_id"`
	Role     string `json:"role"`
	Nonce    uint64 `json:"nonce"`
	Status   string `json:"status"`
}

// Unselected histories remain in the original source images and this explicit
// inventory. They never silently disappear or enter the restoration union.
type ExcludedRecord struct {
	Source  string `json:"source"`
	Record  string `json:"record"`
	Reason  string `json:"reason"`
	Hash    string `json:"hash,omitempty"`
	Sender  string `json:"sender,omitempty"`
	ChainId string `json:"chain_id,omitempty"`
}

// The sum counts each unique signature once; the nonce bound takes the maximum
// alternative fee per nonce because only one alternative can consume it.
type FeeEnvelope struct {
	Role                     string `json:"role"`
	SignedTransactions       int    `json:"signed_transactions"`
	DistinctNonces           int    `json:"distinct_nonces"`
	AllSignaturesMaximumFee  string `json:"all_signatures_maximum_fee"`
	DistinctNoncesMaximumFee string `json:"distinct_nonces_maximum_fee"`
}

// Raw source images are retained so inspection can reconstruct every derived
// record. The digest detects drift; it does not authenticate an operator.
type Archive struct {
	Schema            string           `json:"schema"`
	Selection         Config           `json:"selection"`
	Databases         []DatabaseImage  `json:"databases"`
	Stores            []StoreImage     `json:"stores"`
	Transactions      []Transaction    `json:"transactions"`
	Unsigned          []UnsignedIntent `json:"unsigned"`
	Excluded          []ExcludedRecord `json:"excluded"`
	Fees              []FeeEnvelope    `json:"fee_envelopes"`
	OpaqueNativeFiles int              `json:"opaque_native_files"`
	CensusHash        string           `json:"census_hash"`
}

// Collection is read-only. Every selected source must finish; a failed source,
// count bound, conflicting record or coverage gap returns no usable archive.
func Collect(ctx context.Context, selection Config, reader SnapshotReader) (*Archive, error) {
	if ctx == nil || reader == nil {
		return nil, errors.New("census context or database reader is absent")
	}
	if err := selection.Validate(); err != nil {
		return nil, err
	}
	archive := &Archive{Schema: ArchiveSchema, Selection: selection, Databases: []DatabaseImage{}, Stores: []StoreImage{}}
	for _, source := range selection.Databases {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		snapshot, err := reader.Snapshot(ctx, source, selection.Limits)
		if err != nil {
			return nil, err
		}
		if snapshot == nil {
			return nil, &Refusal{Source: source.Id, Cause: "database reader returned no complete snapshot"}
		}
		archive.Databases = append(archive.Databases, DatabaseImage{Source: source.Id, Snapshot: *snapshot})
		if err := archive.checkInputBounds(); err != nil {
			return nil, err
		}
	}
	for _, source := range selection.Stores {
		files, err := readStore(ctx, source, selection.Limits)
		if err != nil {
			return nil, err
		}
		archive.Stores = append(archive.Stores, StoreImage{Source: source.Id, Files: files})
		if err := archive.checkInputBounds(); err != nil {
			return nil, err
		}
	}
	if err := archive.rebuild(ctx); err != nil {
		return nil, err
	}
	archive.CensusHash = archive.hash()
	if archive.CensusHash == "" {
		return nil, errors.New("complete source image cannot be serialized into a census archive")
	}
	return archive, nil
}

// Recompute from original source images, not caller-edited summaries. This is
// also the offline replay used before every archive write and restoration.
func (self *Archive) Validate(ctx context.Context) error {
	if self == nil || ctx == nil || self.Schema != ArchiveSchema || !canonicalDigest(self.CensusHash) {
		return errors.New("recovery archive identity is absent")
	}
	if err := self.Selection.Validate(); err != nil {
		return err
	}
	if err := self.checkInputBounds(); err != nil {
		return err
	}
	if self.hash() != self.CensusHash {
		return errors.New("recovery archive census hash differs")
	}
	rebuilt := *self
	if err := rebuilt.rebuild(ctx); err != nil {
		return err
	}
	if rebuilt.hash() != self.CensusHash {
		return errors.New("recovery archive derived census differs from its source images")
	}
	return nil
}

// Applying global limits as each source arrives prevents per-source allowances
// from multiplying retention and applies the same bound to archive replay.
func (self *Archive) checkInputBounds() error {
	limits := self.Selection.Limits
	intents, attempts, files, total := 0, 0, 0, 0
	add := func(raw []byte) error {
		if len(raw) > limits.MaximumTransactionBytes || total > limits.MaximumTotalBytes-len(raw) {
			return errors.New("complete census exceeds its explicit byte bounds")
		}
		total += len(raw)
		return nil
	}
	for _, image := range self.Databases {
		intents += len(image.Snapshot.Intents)
		attempts += len(image.Snapshot.Attempts)
		if intents > limits.MaximumIntents || attempts > limits.MaximumAttempts {
			return &Refusal{Source: image.Source, Cause: "complete census exceeds its explicit row bounds"}
		}
		for _, intent := range image.Snapshot.Intents {
			if err := add(intent.Calldata); err != nil {
				return &Refusal{Source: image.Source, Record: intent.Id, Cause: err.Error()}
			}
		}
		for _, attempt := range image.Snapshot.Attempts {
			if err := add(attempt.Raw); err != nil {
				return &Refusal{Source: image.Source, Record: attempt.Hash, Cause: err.Error()}
			}
		}
	}
	for _, image := range self.Stores {
		files += len(image.Files)
		if files > limits.MaximumAttempts {
			return &Refusal{Source: image.Source, Cause: "complete census exceeds its explicit store file bound"}
		}
		for _, file := range image.Files {
			if err := add(file.Raw); err != nil {
				return &Refusal{Source: image.Source, Record: file.Name, Cause: err.Error()}
			}
		}
	}
	return nil
}

// The seal covers the entire fixed wire object except the seal itself.
func (self *Archive) hash() string {
	copy := *self
	copy.CensusHash = ""
	return objectDigest(copy)
}

// Missing source membership is recorded, while missing nonce coverage refuses
// completeness. Originals, replacements and cancellations are equal candidates.
func (self *Archive) rebuild(ctx context.Context) error {
	selection := self.Selection
	if len(self.Databases) != len(selection.Databases) || len(self.Stores) != len(selection.Stores) {
		return errors.New("recovery archive omits or adds a selected source")
	}
	self.Transactions, self.Unsigned, self.Excluded, self.Fees, self.OpaqueNativeFiles = []Transaction{}, []UnsignedIntent{}, []ExcludedRecord{}, []FeeEnvelope{}, 0
	roleByAddress := map[string]Role{}
	for _, role := range selection.Roles {
		roleByAddress[role.Address] = role
	}
	transactions := map[string]*Transaction{}
	intentCount, attemptCount, fileCount, totalBytes := 0, 0, 0, 0
	consumeBytes := func(source, record string, raw []byte) error {
		totalBytes += len(raw)
		if len(raw) > selection.Limits.MaximumTransactionBytes || totalBytes > selection.Limits.MaximumTotalBytes {
			return &Refusal{Source: source, Record: record, Cause: "complete census exceeds its explicit byte bounds"}
		}
		return ctx.Err()
	}
	add := func(source string, allowedRoles []string, raw []byte, origin Origin) (*types.Transaction, error) {
		record := origin.Filename
		if record == "" {
			record = fmt.Sprintf("%s/attempt/%d", origin.IntentId, origin.Attempt)
		}
		tx, sender, err := decodeTransaction(raw, selection.ChainId)
		if err != nil {
			return nil, &Refusal{Source: source, Record: record, Cause: err.Error()}
		}
		role, ok := roleByAddress[sender]
		if !ok || !contains(allowedRoles, role.Id) || tx.Nonce() < role.FirstNonce || tx.Nonce() >= role.NextNonce {
			return nil, &Refusal{Source: source, Record: record, Cause: "signature sender or nonce is outside the independently selected role scope"}
		}
		hash := tx.Hash().Hex()
		entry := transactions[hash]
		if entry == nil {
			var recipient *string
			if tx.To() != nil {
				value := strings.ToLower(tx.To().Hex())
				recipient = &value
			}
			entry = &Transaction{Hash: hash, Raw: bytes.Clone(raw), Role: role.Id, Sender: sender, Nonce: tx.Nonce(), Type: tx.Type(), To: recipient, Value: tx.Value().String(),
				CalldataHash: crypto.Keccak256Hash(tx.Data()).Hex(), GasLimit: tx.Gas(), MaximumGasPrice: tx.GasFeeCap().String(), MaximumFee: new(big.Int).Mul(new(big.Int).SetUint64(tx.Gas()), tx.GasFeeCap()).String(), Origins: []Origin{}, MissingFromStores: []string{}}
			transactions[hash] = entry
		} else if !bytes.Equal(entry.Raw, raw) {
			return nil, &Refusal{Source: source, Record: record, Cause: "same transaction hash has conflicting original bytes"}
		}
		entry.Origins = append(entry.Origins, origin)
		return tx, nil
	}
	for index, image := range self.Databases {
		source := selection.Databases[index]
		if image.Source != source.Id {
			return errors.New("database source order or identity differs")
		}
		intentCount, attemptCount = intentCount+len(image.Snapshot.Intents), attemptCount+len(image.Snapshot.Attempts)
		if intentCount > selection.Limits.MaximumIntents || attemptCount > selection.Limits.MaximumAttempts {
			return &Refusal{Source: source.Id, Cause: "complete census exceeds its explicit row bounds"}
		}
		intents, attempts, selected := map[string]Intent{}, map[string][]Attempt{}, map[string]bool{}
		for _, intent := range image.Snapshot.Intents {
			if err := consumeBytes(source.Id, intent.Id, intent.Calldata); err != nil {
				return err
			}
			if _, exists := intents[intent.Id]; exists {
				return &Refusal{Source: source.Id, Record: intent.Id, Cause: "duplicate intent identity"}
			}
			role, exists := roleByAddress[intent.From]
			if intent.Id == "" || intent.Generation < 0 || intent.IntentKey == "" || intent.LogicalKey == "" || intent.ChainId <= 0 || !canonicalHex(intent.Genesis, 32) || !canonicalHex(intent.From, 20) ||
				!canonicalHex(intent.To, 20) || intent.Nonce < 0 || intent.AttemptCount < 0 ||
				intent.CalldataHash != crypto.Keccak256Hash(intent.Calldata).Hex() || intent.Status == "" {
				return &Refusal{Source: source.Id, Record: intent.Id, Cause: "intent identity, network or calldata differs"}
			}
			selected[intent.Id] = uint64(intent.ChainId) == selection.ChainId && intent.Genesis == selection.Genesis && exists && contains(source.Roles, role.Id)
			if !selected[intent.Id] {
				self.Excluded = append(self.Excluded, ExcludedRecord{Source: source.Id, Record: intent.Id, Reason: "intent network or source role is outside the selected scope", Sender: intent.From, ChainId: fmt.Sprint(intent.ChainId)})
			}
			intents[intent.Id] = intent
		}
		for _, attempt := range image.Snapshot.Attempts {
			record := fmt.Sprintf("%s/attempt/%d", attempt.IntentId, attempt.Number)
			if err := consumeBytes(source.Id, record, attempt.Raw); err != nil {
				return err
			}
			intent, exists := intents[attempt.IntentId]
			if !exists {
				return &Refusal{Source: source.Id, Record: record, Cause: "signed attempt has no retained intent"}
			}
			tx, _, err := decodeTransaction(attempt.Raw, uint64(intent.ChainId))
			if err != nil {
				return &Refusal{Source: source.Id, Record: record, Cause: err.Error()}
			}
			if err := validateAttempt(intent, attempt, tx); err != nil {
				return &Refusal{Source: source.Id, Record: record, Cause: err.Error()}
			}
			if selected[intent.Id] {
				if _, err := add(source.Id, source.Roles, attempt.Raw, Origin{Source: source.Id, IntentId: intent.Id, Attempt: attempt.Number, Kind: attempt.Kind, IntentStatus: intent.Status, AttemptStatus: attempt.Status}); err != nil {
					return err
				}
			}
			attempts[intent.Id] = append(attempts[intent.Id], attempt)
		}
		for _, intent := range image.Snapshot.Intents {
			items := attempts[intent.Id]
			if len(items) != intent.AttemptCount {
				return &Refusal{Source: source.Id, Record: intent.Id, Cause: "retained attempt count differs from intent custody count"}
			}
			sort.Slice(items, func(i, j int) bool { return items[i].Number < items[j].Number })
			currentFound := intent.CurrentHash == nil
			for i, attempt := range items {
				if attempt.Number != i+1 {
					return &Refusal{Source: source.Id, Record: intent.Id, Cause: "attempt sequence has a missing or repeated original signature"}
				}
				if intent.CurrentHash != nil && *intent.CurrentHash == attempt.Hash {
					currentFound = true
				}
			}
			if !currentFound || len(items) > 0 && intent.CurrentHash == nil {
				return &Refusal{Source: source.Id, Record: intent.Id, Cause: "current transaction hash is absent from retained signatures"}
			}
			if len(items) == 0 && selected[intent.Id] {
				self.Unsigned = append(self.Unsigned, UnsignedIntent{Source: source.Id, IntentId: intent.Id, Role: roleByAddress[intent.From].Id, Nonce: uint64(intent.Nonce), Status: intent.Status})
			}
		}
	}
	for index, image := range self.Stores {
		source := selection.Stores[index]
		if image.Source != source.Id {
			return errors.New("store source order or identity differs")
		}
		fileCount += len(image.Files)
		if fileCount > selection.Limits.MaximumAttempts {
			return &Refusal{Source: source.Id, Cause: "complete store census exceeds its explicit file bound"}
		}
		seen := map[string]bool{}
		for _, file := range image.Files {
			if err := consumeBytes(source.Id, file.Name, file.Raw); err != nil {
				return err
			}
			kind, hash := storeFilename(file.Name)
			if kind == "" || seen[file.Name] || len(file.Raw) == 0 {
				return &Refusal{Source: source.Id, Record: file.Name, Cause: "store file is empty, repeated or has an unsupported name"}
			}
			seen[file.Name] = true
			if kind == "scale" {
				self.OpaqueNativeFiles++
				continue
			}
			tx, sender, err := decodeTransaction(file.Raw, 0)
			if err != nil {
				return &Refusal{Source: source.Id, Record: file.Name, Cause: err.Error()}
			}
			if tx.Hash().Hex() != hash {
				return &Refusal{Source: source.Id, Record: file.Name, Cause: "store filename differs from original transaction hash"}
			}
			role, selected := roleByAddress[sender]
			if !selected || !contains(source.Roles, role.Id) || tx.ChainId().Cmp(new(big.Int).SetUint64(selection.ChainId)) != 0 {
				self.Excluded = append(self.Excluded, ExcludedRecord{Source: source.Id, Record: file.Name, Reason: "signature chain or source role is outside the selected scope", Hash: hash, Sender: sender, ChainId: tx.ChainId().String()})
				continue
			}
			if _, err := add(source.Id, source.Roles, file.Raw, Origin{Source: source.Id, Filename: file.Name}); err != nil {
				return err
			}
		}
	}
	if len(transactions) > selection.Limits.MaximumAttempts {
		return errors.New("complete signature union exceeds its explicit transaction bound")
	}
	for _, tx := range transactions {
		for _, store := range selection.Stores {
			if !contains(store.Roles, tx.Role) {
				continue
			}
			found := false
			for _, origin := range tx.Origins {
				if origin.Source == store.Id {
					found = true
				}
			}
			if !found {
				tx.MissingFromStores = append(tx.MissingFromStores, store.Id)
			}
		}
		sort.Slice(tx.Origins, func(i, j int) bool { return objectDigest(tx.Origins[i]) < objectDigest(tx.Origins[j]) })
		self.Transactions = append(self.Transactions, *tx)
	}
	sort.Slice(self.Transactions, func(i, j int) bool { return self.Transactions[i].Hash < self.Transactions[j].Hash })
	sort.Slice(self.Unsigned, func(i, j int) bool {
		a, b := self.Unsigned[i], self.Unsigned[j]
		if a.Source == b.Source {
			return a.IntentId < b.IntentId
		}
		return a.Source < b.Source
	})
	for _, role := range selection.Roles {
		nonces, allFees := map[uint64]*big.Int{}, new(big.Int)
		count := 0
		for _, tx := range self.Transactions {
			if tx.Role != role.Id {
				continue
			}
			fee, _ := new(big.Int).SetString(tx.MaximumFee, 10)
			allFees.Add(allFees, fee)
			count++
			if current := nonces[tx.Nonce]; current == nil || current.Cmp(fee) < 0 {
				nonces[tx.Nonce] = fee
			}
		}
		maximumFees := new(big.Int)
		for nonce := role.FirstNonce; nonce < role.NextNonce; nonce++ {
			fee := nonces[nonce]
			if fee == nil {
				return &Refusal{Source: "role:" + role.Id, Record: fmt.Sprintf("nonce/%d", nonce), Cause: "complete source union is missing an expected signed nonce"}
			}
			maximumFees.Add(maximumFees, fee)
		}
		self.Fees = append(self.Fees, FeeEnvelope{Role: role.Id, SignedTransactions: count, DistinctNonces: len(nonces), AllSignaturesMaximumFee: allFees.String(), DistinctNoncesMaximumFee: maximumFees.String()})
	}
	return ctx.Err()
}

// Only the execution formats emitted by this operator are admitted. Signature
// recovery authenticates the sender, not canonical inclusion or genesis.
func decodeTransaction(raw []byte, chainId uint64) (*types.Transaction, string, error) {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return nil, "", errors.New("original transaction cannot be decoded")
	}
	canonical, err := tx.MarshalBinary()
	if err != nil || !bytes.Equal(canonical, raw) || !tx.Protected() || tx.ChainId().Sign() <= 0 || chainId != 0 && tx.ChainId().Cmp(new(big.Int).SetUint64(chainId)) != 0 ||
		tx.Type() != types.LegacyTxType && tx.Type() != types.DynamicFeeTxType || tx.Gas() == 0 || tx.GasFeeCap().Sign() <= 0 || tx.GasTipCap().Sign() < 0 || tx.GasTipCap().Cmp(tx.GasFeeCap()) > 0 {
		return nil, "", errors.New("transaction encoding, chain, type or fee envelope differs")
	}
	sender, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
	if err != nil {
		return nil, "", errors.New("original transaction signature cannot recover its sender")
	}
	return tx, strings.ToLower(sender.Hex()), nil
}

// Cancellation deliberately differs from the logical execution recipient/data.
// All other signed fields must match durable custody columns exactly.
func validateAttempt(intent Intent, attempt Attempt, tx *types.Transaction) error {
	sender, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
	if err != nil || strings.ToLower(sender.Hex()) != intent.From || tx.Nonce() != uint64(intent.Nonce) || tx.Hash().Hex() != attempt.Hash ||
		attempt.Number < 1 || attempt.GasLimit <= 0 || tx.Gas() != uint64(attempt.GasLimit) || tx.Value().Sign() != 0 || tx.To() == nil || attempt.Status == "" {
		return errors.New("signed transaction differs from its durable attempt identity or envelope")
	}
	decimalEqual := func(value *string, number *big.Int) bool { return value != nil && *value == number.String() }
	if tx.Type() == types.LegacyTxType {
		if !decimalEqual(attempt.GasPrice, tx.GasPrice()) || attempt.GasTipCap != nil || attempt.GasFeeCap != nil {
			return errors.New("legacy fee columns differ from original signature")
		}
	} else if attempt.GasPrice != nil || !decimalEqual(attempt.GasTipCap, tx.GasTipCap()) || !decimalEqual(attempt.GasFeeCap, tx.GasFeeCap()) {
		return errors.New("dynamic fee columns differ from original signature")
	}
	switch attempt.Kind {
	case "execution":
		if *tx.To() != common.HexToAddress(intent.To) || !bytes.Equal(tx.Data(), intent.Calldata) {
			return errors.New("execution signature differs from intent recipient or calldata")
		}
	case "cancellation":
		if *tx.To() != sender || len(tx.Data()) != 0 || tx.Gas() != 21000 {
			return errors.New("cancellation signature is not the original bounded self-transfer")
		}
	default:
		return errors.New("signed attempt kind is unsupported")
	}
	return nil
}

// Source roles are small bounded sets and retain their supplied order.
func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
