package peer

import (
	"bytes"
	"testing"
	"time"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrlnd/chainntnfs"
	"github.com/decred/dcrlnd/channeldb"
	"github.com/decred/dcrlnd/htlcswitch"
	"github.com/decred/dcrlnd/input"
	"github.com/decred/dcrlnd/lnwallet/chancloser"
	"github.com/decred/dcrlnd/lnwire"
)

var (
	// p2SHAddress is a valid pay to script hash address.
	p2SHAddress = "TcrypGAcGCRVXrES7hWqVZb5oLJKCZEtoL1"

	// p2pkhAddress is a valid pay to public key hash address.
	p2PKHAddress = "TsYJk2CWpEjcwgHMcJjva3sDCKZgCMFQvj2"

	// timeout is a timeout value to use for tests which need ot wait for
	// a return value on a channel.
	timeout = time.Second * 5
)

// TestPeerChannelClosureAcceptFeeResponder tests the shutdown responder's
// behavior if we can agree on the fee immediately.
func TestPeerChannelClosureAcceptFeeResponder(t *testing.T) {
	t.Parallel()

	notifier := &mockNotifier{
		confChannel: make(chan *chainntnfs.TxConfirmation),
	}
	broadcastTxChan := make(chan *wire.MsgTx)

	alicePeer, bobChan, cleanUp, err := createTestPeer(
		notifier, broadcastTxChan, noUpdate,
	)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	chanID := lnwire.NewChanIDFromOutPoint(bobChan.ChannelPoint())

	// We send a shutdown request to Alice. She will now be the responding
	// node in this shutdown procedure. We first expect Alice to answer
	// this shutdown request with a Shutdown message.
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: lnwire.NewShutdown(chanID, dummyDeliveryScript),
	}

	var msg lnwire.Message
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive shutdown message")
	}

	shutdownMsg, ok := msg.(*lnwire.Shutdown)
	if !ok {
		t.Fatalf("expected Shutdown message, got %T", msg)
	}

	respDeliveryScript := shutdownMsg.Address

	// Alice will then send a ClosingSigned message, indicating her proposed
	// closing transaction fee. Alice sends the ClosingSigned message as she is
	// the initiator of the channel.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive ClosingSigned message")
	}

	respClosingSigned, ok := msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	// We accept the fee, and send a ClosingSigned with the same fee back,
	// so she knows we agreed.
	aliceFee := respClosingSigned.FeeAtoms
	bobSig, _, _, err := bobChan.CreateCloseProposal(
		aliceFee, dummyDeliveryScript, respDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err := lnwire.NewSigFromSignature(bobSig)
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned := lnwire.NewClosingSigned(chanID, aliceFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Alice should now see that we agreed on the fee, and should broadcast the
	// closing transaction.
	select {
	case <-broadcastTxChan:
	case <-time.After(timeout):
		t.Fatalf("closing tx not broadcast")
	}

	// Need to pull the remaining message off of Alice's outgoing queue.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive ClosingSigned message")
	}
	if _, ok := msg.(*lnwire.ClosingSigned); !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	// Alice should be waiting in a goroutine for a confirmation.
	notifier.confChannel <- &chainntnfs.TxConfirmation{}
}

