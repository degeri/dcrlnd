package lnwallet

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"decred.org/dcrwallet/v2/wallet/txauthor"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrec/secp256k1/v3"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrlnd/input"
	"github.com/decred/dcrlnd/lnwallet/chainfee"
)

// AddressType is an enum-like type which denotes the possible address types
// WalletController supports.
type AddressType uint8

const (
	// WitnessPubKey represents a p2wkh address.
	// TODO(decred) remove this type of address
	WitnessPubKey AddressType = iota

	// NestedWitnessPubKey represents a p2sh output which is itself a
	// nested p2wkh output.
	// TODO(decred) remove this type of address
	NestedWitnessPubKey

	// PubKeyHash represents a p2pkh address
	PubKeyHash

	// ScriptHash represents a p2sh address
	ScriptHash

	// UnknownAddressType represents an output with an unknown or non-standard
	// script.
	UnknownAddressType
)

var (
	// DefaultPublicPassphrase is the default public passphrase used for the
	// wallet.
	DefaultPublicPassphrase = []byte("public")

	// DefaultPrivatePassphrase is the default private passphrase used for
	// the wallet.
	DefaultPrivatePassphrase = []byte("hello")

	// ErrDoubleSpend is returned from PublishTransaction in case the
	// tx being published is spending an output spent by a conflicting
	// transaction.
	ErrDoubleSpend = errors.New("transaction rejected: output already spent")

	// ErrNotMine is an error denoting that a WalletController instance is
	// unable to spend a specified output.
	ErrNotMine = errors.New("the passed output doesn't belong to the wallet")
)

// ErrNoOutputs is returned if we try to create a transaction with no outputs
// or send coins to a set of outputs that is empty.
var ErrNoOutputs = errors.New("no outputs")

// LockID is equivalent to btcsuite/btcwallet/wtxmgr LockID type.
type LockID [32]byte

// Utxo is an unspent output denoted by its outpoint, and output value of the
// original output.
type Utxo struct {
	AddressType   AddressType
	Value         dcrutil.Amount
	Confirmations int64
	PkScript      []byte
	wire.OutPoint

	// TODO(decred) this needs to include ScriptVersion. Then this version needs
	// to be filled and used everywhere instead of DefaultScriptVersion.
}

// TransactionDetail describes a transaction with either inputs which belong to
// the wallet, or has outputs that pay to the wallet.
type TransactionDetail struct {
	// Hash is the transaction hash of the transaction.
	Hash chainhash.Hash

	// Value is the net value of this transaction (in atoms) from the
	// PoV of the wallet. If this transaction purely spends from the
	// wallet's funds, then this value will be negative. Similarly, if this
	// transaction credits the wallet, then this value will be positive.
	Value dcrutil.Amount

	// NumConfirmations is the number of confirmations this transaction
	// has. If the transaction is unconfirmed, then this value will be
	// zero.
	NumConfirmations int32

	// BlockHeight is the hash of the block which includes this
	// transaction. Unconfirmed transactions will have a nil value for this
	// field.
	BlockHash *chainhash.Hash

	// BlockHeight is the height of the block including this transaction.
	// Unconfirmed transaction will show a height of zero.
	BlockHeight int32

	// Timestamp is the unix timestamp of the block including this
	// transaction. If the transaction is unconfirmed, then this will be a
	// timestamp of txn creation.
	Timestamp int64

	// TotalFees is the total fee in atoms paid by this transaction.
	TotalFees int64

	// DestAddresses are the destinations for a transaction
	DestAddresses []stdaddr.Address

	// RawTx returns the raw serialized transaction.
	RawTx []byte

	// Label is an optional transaction label.
	Label string
}

// TransactionSubscription is an interface which describes an object capable of
// receiving notifications of new transaction related to the underlying wallet.
// TODO(roasbeef): add balance updates?
type TransactionSubscription interface {
	// ConfirmedTransactions returns a channel which will be sent on as new
	// relevant transactions are confirmed.
	ConfirmedTransactions() chan *TransactionDetail

	// UnconfirmedTransactions returns a channel which will be sent on as
	// new relevant transactions are seen within the network.
	UnconfirmedTransactions() chan *TransactionDetail

	// Cancel finalizes the subscription, cleaning up any resources
	// allocated.
	Cancel()
}

