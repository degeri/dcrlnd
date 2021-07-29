package chainfee

import (
	"github.com/decred/dcrd/dcrutil/v4"
)

const (
	// FeePerKBFloor is the lowest fee rate in atom/kB that we should use
	// for determining transaction fees. Originally, this was used due to
	// the conversion from sats/kB => sats/KW causing a possible rounding
	// error, but in Decred we use this to track the widely deployed
	// minimum relay fee.
	FeePerKBFloor AtomPerKByte = 1e4
)

// AtomPerKByte represents a fee rate in atom/kB.
type AtomPerKByte dcrutil.Amount

// FeeForSize calculates the fee resulting from this fee rate and the given
// size in bytes.
func (s AtomPerKByte) FeeForSize(bytes int64) dcrutil.Amount {
	return dcrutil.Amount(s) * dcrutil.Amount(bytes) / 1000
}

// String returns a pretty string representation for the rate in DCR/kB.
func (s AtomPerKByte) String() string {
	return dcrutil.Amount(s).String() + "/kB"
}
