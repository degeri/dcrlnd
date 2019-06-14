package wtdb_test

import (
	"bytes"
	crand "crypto/rand"
	"io"
	"io/ioutil"
	"net"
	"os"
	"reflect"
	"testing"

	"github.com/decred/dcrlnd/lnwire"
	"github.com/decred/dcrlnd/watchtower/blob"
	"github.com/decred/dcrlnd/watchtower/wtclient"
	"github.com/decred/dcrlnd/watchtower/wtdb"
	"github.com/decred/dcrlnd/watchtower/wtmock"
	"github.com/decred/dcrlnd/watchtower/wtpolicy"
)

// clientDBInit is a closure used to initialize a wtclient.DB instance its
// cleanup function.
type clientDBInit func(t *testing.T) (wtclient.DB, func())

type clientDBHarness struct {
	t  *testing.T
	db wtclient.DB
}

func newClientDBHarness(t *testing.T, init clientDBInit) (*clientDBHarness, func()) {
	db, cleanup := init(t)

	h := &clientDBHarness{
		t:  t,
		db: db,
	}

	return h, cleanup
}

func (h *clientDBHarness) insertSession(session *wtdb.ClientSession, expErr error) {
	h.t.Helper()

	err := h.db.CreateClientSession(session)
	if err != expErr {
		h.t.Fatalf("expected create client session error: %v, got: %v",
			expErr, err)
	}
}

func (h *clientDBHarness) listSessions() map[wtdb.SessionID]*wtdb.ClientSession {
	h.t.Helper()

	sessions, err := h.db.ListClientSessions()
	if err != nil {
		h.t.Fatalf("unable to list client sessions: %v", err)
	}

	return sessions
}

func (h *clientDBHarness) nextKeyIndex(id wtdb.TowerID, expErr error) uint32 {
	h.t.Helper()

	index, err := h.db.NextSessionKeyIndex(id)
	if err != expErr {
		h.t.Fatalf("expected next session key index error: %v, got: %v",
			expErr, err)
	}

	if index == 0 {
		h.t.Fatalf("next key index should never be 0")
	}

	return index
}

func (h *clientDBHarness) createTower(lnAddr *lnwire.NetAddress,
	expErr error) *wtdb.Tower {

	h.t.Helper()

	tower, err := h.db.CreateTower(lnAddr)
	if err != expErr {
		h.t.Fatalf("expected create tower error: %v, got: %v", expErr, err)
	}

	if tower.ID == 0 {
		h.t.Fatalf("tower id should never be 0")
	}

	return tower
}

func (h *clientDBHarness) loadTower(id wtdb.TowerID, expErr error) *wtdb.Tower {
	h.t.Helper()

	tower, err := h.db.LoadTower(id)
	if err != expErr {
		h.t.Fatalf("expected load tower error: %v, got: %v", expErr, err)
	}

	return tower
}

func (h *clientDBHarness) fetchChanSummaries() map[lnwire.ChannelID]wtdb.ClientChanSummary {
	h.t.Helper()

	summaries, err := h.db.FetchChanSummaries()
	if err != nil {
		h.t.Fatalf("unable to fetch chan summaries: %v", err)
	}

	return summaries
}

func (h *clientDBHarness) registerChan(chanID lnwire.ChannelID,
	sweepPkScript []byte, expErr error) {

	h.t.Helper()

	err := h.db.RegisterChannel(chanID, sweepPkScript)
	if err != expErr {
		h.t.Fatalf("expected register channel error: %v, got: %v",
			expErr, err)
	}
}

func (h *clientDBHarness) commitUpdate(id *wtdb.SessionID,
	update *wtdb.CommittedUpdate, expErr error) uint16 {

	h.t.Helper()

	lastApplied, err := h.db.CommitUpdate(id, update)
	if err != expErr {
		h.t.Fatalf("expected commit update error: %v, got: %v",
			expErr, err)
	}

	return lastApplied
}