// WalletController defines an abstract interface for controlling a local Pure
// Go wallet, a local or remote wallet via an RPC mechanism, or possibly even
// a daemon assisted hardware wallet. This interface serves the purpose of
// allowing LightningWallet to be seamlessly compatible with several wallets
// such as: uspv, dcrwallet, etc. This interface then serves as a "base wallet",
// with Lightning Network awareness taking place at a "higher" level of
// abstraction. Essentially, an overlay wallet.  Implementors of this interface
// must closely adhere to the documented behavior of all interface methods in
// order to ensure identical behavior across all concrete implementations.
type WalletController interface {
	// FetchInputInfo queries for the WalletController's knowledge of the
	// passed outpoint. If the base wallet determines this output is under
	// its control, then the original txout should be returned.  Otherwise,
	// a non-nil error value of ErrNotMine should be returned instead.
	FetchInputInfo(prevOut *wire.OutPoint) (*Utxo, error)

	// ConfirmedBalance returns the sum of all the wallet's unspent outputs
	// that have at least confs confirmations. If confs is set to zero,
	// then all unspent outputs, including those currently in the mempool
	// will be included in the final sum.
	//
	// NOTE: Only witness outputs should be included in the computation of
	// the total spendable balance of the wallet. We require this as only
	// witness inputs can be used for funding channels.
	ConfirmedBalance(confs int32) (dcrutil.Amount, error)

	// NewAddress returns the next external or internal address for the
	// wallet dictated by the value of the `change` parameter. If change is
	// true, then an internal address should be used, otherwise an external
	// address should be returned. The type of address returned is dictated
	// by the wallet's capabilities, and may be of type: p2sh, p2pkh,
	// etc.
	NewAddress(addrType AddressType, change bool) (stdaddr.Address, error)

	// LastUnusedAddress returns the last *unused* address known by the
	// wallet. An address is unused if it hasn't received any payments.
	// This can be useful in UIs in order to continually show the
	// "freshest" address without having to worry about "address inflation"
	// caused by continual refreshing. Similar to NewAddress it can derive
	// a specified address type. By default, this is a non-change address.
	LastUnusedAddress(addrType AddressType) (stdaddr.Address, error)

	// IsOurAddress checks if the passed address belongs to this wallet
	IsOurAddress(a stdaddr.Address) bool

	// SendOutputs funds, signs, and broadcasts a Decred transaction paying
	// out to the specified outputs. In the case the wallet has insufficient
	// funds, or the outputs are non-standard, an error should be returned.
	// This method also takes the target fee expressed in atoms/kB that should
	// be used when crafting the transaction.
	//
	// NOTE: This method requires the global coin selection lock to be held.
	SendOutputs(outputs []*wire.TxOut,
		feeRate chainfee.AtomPerKByte, label string) (*wire.MsgTx, error)

	// CreateSimpleTx creates a Bitcoin transaction paying to the specified
	// outputs. The transaction is not broadcasted to the network. In the
	// case the wallet has insufficient funds, or the outputs are
	// non-standard, an error should be returned. This method also takes
	// the target fee expressed in sat/kw that should be used when crafting
	// the transaction.
	//
	// NOTE: The dryRun argument can be set true to create a tx that
	// doesn't alter the database. A tx created with this set to true
	// SHOULD NOT be broadcasted.
	//
	// NOTE: This method requires the global coin selection lock to be held.
	CreateSimpleTx(outputs []*wire.TxOut, feeRate chainfee.AtomPerKByte,
		dryRun bool) (*txauthor.AuthoredTx, error)

	// ListUnspentWitness returns all unspent outputs which are version 0
	// witness programs. The 'minconfirms' and 'maxconfirms' parameters
	// indicate the minimum and maximum number of confirmations an output
	// needs in order to be returned by this method. Passing -1 as
	// 'minconfirms' indicates that even unconfirmed outputs should be
	// returned. Using MaxInt32 as 'maxconfirms' implies returning all
	// outputs with at least 'minconfirms'.
	//
	// NOTE: This method requires the global coin selection lock to be held.
	ListUnspentWitness(minconfirms, maxconfirms int32) ([]*Utxo, error)

	// ListTransactionDetails returns a list of all transactions which are
	// relevant to the wallet over [startHeight;endHeight]. If start height
	// is greater than end height, the transactions will be retrieved in
	// reverse order. To include unconfirmed transactions, endHeight should
	// be set to the special value -1. This will return transactions from
	// the tip of the chain until the start height (inclusive) and
	// unconfirmed transactions.
	ListTransactionDetails(startHeight,
		endHeight int32) ([]*TransactionDetail, error)

	// LockOutpoint marks an outpoint as locked meaning it will no longer
	// be deemed as eligible for coin selection. Locking outputs are
	// utilized in order to avoid race conditions when selecting inputs for
	// usage when funding a channel.
	//
	// NOTE: This method requires the global coin selection lock to be held.
	LockOutpoint(o wire.OutPoint)

	// UnlockOutpoint unlocks a previously locked output, marking it
	// eligible for coin selection.
	//
	// NOTE: This method requires the global coin selection lock to be held.
	UnlockOutpoint(o wire.OutPoint)

	// LeaseOutput locks an output to the given ID, preventing it from being
	// available for any future coin selection attempts. The absolute time
	// of the lock's expiration is returned. The expiration of the lock can
	// be extended by successive invocations of this call. Outputs can be
	// unlocked before their expiration through `ReleaseOutput`.
	//
	// If the output is not known, wtxmgr.ErrUnknownOutput is returned. If
	// the output has already been locked to a different ID, then
	// wtxmgr.ErrOutputAlreadyLocked is returned.
	//
	// NOTE: This method requires the global coin selection lock to be held.
	LeaseOutput(id LockID, op wire.OutPoint) (time.Time, error)

	// ReleaseOutput unlocks an output, allowing it to be available for coin
	// selection if it remains unspent. The ID should match the one used to
	// originally lock the output.
	//
	// NOTE: This method requires the global coin selection lock to be held.
	ReleaseOutput(id LockID, op wire.OutPoint) error

	// PublishTransaction performs cursory validation (dust checks, etc),
	// then finally broadcasts the passed transaction to the Decred network.
	// If the transaction is rejected because it is conflicting with an
	// already known transaction, ErrDoubleSpend is returned. If the
	// transaction is already known (published already), no error will be
	// returned. Other error returned depends on the currently active chain
	// backend. It takes an optional label which will save a label with the
	// published transaction.
	PublishTransaction(tx *wire.MsgTx, label string) error

	// AbandonDoubleSpends removes all unconfirmed transactions that also
	// spend any of the specified outpoints from the wallet. This is used
	// to fix issues when an input used in multiple different sweep
	// transactions gets confirmed in one of them (rendering the other
	// transactions invalid).
	AbandonDoubleSpends(spentOutpoints ...*wire.OutPoint) error

	// LabelTransaction adds a label to a transaction. If the tx already
	// has a label, this call will fail unless the overwrite parameter is
	// set. Labels must not be empty, and they are limited to 500 chars.
	LabelTransaction(hash chainhash.Hash, label string, overwrite bool) error

	// SubscribeTransactions returns a TransactionSubscription client which
	// is capable of receiving async notifications as new transactions
	// related to the wallet are seen within the network, or found in
	// blocks.
	//
	// NOTE: a non-nil error should be returned if notifications aren't
	// supported.
	//
	// TODO(roasbeef): make distinct interface?
	SubscribeTransactions() (TransactionSubscription, error)

	// IsSynced returns a boolean indicating if from the PoV of the wallet,
	// it has fully synced to the current best block in the main chain.
	// It also returns an int64 indicating the timestamp of the best block
	// known to the wallet, expressed in Unix epoch time
	IsSynced() (bool, int64, error)

	// InitialSyncChannel returns a channel that is closed once the wallet
	// has performed its initial sync procedures, is caught up to the
	// network and potentially ready for use.
	InitialSyncChannel() <-chan struct{}

	// BestBlock returns the block height, block hash and timestamp for the
	// tip of the main chain as viewed by the internal wallet controller.
	BestBlock() (int64, chainhash.Hash, int64, error)

	// GetRecoveryInfo returns a boolean indicating whether the wallet is
	// started in recovery mode. It also returns a float64 indicating the
	// recovery progress made so far.
	GetRecoveryInfo() (bool, float64, error)

	// Start initializes the wallet, making any necessary connections,
	// starting up required goroutines etc.
	Start() error

	// Stop signals the wallet for shutdown. Shutdown may entail closing
	// any active sockets, database handles, stopping goroutines, etc.
	Stop() error

	// BackEnd returns a name for the wallet's backing chain service,
	// which could be e.g. dcrd or another consensus service.
	BackEnd() string
}

