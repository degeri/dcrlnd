package contractcourt

import (
	"testing"
	"time"

	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrlnd/chainntnfs"
	"github.com/decred/dcrlnd/channeldb"
	"github.com/decred/dcrlnd/channeldb/kvdb"
	"github.com/decred/dcrlnd/input"
	"github.com/decred/dcrlnd/lnwallet"
	"github.com/decred/dcrlnd/lnwallet/chainfee"
	"github.com/decred/dcrlnd/sweep"
)

type commitSweepResolverTestContext struct {
	resolver           *commitSweepResolver
	notifier           *mockNotifier
	sweeper            *mockSweeper
	resolverResultChan chan resolveResult
	t                  *testing.T
}

func newCommitSweepResolverTestContext(t *testing.T,
	resolution *lnwallet.CommitOutputResolution) *commitSweepResolverTestContext {

	notifier := &mockNotifier{
		epochChan: make(chan *chainntnfs.BlockEpoch),
		spendChan: make(chan *chainntnfs.SpendDetail),
		confChan:  make(chan *chainntnfs.TxConfirmation),
	}

	sweeper := newMockSweeper()

	checkPointChan := make(chan struct{}, 1)

	chainCfg := ChannelArbitratorConfig{
		ChainArbitratorConfig: ChainArbitratorConfig{
			Notifier: notifier,
			Sweeper:  sweeper,
		},
		PutResolverReport: func(_ kvdb.RwTx,
			_ *channeldb.ResolverReport) error {

			return nil
		},
	}

	cfg := ResolverConfig{
		ChannelArbitratorConfig: chainCfg,
		Checkpoint: func(_ ContractResolver,
			_ ...*channeldb.ResolverReport) error {

			checkPointChan <- struct{}{}
			return nil
		},
	}

	resolver := newCommitSweepResolver(
		*resolution, 0, wire.OutPoint{}, cfg,
	)

	return &commitSweepResolverTestContext{
		resolver: resolver,
		notifier: notifier,
		sweeper:  sweeper,
		t:        t,
	}
}

func (i *commitSweepResolverTestContext) resolve() {
	// Start resolver.
	i.resolverResultChan = make(chan resolveResult, 1)
	go func() {
		nextResolver, err := i.resolver.Resolve()
		i.resolverResultChan <- resolveResult{
			nextResolver: nextResolver,
			err:          err,
		}
	}()
}

func (i *commitSweepResolverTestContext) notifyEpoch(height int32) {
	i.notifier.epochChan <- &chainntnfs.BlockEpoch{
		Height: height,
	}
}

func (i *commitSweepResolverTestContext) waitForResult() {
	i.t.Helper()

	result := <-i.resolverResultChan
	if result.err != nil {
		i.t.Fatal(result.err)
	}

	if result.nextResolver != nil {
		i.t.Fatal("expected no next resolver")
	}
}

type mockSweeper struct {
	sweptInputs   chan input.Input
	updatedInputs chan wire.OutPoint
	sweepTx       *wire.MsgTx
	sweepErr      error
}

func newMockSweeper() *mockSweeper {
	return &mockSweeper{
		sweptInputs:   make(chan input.Input),
		updatedInputs: make(chan wire.OutPoint),
		sweepTx:       &wire.MsgTx{},
	}
}

func (s *mockSweeper) SweepInput(input input.Input, params sweep.Params) (
	chan sweep.Result, error) {

	s.sweptInputs <- input

	result := make(chan sweep.Result, 1)
	result <- sweep.Result{
		Tx:  s.sweepTx,
		Err: s.sweepErr,
	}
	return result, nil
}

func (s *mockSweeper) CreateSweepTx(inputs []input.Input, feePref sweep.FeePreference,
	currentBlockHeight uint32) (*wire.MsgTx, error) {

	return nil, nil
}

func (s *mockSweeper) RelayFeePerKB() chainfee.AtomPerKByte {
	return 1e4
}

func (s *mockSweeper) UpdateParams(input wire.OutPoint,
	params sweep.ParamsUpdate) (chan sweep.Result, error) {

	s.updatedInputs <- input

	result := make(chan sweep.Result, 1)
	result <- sweep.Result{
		Tx: s.sweepTx,
	}
	return result, nil
}

var _ UtxoSweeper = &mockSweeper{}

// TestCommitSweepResolverNoDelay tests resolution of a direct commitment output
// unencumbered by a time lock.
func TestCommitSweepResolverNoDelay(t *testing.T) {
	t.Parallel()
	defer timeout(t)()

	res := lnwallet.CommitOutputResolution{
		SelfOutputSignDesc: input.SignDescriptor{
			Output: &wire.TxOut{
				Value: 100,
			},
			WitnessScript: []byte{0},
		},
	}

	ctx := newCommitSweepResolverTestContext(t, &res)

	// Replace our checkpoint with one which will push reports into a
	// channel for us to consume. We replace this function on the resolver
	// itself because it is created by the test context.
	reportChan := make(chan *channeldb.ResolverReport)
	ctx.resolver.Checkpoint = func(_ ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		// Send all of our reports into the channel.
		for _, report := range reports {
			reportChan <- report
		}

		return nil
	}

	ctx.resolve()

	spendTx := &wire.MsgTx{}
	spendHash := spendTx.TxHash()
	ctx.notifier.confChan <- &chainntnfs.TxConfirmation{
		Tx: spendTx,
	}

	// No csv delay, so the input should be swept immediately.
	<-ctx.sweeper.sweptInputs

	amt := dcrutil.Amount(res.SelfOutputSignDesc.Output.Value)
	expectedReport := &channeldb.ResolverReport{
		OutPoint:        wire.OutPoint{},
		Amount:          amt,
		ResolverType:    channeldb.ResolverTypeCommit,
		ResolverOutcome: channeldb.ResolverOutcomeClaimed,
		SpendTxID:       &spendHash,
	}

	assertResolverReport(t, reportChan, expectedReport)

	ctx.waitForResult()
}

