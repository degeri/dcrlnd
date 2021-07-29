package input

import (
	"fmt"

	"github.com/decred/dcrd/txscript/v4/stdaddr"
)

// PayToAddrScript is an adapted version of txscript/v3 PayToAddrScript(). This
// is adapted to ease migration efforts.
func PayToAddrScript(addr stdaddr.Address) ([]byte, error) {
	version, script := addr.PaymentScript()
	if version != 0 {
		return nil, fmt.Errorf("incompatible script verion %d", version)
	}

	return script, nil
}