// TestPeerChannelClosureAcceptFeeInitiator tests the shutdown initiator's
// behavior if we can agree on the fee immediately.
func TestPeerChannelClosureAcceptFeeInitiator(t *testing.T) {
	t.Parallel()

	notifier := &mockNotifier{
		confChannel: make(chan *chainntnfs.TxConfirmation),
	}
	broadcastTxChan := make(chan *wire.MsgTx)

	alicePeer, bobChan, cleanUp, err := createTestPeer(
		notifier, broadcastTxChan, noUpdate,
	)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// We make Alice send a shutdown request.
	updateChan := make(chan interface{}, 1)
	errChan := make(chan error, 1)
	closeCommand := &htlcswitch.ChanClose{
		CloseType:      htlcswitch.CloseRegular,
		ChanPoint:      bobChan.ChannelPoint(),
		Updates:        updateChan,
		TargetFeePerKB: 12500,
		Err:            errChan,
	}
	alicePeer.localCloseChanReqs <- closeCommand

	// We can now pull a Shutdown message off of Alice's outgoingQueue.
	var msg lnwire.Message
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive shutdown request")
	}

	shutdownMsg, ok := msg.(*lnwire.Shutdown)
	if !ok {
		t.Fatalf("expected Shutdown message, got %T", msg)
	}

	aliceDeliveryScript := shutdownMsg.Address

	// Bob will respond with his own Shutdown message.
	chanID := shutdownMsg.ChannelID
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: lnwire.NewShutdown(chanID,
			dummyDeliveryScript),
	}

	// Alice will reply with a ClosingSigned here.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed message")
	}
	closingSignedMsg, ok := msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected to receive closing signed message, got %T", msg)
	}

	// Bob should reply with the exact same fee in his next ClosingSigned
	// message.
	bobFee := closingSignedMsg.FeeAtoms
	bobSig, _, _, err := bobChan.CreateCloseProposal(
		bobFee, dummyDeliveryScript, aliceDeliveryScript,
	)
	if err != nil {
		t.Fatalf("unable to create close proposal: %v", err)
	}
	parsedSig, err := lnwire.NewSigFromSignature(bobSig)
	if err != nil {
		t.Fatalf("unable to parse signature: %v", err)
	}

	closingSigned := lnwire.NewClosingSigned(shutdownMsg.ChannelID,
		bobFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Alice should accept Bob's fee, broadcast the cooperative close tx, and
	// send a ClosingSigned message back to Bob.

	// Alice should now broadcast the closing transaction.
	select {
	case <-broadcastTxChan:
	case <-time.After(timeout):
		t.Fatalf("closing tx not broadcast")
	}

	// Alice should respond with the ClosingSigned they both agreed upon.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed message")
	}

	closingSignedMsg, ok = msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	if closingSignedMsg.FeeAtoms != bobFee {
		t.Fatalf("expected ClosingSigned fee to be %v, instead got %v",
			bobFee, closingSignedMsg.FeeAtoms)
	}

	// Alice should be waiting on a single confirmation for the coop close tx.
	notifier.confChannel <- &chainntnfs.TxConfirmation{}
}

// TestPeerChannelClosureFeeNegotiationsResponder tests the shutdown
// responder's behavior in the case where we must do several rounds of fee
// negotiation before we agree on a fee.
func TestPeerChannelClosureFeeNegotiationsResponder(t *testing.T) {
	t.Parallel()

	notifier := &mockNotifier{
		confChannel: make(chan *chainntnfs.TxConfirmation),
	}
	broadcastTxChan := make(chan *wire.MsgTx)

	alicePeer, bobChan, cleanUp, err := createTestPeer(
		notifier, broadcastTxChan, noUpdate,
	)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	chanID := lnwire.NewChanIDFromOutPoint(bobChan.ChannelPoint())

	// Bob sends a shutdown request to Alice. She will now be the responding
	// node in this shutdown procedure. We first expect Alice to answer this
	// Shutdown request with a Shutdown message.
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: lnwire.NewShutdown(chanID,
			dummyDeliveryScript),
	}

	var msg lnwire.Message
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive shutdown message")
	}

	shutdownMsg, ok := msg.(*lnwire.Shutdown)
	if !ok {
		t.Fatalf("expected Shutdown message, got %T", msg)
	}

	aliceDeliveryScript := shutdownMsg.Address

	// As Alice is the channel initiator, she will send her ClosingSigned
	// message.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed message")
	}

	aliceClosingSigned, ok := msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	// Bob doesn't agree with the fee and will send one back that's 2.5x.
	preferredRespFee := aliceClosingSigned.FeeAtoms
	increasedFee := dcrutil.Amount(float64(preferredRespFee) * 2.5)
	bobSig, _, _, err := bobChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, aliceDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err := lnwire.NewSigFromSignature(bobSig)
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned := lnwire.NewClosingSigned(chanID, increasedFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Alice will now see the new fee we propose, but with current settings it
	// won't accept it immediately as it differs too much by its ideal fee. We
	// should get a new proposal back, which should have the average fee rate
	// proposed.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed message")
	}

	aliceClosingSigned, ok = msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	// The fee sent by Alice should be less than the fee Bob just sent as
	// Alice should attempt to compromise.
	aliceFee := aliceClosingSigned.FeeAtoms
	if aliceFee > increasedFee {
		t.Fatalf("new fee should be less than our fee: new=%v, "+
			"prior=%v", aliceFee, increasedFee)
	}
	lastFeeResponder := aliceFee

	// We try negotiating a 2.1x fee, which should also be rejected.
	increasedFee = dcrutil.Amount(float64(preferredRespFee) * 2.1)
	bobSig, _, _, err = bobChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, aliceDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err = lnwire.NewSigFromSignature(bobSig)
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned = lnwire.NewClosingSigned(chanID, increasedFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Bob's latest proposal still won't be accepted and Alice should send over
	// a new ClosingSigned message. It should be the average of what Bob and
	// Alice each proposed last time.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed message")
	}

	aliceClosingSigned, ok = msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	// Alice should inch towards Bob's fee, in order to compromise.
	// Additionally, this fee should be less than the fee Bob sent before.
	aliceFee = aliceClosingSigned.FeeAtoms
	if aliceFee < lastFeeResponder {
		t.Fatalf("new fee should be greater than prior: new=%v, "+
			"prior=%v", aliceFee, lastFeeResponder)
	}
	if aliceFee > increasedFee {
		t.Fatalf("new fee should be less than Bob's fee: new=%v, "+
			"prior=%v", aliceFee, increasedFee)
	}

	// Finally, Bob will accept the fee by echoing back the same fee that Alice
	// just sent over.
	bobSig, _, _, err = bobChan.CreateCloseProposal(
		aliceFee, dummyDeliveryScript, aliceDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err = lnwire.NewSigFromSignature(bobSig)
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned = lnwire.NewClosingSigned(chanID, aliceFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Alice will now see that Bob agreed on the fee, and broadcast the coop
	// close transaction.
	select {
	case <-broadcastTxChan:
	case <-time.After(timeout):
		t.Fatalf("closing tx not broadcast")
	}

	// Alice should respond with the ClosingSigned they both agreed upon.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed message")
	}
	if _, ok := msg.(*lnwire.ClosingSigned); !ok {
		t.Fatalf("expected to receive closing signed message, got %T", msg)
	}

	// Alice should be waiting on a single confirmation for the coop close tx.
	notifier.confChannel <- &chainntnfs.TxConfirmation{}
}

