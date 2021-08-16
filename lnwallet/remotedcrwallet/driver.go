package remotedcrwallet

import (
	"fmt"

	"github.com/decred/dcrlnd/lnwallet"
)

const (
	walletType = "remotedcrwallet"
)

var (
	// We currently only support full chain sync (no spv), so hardcode
	// the available backends.
	availableBackends = []string{"dcrd", "spv"}
)

// createNewWallet creates a new instance of DcrWallet given the proper list of
// initialization parameters. This function is the factory function required to
// properly create an instance of the lnwallet.WalletDriver struct for
// DcrWallet.
func createNewWallet(args ...interface{}) (lnwallet.WalletController, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("incorrect number of arguments to .New(...), "+
			"expected 1, instead passed %v", len(args))
	}

	config, ok := args[0].(*Config)
	if !ok {
		return nil, fmt.Errorf("first argument to remotedcrwallet.New " +
			"is incorrect, expected a *remotedcrwallet.Config")
	}

	return New(*config)
}

// init registers a driver for the DcrWallet concrete implementation of the
// lnwallet.WalletController interface.
func init() {
	// Register the driver.
	driver := &lnwallet.WalletDriver{
		WalletType: walletType,
		New:        createNewWallet,
		BackEnds:   func() []string { return availableBackends },
	}

	if err := lnwallet.RegisterWallet(driver); err != nil {
		panic(fmt.Sprintf("failed to register wallet driver '%s': %v",
			walletType, err))
	}
}