// BlockChainIO is a dedicated source which will be used to obtain queries
// related to the current state of the blockchain. The data returned by each of
// the defined methods within this interface should always return the most up
// to date data possible.
//
// TODO(roasbeef): move to diff package perhaps?
// TODO(roasbeef): move publish txn here?
type BlockChainIO interface {
	// GetBestBlock returns the current height and block hash of the valid
	// most-work chain the implementation is aware of.
	GetBestBlock() (*chainhash.Hash, int32, error)

	// GetUtxo attempts to return the passed outpoint if it's still a
	// member of the utxo set. The passed height hint should be the "birth
	// height" of the passed outpoint. The script passed should be the
	// script that the outpoint creates. In the case that the output is in
	// the UTXO set, then the output corresponding to that output is
	// returned.  Otherwise, a non-nil error will be returned.
	// As for some backends this call can initiate a rescan, the passed
	// cancel channel can be closed to abort the call.
	GetUtxo(op *wire.OutPoint, pkScript []byte, heightHint uint32,
		cancel <-chan struct{}) (*wire.TxOut, error)

	// GetBlockHash returns the hash of the block in the best blockchain
	// at the given height.
	GetBlockHash(blockHeight int64) (*chainhash.Hash, error)

	// GetBlock returns the block in the main chain identified by the given
	// hash.
	GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error)
}