func (h *clientDBHarness) ackUpdate(id *wtdb.SessionID, seqNum uint16,
	lastApplied uint16, expErr error) {

	h.t.Helper()

	err := h.db.AckUpdate(id, seqNum, lastApplied)
	if err != expErr {
		h.t.Fatalf("expected commit update error: %v, got: %v",
			expErr, err)
	}
}

// testCreateClientSession asserts various conditions regarding the creation of
// a new ClientSession. The test asserts:
//   - client sessions can only be created if a session key index is reserved.
//   - client sessions cannot be created with an incorrect session key index .
//   - inserting duplicate sessions fails.
func testCreateClientSession(h *clientDBHarness) {
	// Create a test client session to insert.
	session := &wtdb.ClientSession{
		ClientSessionBody: wtdb.ClientSessionBody{
			TowerID: wtdb.TowerID(3),
			Policy: wtpolicy.Policy{
				MaxUpdates: 100,
			},
			RewardPkScript: []byte{0x01, 0x02, 0x03},
		},
		ID: wtdb.SessionID([33]byte{0x01}),
	}

	// First, assert that this session is not already present in the
	// database.
	if _, ok := h.listSessions()[session.ID]; ok {
		h.t.Fatalf("session for id %x should not exist yet", session.ID)
	}

	// Attempting to insert the client session without reserving a session
	// key index should fail.
	h.insertSession(session, wtdb.ErrNoReservedKeyIndex)

	// Now, reserve a session key for this tower.
	keyIndex := h.nextKeyIndex(session.TowerID, nil)

	// The client session hasn't been updated with the reserved key index
	// (since it's still zero). Inserting should fail due to the mismatch.
	h.insertSession(session, wtdb.ErrIncorrectKeyIndex)

	// Reserve another key for the same index. Since no session has been
	// successfully created, it should return the same index to maintain
	// idempotency across restarts.
	keyIndex2 := h.nextKeyIndex(session.TowerID, nil)
	if keyIndex != keyIndex2 {
		h.t.Fatalf("next key index should be idempotent: want: %v, "+
			"got %v", keyIndex, keyIndex2)
	}

	// Now, set the client session's key index so that it is proper and
	// insert it. This should succeed.
	session.KeyIndex = keyIndex
	h.insertSession(session, nil)

	// Verify that the session now exists in the database.
	if _, ok := h.listSessions()[session.ID]; !ok {
		h.t.Fatalf("session for id %x should exist now", session.ID)
	}

	// Attempt to insert the session again, which should fail due to the
	// session already existing.
	h.insertSession(session, wtdb.ErrClientSessionAlreadyExists)

	// Finally, assert that reserving another key index succeeds with a
	// different key index, now that the first one has been finalized.
	keyIndex3 := h.nextKeyIndex(session.TowerID, nil)
	if keyIndex == keyIndex3 {
		h.t.Fatalf("key index still reserved after creating session")
	}
}