// testCommitSweepResolverDelay tests resolution of a direct commitment output
// that is encumbered by a time lock. sweepErr indicates whether the local node
// fails to sweep the output.
func testCommitSweepResolverDelay(t *testing.T, sweepErr error) {
	defer timeout(t)()

	const sweepProcessInterval = 100 * time.Millisecond
	amt := int64(100)
	outpoint := wire.OutPoint{
		Index: 5,
	}
	res := lnwallet.CommitOutputResolution{
		SelfOutputSignDesc: input.SignDescriptor{
			Output: &wire.TxOut{
				Value: amt,
			},
			WitnessScript: []byte{0},
		},
		MaturityDelay: 3,
		SelfOutPoint:  outpoint,
	}

	ctx := newCommitSweepResolverTestContext(t, &res)

	// Replace our checkpoint with one which will push reports into a
	// channel for us to consume. We replace this function on the resolver
	// itself because it is created by the test context.
	reportChan := make(chan *channeldb.ResolverReport)
	ctx.resolver.Checkpoint = func(_ ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		// Send all of our reports into the channel.
		for _, report := range reports {
			reportChan <- report
		}

		return nil
	}

	// Setup whether we expect the sweeper to receive a sweep error in this
	// test case.
	ctx.sweeper.sweepErr = sweepErr

	report := ctx.resolver.report()
	expectedReport := ContractReport{
		Outpoint:     outpoint,
		Type:         ReportOutputUnencumbered,
		Amount:       dcrutil.Amount(amt),
		LimboBalance: dcrutil.Amount(amt),
	}
	if *report != expectedReport {
		t.Fatalf("unexpected resolver report. want=%v got=%v",
			expectedReport, report)
	}

	ctx.resolve()

	ctx.notifier.confChan <- &chainntnfs.TxConfirmation{
		BlockHeight: testInitialBlockHeight - 1,
	}

	// Allow resolver to process confirmation.
	time.Sleep(sweepProcessInterval)

	// Expect report to be updated.
	report = ctx.resolver.report()
	if report.MaturityHeight != testInitialBlockHeight+2 {
		t.Fatal("report maturity height incorrect")
	}

	// Notify initial block height. The csv lock is still in effect, so we
	// don't expect any sweep to happen yet.
	ctx.notifyEpoch(testInitialBlockHeight)

	select {
	case <-ctx.sweeper.sweptInputs:
		t.Fatal("no sweep expected")
	case <-time.After(sweepProcessInterval):
	}

	// A new block arrives. The commit tx confirmed at height -1 and the csv
	// is 3, so a spend will be valid in the first block after height +1.
	ctx.notifyEpoch(testInitialBlockHeight + 1)

	<-ctx.sweeper.sweptInputs

	// Set the resolution report outcome based on whether our sweep
	// succeeded.
	outcome := channeldb.ResolverOutcomeClaimed
	if sweepErr != nil {
		outcome = channeldb.ResolverOutcomeUnclaimed
	}
	sweepTx := ctx.sweeper.sweepTx.TxHash()

	assertResolverReport(t, reportChan, &channeldb.ResolverReport{
		OutPoint:        outpoint,
		ResolverType:    channeldb.ResolverTypeCommit,
		ResolverOutcome: outcome,
		Amount:          dcrutil.Amount(amt),
		SpendTxID:       &sweepTx,
	})

	ctx.waitForResult()

	// If this test case generates a sweep error, we don't expect to be
	// able to recover anything. This might happen if the local commitment
	// output was swept by a justice transaction by the remote party.
	expectedRecoveredBalance := dcrutil.Amount(amt)
	if sweepErr != nil {
		expectedRecoveredBalance = 0
	}

	report = ctx.resolver.report()
	expectedReport = ContractReport{
		Outpoint:         outpoint,
		Type:             ReportOutputUnencumbered,
		Amount:           dcrutil.Amount(amt),
		MaturityHeight:   testInitialBlockHeight + 2,
		RecoveredBalance: expectedRecoveredBalance,
	}
	if *report != expectedReport {
		t.Fatalf("unexpected resolver report. want=%v got=%v",
			expectedReport, report)
	}

}

// TestCommitSweepResolverDelay tests resolution of a direct commitment output
// that is encumbered by a time lock.
func TestCommitSweepResolverDelay(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		sweepErr error
	}{{
		name:     "success",
		sweepErr: nil,
	}, {
		name:     "remote spend",
		sweepErr: sweep.ErrRemoteSpend,
	}}

	for _, tc := range testCases {
		tc := tc
		ok := t.Run(tc.name, func(t *testing.T) {
			testCommitSweepResolverDelay(t, tc.sweepErr)
		})
		if !ok {
			break
		}
	}
}