// TestPeerChannelClosureFeeNegotiationsInitiator tests the shutdown
// initiator's behavior in the case where we must do several rounds of fee
// negotiation before we agree on a fee.
func TestPeerChannelClosureFeeNegotiationsInitiator(t *testing.T) {
	t.Parallel()

	notifier := &mockNotifier{
		confChannel: make(chan *chainntnfs.TxConfirmation),
	}
	broadcastTxChan := make(chan *wire.MsgTx)

	alicePeer, bobChan, cleanUp, err := createTestPeer(
		notifier, broadcastTxChan, noUpdate,
	)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// We make the initiator send a shutdown request.
	updateChan := make(chan interface{}, 1)
	errChan := make(chan error, 1)
	closeCommand := &htlcswitch.ChanClose{
		CloseType:      htlcswitch.CloseRegular,
		ChanPoint:      bobChan.ChannelPoint(),
		Updates:        updateChan,
		TargetFeePerKB: 12500,
		Err:            errChan,
	}

	alicePeer.localCloseChanReqs <- closeCommand

	// Alice should now send a Shutdown request to Bob.
	var msg lnwire.Message
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive shutdown request")
	}

	shutdownMsg, ok := msg.(*lnwire.Shutdown)
	if !ok {
		t.Fatalf("expected Shutdown message, got %T", msg)
	}

	aliceDeliveryScript := shutdownMsg.Address

	// Bob will answer the Shutdown message with his own Shutdown.
	chanID := lnwire.NewChanIDFromOutPoint(bobChan.ChannelPoint())
	respShutdown := lnwire.NewShutdown(chanID, dummyDeliveryScript)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: respShutdown,
	}

	// Alice should now respond with a ClosingSigned message with her ideal
	// fee rate.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed")
	}
	closingSignedMsg, ok := msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	idealFeeRate := closingSignedMsg.FeeAtoms
	lastReceivedFee := idealFeeRate

	increasedFee := dcrutil.Amount(float64(idealFeeRate) * 2.1)
	lastSentFee := increasedFee

	bobSig, _, _, err := bobChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, aliceDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err := lnwire.NewSigFromSignature(bobSig)
	if err != nil {
		t.Fatalf("unable to parse signature: %v", err)
	}

	closingSigned := lnwire.NewClosingSigned(chanID, increasedFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// It still won't be accepted, and we should get a new proposal, the
	// average of what we proposed, and what they proposed last time.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed")
	}
	closingSignedMsg, ok = msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	aliceFee := closingSignedMsg.FeeAtoms
	if aliceFee < lastReceivedFee {
		t.Fatalf("new fee should be greater than prior: new=%v, old=%v",
			aliceFee, lastReceivedFee)
	}
	if aliceFee > lastSentFee {
		t.Fatalf("new fee should be less than our fee: new=%v, old=%v",
			aliceFee, lastSentFee)
	}

	lastReceivedFee = aliceFee

	// We'll try negotiating a 1.5x fee, which should also be rejected.
	increasedFee = dcrutil.Amount(float64(idealFeeRate) * 1.5)
	lastSentFee = increasedFee

	bobSig, _, _, err = bobChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, aliceDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err = lnwire.NewSigFromSignature(bobSig)
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}

	closingSigned = lnwire.NewClosingSigned(chanID, increasedFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Alice won't accept Bob's new proposal, and Bob should receive a new
	// proposal which is the average of what Bob proposed and Alice proposed
	// last time.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed")
	}
	closingSignedMsg, ok = msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	aliceFee = closingSignedMsg.FeeAtoms
	if aliceFee < lastReceivedFee {
		t.Fatalf("new fee should be greater than prior: new=%v, old=%v",
			aliceFee, lastReceivedFee)
	}
	if aliceFee > lastSentFee {
		t.Fatalf("new fee should be less than Bob's fee: new=%v, old=%v",
			aliceFee, lastSentFee)
	}

	// Bob will now accept their fee by sending back a ClosingSigned message
	// with an identical fee.
	bobSig, _, _, err = bobChan.CreateCloseProposal(
		aliceFee, dummyDeliveryScript, aliceDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err = lnwire.NewSigFromSignature(bobSig)
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned = lnwire.NewClosingSigned(chanID, aliceFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Wait for closing tx to be broadcasted.
	select {
	case <-broadcastTxChan:
	case <-time.After(timeout):
		t.Fatalf("closing tx not broadcast")
	}

	// Alice should respond with the ClosingSigned they both agreed upon.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed message")
	}
	if _, ok := msg.(*lnwire.ClosingSigned); !ok {
		t.Fatalf("expected to receive closing signed message, got %T", msg)
	}

	// Alice should be waiting on a single confirmation for the coop close tx.
	notifier.confChannel <- &chainntnfs.TxConfirmation{}
}