// testCreateTower asserts the behavior of creating new Tower objects within the
// database, and that the latest address is always prepended to the list of
// known addresses for the tower.
func testCreateTower(h *clientDBHarness) {
	// Test that loading a tower with an arbitrary tower id fails.
	h.loadTower(20, wtdb.ErrTowerNotFound)

	pk, err := randPubKey()
	if err != nil {
		h.t.Fatalf("unable to generate pubkey: %v", err)
	}

	addr1 := &net.TCPAddr{IP: []byte{0x01, 0x00, 0x00, 0x00}, Port: 9911}
	lnAddr := &lnwire.NetAddress{
		IdentityKey: pk,
		Address:     addr1,
	}

	// Insert a random tower into the database.
	tower := h.createTower(lnAddr, nil)

	// Load the tower from the database and assert that it matches the tower
	// we created.
	tower2 := h.loadTower(tower.ID, nil)
	if !reflect.DeepEqual(tower, tower2) {
		h.t.Fatalf("loaded tower mismatch, want: %v, got: %v",
			tower, tower2)
	}

	// Insert the address again into the database. Since the address is the
	// same, this should result in an unmodified tower record.
	towerDupAddr := h.createTower(lnAddr, nil)
	if len(towerDupAddr.Addresses) != 1 {
		h.t.Fatalf("duplicate address should be deduped")
	}
	if !reflect.DeepEqual(tower, towerDupAddr) {
		h.t.Fatalf("mismatch towers, want: %v, got: %v",
			tower, towerDupAddr)
	}

	// Generate a new address for this tower.
	addr2 := &net.TCPAddr{IP: []byte{0x02, 0x00, 0x00, 0x00}, Port: 9911}

	lnAddr2 := &lnwire.NetAddress{
		IdentityKey: pk,
		Address:     addr2,
	}

	// Insert the updated address, which should produce a tower with a new
	// address.
	towerNewAddr := h.createTower(lnAddr2, nil)

	// Load the tower from the database, and assert that it matches the
	// tower returned from creation.
	towerNewAddr2 := h.loadTower(tower.ID, nil)
	if !reflect.DeepEqual(towerNewAddr, towerNewAddr2) {
		h.t.Fatalf("loaded tower mismatch, want: %v, got: %v",
			towerNewAddr, towerNewAddr2)
	}

	// Assert that there are now two addresses on the tower object.
	if len(towerNewAddr.Addresses) != 2 {
		h.t.Fatalf("new address should be added")
	}

	// Finally, assert that the new address was prepended since it is deemed
	// fresher.
	if !reflect.DeepEqual(tower.Addresses, towerNewAddr.Addresses[1:]) {
		h.t.Fatalf("new address should be prepended")
	}
}

// testChanSummaries tests the process of a registering a channel and its
// associated sweep pkscript.
func testChanSummaries(h *clientDBHarness) {
	// First, assert that this channel is not already registered.
	var chanID lnwire.ChannelID
	if _, ok := h.fetchChanSummaries()[chanID]; ok {
		h.t.Fatalf("pkscript for channel %x should not exist yet",
			chanID)
	}

	// Generate a random sweep pkscript and register it for this channel.
	expPkScript := make([]byte, 22)
	if _, err := io.ReadFull(crand.Reader, expPkScript); err != nil {
		h.t.Fatalf("unable to generate pkscript: %v", err)
	}
	h.registerChan(chanID, expPkScript, nil)

	// Assert that the channel exists and that its sweep pkscript matches
	// the one we registered.
	summary, ok := h.fetchChanSummaries()[chanID]
	if !ok {
		h.t.Fatalf("pkscript for channel %x should not exist yet",
			chanID)
	} else if !bytes.Equal(expPkScript, summary.SweepPkScript) {
		h.t.Fatalf("pkscript mismatch, want: %x, got: %x",
			expPkScript, summary.SweepPkScript)
	}

	// Finally, assert that re-registering the same channel produces a
	// failure.
	h.registerChan(chanID, expPkScript, wtdb.ErrChannelAlreadyRegistered)
}

