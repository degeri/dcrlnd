package lntest

import (
	"context"
	"encoding/hex"
	"errors"
	"math"
	"runtime"
	"time"

	"github.com/decred/dcrd/blockchain/standalone"
	"github.com/decred/dcrd/chaincfg/chainhash"
	rpcclient3 "github.com/decred/dcrd/rpcclient/v6"
	"github.com/decred/dcrd/wire"
)

// solveBlock attempts to find a nonce which makes the passed block header hash
// to a value less than the target difficulty.  When a successful solution is
// found, true is returned and the nonce field of the passed header is updated
// with the solution.  False is returned if no solution exists.
func solveBlock(header *wire.BlockHeader) bool {
	// sbResult is used by the solver goroutines to send results.
	type sbResult struct {
		found bool
		nonce uint32
	}

	// solver accepts a block header and a nonce range to test. It is
	// intended to be run as a goroutine.
	targetDifficulty := standalone.CompactToBig(header.Bits)
	quit := make(chan bool)
	results := make(chan sbResult)
	solver := func(hdr wire.BlockHeader, startNonce, stopNonce uint32) {
		// We need to modify the nonce field of the header, so make sure
		// we work with a copy of the original header.
		for i := startNonce; i >= startNonce && i <= stopNonce; i++ {
			select {
			case <-quit:
				results <- sbResult{false, 0}
				return
			default:
				hdr.Nonce = i
				hash := hdr.BlockHash()
				if standalone.HashToBig(&hash).Cmp(
					targetDifficulty) <= 0 {

					results <- sbResult{true, i}
					return
				}
			}
		}
		results <- sbResult{false, 0}
	}

	startNonce := uint32(1)
	stopNonce := uint32(math.MaxUint32)
	numCores := uint32(runtime.NumCPU())
	noncesPerCore := (stopNonce - startNonce) / numCores
	for i := uint32(0); i < numCores; i++ {
		rangeStart := startNonce + (noncesPerCore * i)
		rangeStop := startNonce + (noncesPerCore * (i + 1)) - 1
		if i == numCores-1 {
			rangeStop = stopNonce
		}
		go solver(*header, rangeStart, rangeStop)
	}
	var foundResult bool
	for i := uint32(0); i < numCores; i++ {
		result := <-results
		if !foundResult && result.found {
			close(quit)
			header.Nonce = result.nonce
			foundResult = true
		}
	}

	return foundResult
}

// AdjustedSimnetMiner is an alternative miner function that instead of relying
// on the backing node to mine a block, fetches the work required for the next
// block and mines the block itself while adjusting the timestamp so that (on
// simnet) no difficulty increase is trigered. After finding a block, it
// automatically publishes it to the underlying node.
//
// This is only applicable for tests that run on simnet or other networks that
// have a target block per count of 1 second.
func AdjustedSimnetMiner(client *rpcclient3.Client, nb uint32) ([]*chainhash.Hash, error) {

	hashes := make([]*chainhash.Hash, nb)

	for i := uint32(0); i < nb; i++ {
		work, err := client.GetWork(context.TODO())
		if err != nil {
			return nil, err
		}

		workBytes, err := hex.DecodeString(work.Data)
		if err != nil {
			return nil, err
		}

		var header wire.BlockHeader
		err = header.FromBytes(workBytes)
		if err != nil {
			return nil, err
		}

		prevBlock, err := client.GetBlock(context.TODO(), &header.PrevBlock)
		if err != nil {
			return nil, err
		}

		header.Timestamp = prevBlock.Header.Timestamp.Add(time.Second)
		header.Bits = prevBlock.Header.Bits
		solved := solveBlock(&header)
		if !solved {
			return nil, errors.New("unable to solve block")
		}

		var extraBytes [12]byte
		workBytes, err = header.Bytes()
		if err != nil {
			return nil, err
		}
		workBytes = append(workBytes, extraBytes[:]...)
		workData := hex.EncodeToString(workBytes)
		accepted, err := client.GetWorkSubmit(context.TODO(), workData)
		if err != nil {
			return nil, err
		}

		if !accepted {
			return nil, errors.New("solved block was not accepted")
		}

		bh := header.BlockHash()
		hashes[i] = &bh
	}

	return hashes, nil
}
