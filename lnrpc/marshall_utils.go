package lnrpc

import (
	"encoding/hex"
	"errors"
	fmt "fmt"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrlnd/lnwallet"
	"github.com/decred/dcrlnd/lnwire"
)

var (
	// ErrAtomsMAtomsMutualExclusive is returned when both an atom and an
	// matom amount are set.
	ErrAtomMAtomMutualExclusive = errors.New(
		"atom and matom arguments are mutually exclusive",
	)
)

// CalculateFeeLimit returns the fee limit in millisatoshis. If a percentage
// based fee limit has been requested, we'll factor in the ratio provided with
// the amount of the payment.
func CalculateFeeLimit(feeLimit *FeeLimit,
	amount lnwire.MilliAtom) lnwire.MilliAtom {

	switch feeLimit.GetLimit().(type) {

	case *FeeLimit_Fixed:
		return lnwire.NewMAtomsFromAtoms(
			dcrutil.Amount(feeLimit.GetFixed()),
		)

	case *FeeLimit_FixedMAtoms:
		return lnwire.MilliAtom(feeLimit.GetFixedMAtoms())

	case *FeeLimit_Percent:
		return amount * lnwire.MilliAtom(feeLimit.GetPercent()) / 100

	default:
		// If a fee limit was not specified, we'll use the payment's
		// amount as an upper bound in order to avoid payment attempts
		// from incurring fees higher than the payment amount itself.
		return amount
	}
}

// UnmarshallAmt returns a strong msat type for a atom/matom pair of rpc
// fields.
func UnmarshallAmt(amtAtom, amtMAtom int64) (lnwire.MilliAtom, error) {
	if amtAtom != 0 && amtMAtom != 0 {
		return 0, ErrAtomMAtomMutualExclusive
	}

	if amtAtom != 0 {
		return lnwire.NewMAtomsFromAtoms(dcrutil.Amount(amtAtom)), nil
	}

	return lnwire.MilliAtom(amtMAtom), nil
}

// ParseConfs validates the minimum and maximum confirmation arguments of a
// ListUnspent request.
func ParseConfs(min, max int32) (int32, int32, error) {
	switch {
	// Ensure that the user didn't attempt to specify a negative number of
	// confirmations, as that isn't possible.
	case min < 0:
		return 0, 0, fmt.Errorf("min confirmations must be >= 0")

	// We'll also ensure that the min number of confs is strictly less than
	// or equal to the max number of confs for sanity.
	case min > max:
		return 0, 0, fmt.Errorf("max confirmations must be >= min " +
			"confirmations")

	default:
		return min, max, nil
	}
}

// MarshalUtxos translates a []*lnwallet.Utxo into a []*lnrpc.Utxo.
func MarshalUtxos(utxos []*lnwallet.Utxo, activeNetParams *chaincfg.Params) (
	[]*Utxo, error) {

	// TODO(decred): this needs to come from the utxo itself.
	const scriptVersion uint16 = 0

	res := make([]*Utxo, 0, len(utxos))
	for _, utxo := range utxos {
		// Translate lnwallet address type to the proper gRPC proto
		// address type.
		var addrType AddressType
		switch utxo.AddressType {

		case lnwallet.WitnessPubKey:
			addrType = AddressType_WITNESS_PUBKEY_HASH

		case lnwallet.NestedWitnessPubKey:
			addrType = AddressType_NESTED_PUBKEY_HASH

		case lnwallet.PubKeyHash:
			addrType = AddressType_PUBKEY_HASH

		case lnwallet.ScriptHash:
			addrType = AddressType_SCRIPT_HASH

		case lnwallet.UnknownAddressType:
			continue

		default:
			return nil, fmt.Errorf("invalid utxo address type")
		}

		// Now that we know we have a proper mapping to an address,
		// we'll convert the regular outpoint to an lnrpc variant.
		outpoint := &OutPoint{
			TxidBytes:   utxo.OutPoint.Hash[:],
			TxidStr:     utxo.OutPoint.Hash.String(),
			OutputIndex: utxo.OutPoint.Index,
		}

		utxoResp := Utxo{
			AddressType:   addrType,
			AmountAtoms:   int64(utxo.Value),
			PkScript:      hex.EncodeToString(utxo.PkScript),
			Outpoint:      outpoint,
			Confirmations: utxo.Confirmations,
		}

		// Finally, we'll attempt to extract the raw address from the
		// script so we can display a human friendly address to the end
		// user.
		_, outAddresses, _, err := txscript.ExtractPkScriptAddrs(
			scriptVersion, utxo.PkScript, activeNetParams, false,
		)
		if err != nil {
			return nil, err
		}

		// If we can't properly locate a single address, then this was
		// an error in our mapping, and we'll return an error back to
		// the user.
		if len(outAddresses) != 1 {
			return nil, fmt.Errorf("an output was unexpectedly " +
				"multisig")
		}
		utxoResp.Address = outAddresses[0].String()

		res = append(res, &utxoResp)
	}

	return res, nil
}