// testCommitUpdate tests the behavior of CommitUpdate, ensuring that they can
func testCommitUpdate(h *clientDBHarness) {
	session := &wtdb.ClientSession{
		ClientSessionBody: wtdb.ClientSessionBody{
			TowerID: wtdb.TowerID(3),
			Policy: wtpolicy.Policy{
				MaxUpdates: 100,
			},
			RewardPkScript: []byte{0x01, 0x02, 0x03},
		},
		ID: wtdb.SessionID([33]byte{0x02}),
	}

	// Generate a random update and try to commit before inserting the
	// session, which should fail.
	update1 := randCommittedUpdate(h.t, 1)
	h.commitUpdate(&session.ID, update1, wtdb.ErrClientSessionNotFound)

	// Reserve a session key index and insert the session.
	session.KeyIndex = h.nextKeyIndex(session.TowerID, nil)
	h.insertSession(session, nil)

	// Now, try to commit the update that failed initially which should
	// succeed. The lastApplied value should be 0 since we have not received
	// an ack from the tower.
	lastApplied := h.commitUpdate(&session.ID, update1, nil)
	if lastApplied != 0 {
		h.t.Fatalf("last applied mismatch, want: 0, got: %v",
			lastApplied)
	}

	// Assert that the committed update appears in the client session's
	// CommittedUpdates map when loaded from disk and that there are no
	// AckedUpdates.
	dbSession := h.listSessions()[session.ID]
	checkCommittedUpdates(h.t, dbSession, []wtdb.CommittedUpdate{
		*update1,
	})
	checkAckedUpdates(h.t, dbSession, nil)

	// Try to commit the same update, which should succeed due to
	// idempotency (which is preserved when the breach hint is identical to
	// the on-disk update's hint). The lastApplied value should remain
	// unchanged.
	lastApplied2 := h.commitUpdate(&session.ID, update1, nil)
	if lastApplied2 != lastApplied {
		h.t.Fatalf("last applied should not have changed, got %v",
			lastApplied2)
	}

	// Assert that the loaded ClientSession is the same as before.
	dbSession = h.listSessions()[session.ID]
	checkCommittedUpdates(h.t, dbSession, []wtdb.CommittedUpdate{
		*update1,
	})
	checkAckedUpdates(h.t, dbSession, nil)

	// Generate another random update and try to commit it at the identical
	// sequence number. Since the breach hint has changed, this should fail.
	update2 := randCommittedUpdate(h.t, 1)
	h.commitUpdate(&session.ID, update2, wtdb.ErrUpdateAlreadyCommitted)

	// Next, insert the new update at the next unallocated sequence number
	// which should succeed.
	update2.SeqNum = 2
	lastApplied3 := h.commitUpdate(&session.ID, update2, nil)
	if lastApplied3 != lastApplied {
		h.t.Fatalf("last applied should not have changed, got %v",
			lastApplied3)
	}

	// Check that both updates now appear as committed on the ClientSession
	// loaded from disk.
	dbSession = h.listSessions()[session.ID]
	checkCommittedUpdates(h.t, dbSession, []wtdb.CommittedUpdate{
		*update1,
		*update2,
	})
	checkAckedUpdates(h.t, dbSession, nil)

	// Finally, create one more random update and try to commit it at index
	// 4, which should be rejected since 3 is the next slot the database
	// expects.
	update4 := randCommittedUpdate(h.t, 4)
	h.commitUpdate(&session.ID, update4, wtdb.ErrCommitUnorderedUpdate)

	// Assert that the ClientSession loaded from disk remains unchanged.
	dbSession = h.listSessions()[session.ID]
	checkCommittedUpdates(h.t, dbSession, []wtdb.CommittedUpdate{
		*update1,
		*update2,
	})
	checkAckedUpdates(h.t, dbSession, nil)
}