// TestChooseDeliveryScript tests that chooseDeliveryScript correctly errors
// when upfront and user set scripts that do not match are provided, allows
// matching values and returns appropriate values in the case where one or none
// are set.
func TestChooseDeliveryScript(t *testing.T) {
	// generate non-zero scripts for testing.
	script1 := genScript(t, p2SHAddress)
	script2 := genScript(t, p2PKHAddress)

	tests := []struct {
		name           string
		userScript     lnwire.DeliveryAddress
		shutdownScript lnwire.DeliveryAddress
		expectedScript lnwire.DeliveryAddress
		expectedError  error
	}{
		{
			name:           "Neither set",
			userScript:     nil,
			shutdownScript: nil,
			expectedScript: nil,
			expectedError:  nil,
		},
		{
			name:           "Both set and equal",
			userScript:     script1,
			shutdownScript: script1,
			expectedScript: script1,
			expectedError:  nil,
		},
		{
			name:           "Both set and not equal",
			userScript:     script1,
			shutdownScript: script2,
			expectedScript: nil,
			expectedError:  chancloser.ErrUpfrontShutdownScriptMismatch,
		},
		{
			name:           "Only upfront script",
			userScript:     nil,
			shutdownScript: script1,
			expectedScript: script1,
			expectedError:  nil,
		},
		{
			name:           "Only user script",
			userScript:     script2,
			shutdownScript: nil,
			expectedScript: script2,
			expectedError:  nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			script, err := chooseDeliveryScript(
				test.shutdownScript, test.userScript,
			)
			if err != test.expectedError {
				t.Fatalf("Expected: %v, got: %v", test.expectedError, err)
			}

			if !bytes.Equal(script, test.expectedScript) {
				t.Fatalf("Expected: %x, got: %x", test.expectedScript, script)
			}
		})
	}
}

