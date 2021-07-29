package netann

import (
	"bytes"
	"testing"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrlnd/channeldb"
	"github.com/decred/dcrlnd/lnwire"
	"github.com/stretchr/testify/assert"
)

func TestCreateChanAnnouncement(t *testing.T) {
	t.Parallel()

	key := [33]byte{0x1}
	sig := lnwire.Sig{0x1}
	features := lnwire.NewRawFeatureVector(lnwire.AnchorsRequired)
	var featuresBuf bytes.Buffer
	if err := features.Encode(&featuresBuf); err != nil {
		t.Fatalf("unable to encode features: %v", err)
	}

	expChanAnn := &lnwire.ChannelAnnouncement{
		ChainHash:       chainhash.Hash{0x1},
		ShortChannelID:  lnwire.ShortChannelID{BlockHeight: 1},
		NodeID1:         key,
		NodeID2:         key,
		NodeSig1:        sig,
		NodeSig2:        sig,
		DecredKey1:      key,
		DecredKey2:      key,
		DecredSig1:      sig,
		DecredSig2:      sig,
		Features:        features,
		ExtraOpaqueData: []byte{0x1},
	}

	chanProof := &channeldb.ChannelAuthProof{
		NodeSig1Bytes:   expChanAnn.NodeSig1.ToSignatureBytes(),
		NodeSig2Bytes:   expChanAnn.NodeSig2.ToSignatureBytes(),
		DecredSig1Bytes: expChanAnn.DecredSig1.ToSignatureBytes(),
		DecredSig2Bytes: expChanAnn.DecredSig2.ToSignatureBytes(),
	}
	chanInfo := &channeldb.ChannelEdgeInfo{
		ChainHash:       expChanAnn.ChainHash,
		ChannelID:       expChanAnn.ShortChannelID.ToUint64(),
		ChannelPoint:    wire.OutPoint{Index: 1},
		Capacity:        dcrutil.AtomsPerCoin,
		NodeKey1Bytes:   key,
		NodeKey2Bytes:   key,
		DecredKey1Bytes: key,
		DecredKey2Bytes: key,
		Features:        featuresBuf.Bytes(),
		ExtraOpaqueData: expChanAnn.ExtraOpaqueData,
	}
	chanAnn, _, _, err := CreateChanAnnouncement(
		chanProof, chanInfo, nil, nil,
	)
	if err != nil {
		t.Fatalf("unable to create channel announcement: %v", err)
	}

	assert.Equal(t, chanAnn, expChanAnn)
}