// testAckUpdate asserts the behavior of AckUpdate.
func testAckUpdate(h *clientDBHarness) {
	// Create a new session that the updates in this will be tied to.
	session := &wtdb.ClientSession{
		ClientSessionBody: wtdb.ClientSessionBody{
			TowerID: wtdb.TowerID(3),
			Policy: wtpolicy.Policy{
				MaxUpdates: 100,
			},
			RewardPkScript: []byte{0x01, 0x02, 0x03},
		},
		ID: wtdb.SessionID([33]byte{0x03}),
	}

	// Try to ack an update before inserting the client session, which
	// should fail.
	h.ackUpdate(&session.ID, 1, 0, wtdb.ErrClientSessionNotFound)

	// Reserve a session key and insert the client session.
	session.KeyIndex = h.nextKeyIndex(session.TowerID, nil)
	h.insertSession(session, nil)

	// Now, try to ack update 1. This should fail since update 1 was never
	// committed.
	h.ackUpdate(&session.ID, 1, 0, wtdb.ErrCommittedUpdateNotFound)

	// Commit to a random update at seqnum 1.
	update1 := randCommittedUpdate(h.t, 1)
	lastApplied := h.commitUpdate(&session.ID, update1, nil)
	if lastApplied != 0 {
		h.t.Fatalf("last applied mismatch, want: 0, got: %v",
			lastApplied)
	}

	// Acking seqnum 1 should succeed.
	h.ackUpdate(&session.ID, 1, 1, nil)

	// Acking seqnum 1 again should fail.
	h.ackUpdate(&session.ID, 1, 1, wtdb.ErrCommittedUpdateNotFound)

	// Acking a valid seqnum with a reverted last applied value should fail.
	h.ackUpdate(&session.ID, 1, 0, wtdb.ErrLastAppliedReversion)

	// Acking with a last applied greater than any allocated seqnum should
	// fail.
	h.ackUpdate(&session.ID, 4, 3, wtdb.ErrUnallocatedLastApplied)

	// Assert that the ClientSession loaded from disk has one update in it's
	// AckedUpdates map, and that the committed update has been removed.
	dbSession := h.listSessions()[session.ID]
	checkCommittedUpdates(h.t, dbSession, nil)
	checkAckedUpdates(h.t, dbSession, map[uint16]wtdb.BackupID{
		1: update1.BackupID,
	})

	// Commit to another random update, and assert that the last applied
	// value is 1, since this was what was provided in the last successful
	// ack.
	update2 := randCommittedUpdate(h.t, 2)
	lastApplied = h.commitUpdate(&session.ID, update2, nil)
	if lastApplied != 1 {
		h.t.Fatalf("last applied mismatch, want: 1, got: %v",
			lastApplied)
	}

	// Ack seqnum 2.
	h.ackUpdate(&session.ID, 2, 2, nil)

	// Assert that both updates exist as AckedUpdates when loaded from disk.
	dbSession = h.listSessions()[session.ID]
	checkCommittedUpdates(h.t, dbSession, nil)
	checkAckedUpdates(h.t, dbSession, map[uint16]wtdb.BackupID{
		1: update1.BackupID,
		2: update2.BackupID,
	})

	// Acking again with a lower last applied should fail.
	h.ackUpdate(&session.ID, 2, 1, wtdb.ErrLastAppliedReversion)

	// Acking an unallocated seqnum should fail.
	h.ackUpdate(&session.ID, 4, 2, wtdb.ErrCommittedUpdateNotFound)

	// Acking with a last applied greater than any allocated seqnum should
	// fail.
	h.ackUpdate(&session.ID, 4, 3, wtdb.ErrUnallocatedLastApplied)
}

// checkCommittedUpdates asserts that the CommittedUpdates on session match the
// expUpdates provided.
func checkCommittedUpdates(t *testing.T, session *wtdb.ClientSession,
	expUpdates []wtdb.CommittedUpdate) {

	t.Helper()

	// We promote nil expUpdates to an initialized slice since the database
	// should never return a nil slice. This promotion is done purely out of
	// convenience for the testing framework.
	if expUpdates == nil {
		expUpdates = make([]wtdb.CommittedUpdate, 0)
	}

	if !reflect.DeepEqual(session.CommittedUpdates, expUpdates) {
		t.Fatalf("committed updates mismatch, want: %v, got: %v",
			expUpdates, session.CommittedUpdates)
	}
}

// checkAckedUpdates asserts that the AckedUpdates on a sessio match the
// expUpdates provided.
func checkAckedUpdates(t *testing.T, session *wtdb.ClientSession,
	expUpdates map[uint16]wtdb.BackupID) {

	// We promote nil expUpdates to an initialized map since the database
	// should never return a nil map. This promotion is done purely out of
	// convenience for the testing framework.
	if expUpdates == nil {
		expUpdates = make(map[uint16]wtdb.BackupID)
	}

	if !reflect.DeepEqual(session.AckedUpdates, expUpdates) {
		t.Fatalf("acked updates mismatch, want: %v, got: %v",
			expUpdates, session.AckedUpdates)
	}
}