// TestCustomShutdownScript tests that the delivery script of a shutdown
// message can be set to a specified address. It checks that setting a close
// script fails for channels which have an upfront shutdown script already set.
func TestCustomShutdownScript(t *testing.T) {
	script := genScript(t, p2SHAddress)

	// setShutdown is a function which sets the upfront shutdown address for
	// the local channel.
	setShutdown := func(a, b *channeldb.OpenChannel) {
		a.LocalShutdownScript = script
		b.RemoteShutdownScript = script
	}

	tests := []struct {
		name string

		// update is a function used to set values on the channel set up for the
		// test. It is used to set values for upfront shutdown addresses.
		update func(a, b *channeldb.OpenChannel)

		// userCloseScript is the address specified by the user.
		userCloseScript lnwire.DeliveryAddress

		// expectedScript is the address we expect to be set on the shutdown
		// message.
		expectedScript lnwire.DeliveryAddress

		// expectedError is the error we expect, if any.
		expectedError error
	}{
		{
			name:            "User set script",
			update:          noUpdate,
			userCloseScript: script,
			expectedScript:  script,
		},
		{
			name:   "No user set script",
			update: noUpdate,
		},
		{
			name:           "Shutdown set, no user script",
			update:         setShutdown,
			expectedScript: script,
		},
		{
			name:            "Shutdown set, user script matches",
			update:          setShutdown,
			userCloseScript: script,
			expectedScript:  script,
		},
		{
			name:            "Shutdown set, user script different",
			update:          setShutdown,
			userCloseScript: []byte("different addr"),
			expectedError:   chancloser.ErrUpfrontShutdownScriptMismatch,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			notifier := &mockNotifier{
				confChannel: make(chan *chainntnfs.TxConfirmation),
			}
			broadcastTxChan := make(chan *wire.MsgTx)

			// Open a channel.
			alicePeer, bobChan, cleanUp, err := createTestPeer(
				notifier, broadcastTxChan, test.update,
			)
			if err != nil {
				t.Fatalf("unable to create test channels: %v", err)
			}
			defer cleanUp()

			// Request initiator to cooperatively close the channel, with
			// a specified delivery address.
			updateChan := make(chan interface{}, 1)
			errChan := make(chan error, 1)
			chanPoint := bobChan.ChannelPoint()
			closeCommand := htlcswitch.ChanClose{
				CloseType:      htlcswitch.CloseRegular,
				ChanPoint:      chanPoint,
				Updates:        updateChan,
				TargetFeePerKB: 12500,
				DeliveryScript: test.userCloseScript,
				Err:            errChan,
			}

			// Send the close command for the correct channel and check that a
			// shutdown message is sent.
			alicePeer.localCloseChanReqs <- &closeCommand

			var msg lnwire.Message
			select {
			case outMsg := <-alicePeer.outgoingQueue:
				msg = outMsg.msg
			case <-time.After(timeout):
				t.Fatalf("did not receive shutdown message")
			case err := <-errChan:
				// Fail if we do not expect an error.
				if err != test.expectedError {
					t.Fatalf("error closing channel: %v", err)
				}

				// Terminate the test early if have received an error, no
				// further action is expected.
				return
			}

			// Check that we have received a shutdown message.
			shutdownMsg, ok := msg.(*lnwire.Shutdown)
			if !ok {
				t.Fatalf("expected shutdown message, got %T", msg)
			}

			// If the test has not specified an expected address, do not check
			// whether the shutdown address matches. This covers the case where
			// we epect shutdown to a random address and cannot match it.
			if len(test.expectedScript) == 0 {
				return
			}

			// Check that the Shutdown message includes the expected delivery
			// script.
			if !bytes.Equal(test.expectedScript, shutdownMsg.Address) {
				t.Fatalf("expected delivery script: %x, got: %x",
					test.expectedScript, shutdownMsg.Address)
			}
		})
	}
}

// genScript creates a script paying out to the address provided, which must
// be a valid address.
func genScript(t *testing.T, address string) lnwire.DeliveryAddress {
	// Generate an address which can be used for testing.
	deliveryAddr, err := stdaddr.DecodeAddress(
		address,
		chaincfg.TestNet3Params(),
	)
	if err != nil {
		t.Fatalf("invalid delivery address: %v", err)
	}

	script, err := input.PayToAddrScript(deliveryAddr)
	if err != nil {
		t.Fatalf("cannot create script: %v", err)
	}

	return script
}