// Messageinput.Signer represents an abstract object capable of signing arbitrary
// messages. The capabilities of this interface are used to sign announcements
// to the network, or just arbitrary messages that leverage the wallet's keys
// to attest to some message.
type MessageSigner interface {
	// SignMessage attempts to sign a target message with the private key
	// that corresponds to the passed public key. If the target private key
	// is unable to be found, then an error will be returned. The actual
	// digest signed is the double SHA-256 of the passed message.
	SignMessage(pubKey *secp256k1.PublicKey, msg []byte) (input.Signature, error)
}

// WalletDriver represents a "driver" for a particular concrete
// WalletController implementation. A driver is identified by a globally unique
// string identifier along with a 'New()' method which is responsible for
// initializing a particular WalletController concrete implementation.
type WalletDriver struct {
	// WalletType is a string which uniquely identifies the
	// WalletController that this driver, drives.
	WalletType string

	// New creates a new instance of a concrete WalletController
	// implementation given a variadic set up arguments. The function takes
	// a variadic number of interface parameters in order to provide
	// initialization flexibility, thereby accommodating several potential
	// WalletController implementations.
	New func(args ...interface{}) (WalletController, error)

	// BackEnds returns a list of available chain service drivers for the
	// wallet driver.
	BackEnds func() []string
}

var (
	wallets     = make(map[string]*WalletDriver)
	registerMtx sync.Mutex
)

// RegisteredWallets returns a slice of all currently registered notifiers.
//
// NOTE: This function is safe for concurrent access.
func RegisteredWallets() []*WalletDriver {
	registerMtx.Lock()
	defer registerMtx.Unlock()

	registeredWallets := make([]*WalletDriver, 0, len(wallets))
	for _, wallet := range wallets {
		registeredWallets = append(registeredWallets, wallet)
	}

	return registeredWallets
}

// RegisterWallet registers a WalletDriver which is capable of driving a
// concrete WalletController interface. In the case that this driver has
// already been registered, an error is returned.
//
// NOTE: This function is safe for concurrent access.
func RegisterWallet(driver *WalletDriver) error {
	registerMtx.Lock()
	defer registerMtx.Unlock()

	if _, ok := wallets[driver.WalletType]; ok {
		return fmt.Errorf("wallet already registered")
	}

	wallets[driver.WalletType] = driver

	return nil
}

// SupportedWallets returns a slice of strings that represents the wallet
// drivers that have been registered and are therefore supported.
//
// NOTE: This function is safe for concurrent access.
func SupportedWallets() []string {
	registerMtx.Lock()
	defer registerMtx.Unlock()

	supportedWallets := make([]string, 0, len(wallets))
	for walletName := range wallets {
		supportedWallets = append(supportedWallets, walletName)
	}

	return supportedWallets
}