// TestClientDB asserts the behavior of a fresh client db, a reopened client db,
// and the mock implementation. This ensures that all databases function
// identically, especially in the negative paths.
func TestClientDB(t *testing.T) {
	dbs := []struct {
		name string
		init clientDBInit
	}{
		{
			name: "fresh clientdb",
			init: func(t *testing.T) (wtclient.DB, func()) {
				path, err := ioutil.TempDir("", "clientdb")
				if err != nil {
					t.Fatalf("unable to make temp dir: %v",
						err)
				}

				db, err := wtdb.OpenClientDB(path)
				if err != nil {
					os.RemoveAll(path)
					t.Fatalf("unable to open db: %v", err)
				}

				cleanup := func() {
					db.Close()
					os.RemoveAll(path)
				}

				return db, cleanup
			},
		},
		{
			name: "reopened clientdb",
			init: func(t *testing.T) (wtclient.DB, func()) {
				path, err := ioutil.TempDir("", "clientdb")
				if err != nil {
					t.Fatalf("unable to make temp dir: %v",
						err)
				}

				db, err := wtdb.OpenClientDB(path)
				if err != nil {
					os.RemoveAll(path)
					t.Fatalf("unable to open db: %v", err)
				}
				db.Close()

				db, err = wtdb.OpenClientDB(path)
				if err != nil {
					os.RemoveAll(path)
					t.Fatalf("unable to reopen db: %v", err)
				}

				cleanup := func() {
					db.Close()
					os.RemoveAll(path)
				}

				return db, cleanup
			},
		},
		{
			name: "mock",
			init: func(t *testing.T) (wtclient.DB, func()) {
				return wtmock.NewClientDB(), func() {}
			},
		},
	}

	tests := []struct {
		name string
		run  func(*clientDBHarness)
	}{
		{
			name: "create client session",
			run:  testCreateClientSession,
		},
		{
			name: "create tower",
			run:  testCreateTower,
		},
		{
			name: "chan summaries",
			run:  testChanSummaries,
		},
		{
			name: "commit update",
			run:  testCommitUpdate,
		},
		{
			name: "ack update",
			run:  testAckUpdate,
		},
	}

	for _, database := range dbs {
		db := database
		t.Run(db.name, func(t *testing.T) {
			t.Parallel()

			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					h, cleanup := newClientDBHarness(
						t, db.init,
					)
					defer cleanup()

					test.run(h)
				})
			}
		})
	}
}

// randCommittedUpdate generates a random committed update.
func randCommittedUpdate(t *testing.T, seqNum uint16) *wtdb.CommittedUpdate {
	var chanID lnwire.ChannelID
	if _, err := io.ReadFull(crand.Reader, chanID[:]); err != nil {
		t.Fatalf("unable to generate chan id: %v", err)
	}

	var hint blob.BreachHint
	if _, err := io.ReadFull(crand.Reader, hint[:]); err != nil {
		t.Fatalf("unable to generate breach hint: %v", err)
	}

	encBlob := make([]byte, blob.Size(blob.FlagCommitOutputs.Type()))
	if _, err := io.ReadFull(crand.Reader, encBlob); err != nil {
		t.Fatalf("unable to generate encrypted blob: %v", err)
	}

	return &wtdb.CommittedUpdate{
		SeqNum: seqNum,
		CommittedUpdateBody: wtdb.CommittedUpdateBody{
			BackupID: wtdb.BackupID{
				ChanID:       chanID,
				CommitHeight: 666,
			},
			Hint:          hint,
			EncryptedBlob: encBlob,
		},
	}
}
