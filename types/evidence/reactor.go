/*
 *  Copyright 2020 KardiaChain
 *  This file is part of the go-kardia library.
 *
 *  The go-kardia library is free software: you can redistribute it and/or modify
 *  it under the terms of the GNU Lesser General Public License as published by
 *  the Free Software Foundation, either version 3 of the License, or
 *  (at your option) any later version.
 *
 *  The go-kardia library is distributed in the hope that it will be useful,
 *  but WITHOUT ANY WARRANTY; without even the implied warranty of
 *  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 *  GNU Lesser General Public License for more details.
 *
 *  You should have received a copy of the GNU Lesser General Public License
 *  along with the go-kardia library. If not, see <http://www.gnu.org/licenses/>.
 */

package evidence

import (
	"fmt"
	"time"

	"github.com/kardiachain/go-kardiamain/lib/clist"
	"github.com/kardiachain/go-kardiamain/lib/log"

	"github.com/kardiachain/go-kardiamain/lib/p2p"
	"github.com/kardiachain/go-kardiamain/types"

	ep "github.com/kardiachain/go-kardiamain/proto/kardiachain/evidence"
	kproto "github.com/kardiachain/go-kardiamain/proto/kardiachain/types"
)

const (
	EvidenceChannel = byte(0x38)

	maxMsgSize = 1048576 // 1MB TODO make it configurable

	broadcastEvidenceIntervalS = 60  // broadcast uncommitted evidence this often
	peerCatchupSleepIntervalMS = 100 // If peer is behind, sleep this amount
)

// Reactor handles evpool evidence broadcasting amongst peers.
type Reactor struct {
	p2p.BaseReactor
	evpool *Pool
}

// NewReactor returns a new Reactor with the given config and evpool.
func NewReactor(evpool *Pool) *Reactor {
	evR := &Reactor{
		evpool: evpool,
	}
	evR.BaseReactor = *p2p.NewBaseReactor("Evidence", evR)
	return evR
}

// SetLogger sets the Logger on the reactor and the underlying Evidence.
func (evR *Reactor) SetLogger(l log.Logger) {
	evR.Logger = l
	evR.evpool.SetLogger(l)
}

// GetChannels implements Reactor.
// It returns the list of channels for this reactor.
func (evR *Reactor) GetChannels() []*p2p.ChannelDescriptor {
	return []*p2p.ChannelDescriptor{
		{
			ID:                  EvidenceChannel,
			Priority:            5,
			RecvMessageCapacity: maxMsgSize,
		},
	}
}

// AddPeer implements Reactor.
func (evR *Reactor) AddPeer(peer p2p.Peer) {
	go evR.broadcastEvidenceRoutine(peer)
}

// Receive implements Reactor.
// It adds any received evidence to the evpool.
func (evR *Reactor) Receive(chID byte, src p2p.Peer, msgBytes []byte) {
	evis, err := decodeMsg(msgBytes)
	if err != nil {
		evR.Logger.Error("Error decoding message", "src", src, "chId", chID, "err", err, "bytes", msgBytes)
		evR.Switch.StopPeerForError(src, err)
		return
	}
	for _, ev := range evis {
		err := evR.evpool.AddEvidence(ev)
		switch err.(type) {
		case *types.ErrEvidenceInvalid:
			evR.Logger.Error(err.Error())
			// punish peer
			evR.Switch.StopPeerForError(src, err)
			return
		case nil:
		default:
			// continue to the next piece of evidence
			evR.Logger.Error("Evidence has not been added", "evidence", evis, "err", err)
		}
	}
}

// Modeled after the mempool routine.
// - Evidence accumulates in a clist.
// - Each peer has a routine that iterates through the clist,
// sending available evidence to the peer.
// - If we're waiting for new evidence and the list is not empty,
// start iterating from the beginning again.
func (evR *Reactor) broadcastEvidenceRoutine(peer p2p.Peer) {
	var next *clist.CElement
	for {

		if !peer.IsRunning() || !evR.IsRunning() {
			return
		}

		// This happens because the CElement we were looking at got garbage
		// collected (removed). That is, .NextWait() returned nil. Go ahead and
		// start from the beginning.
		if next == nil {
			<-evR.evpool.EvidenceWaitChan()
			if next = evR.evpool.EvidenceFront(); next == nil {
				continue
			}
		}

		ev := next.Value.(types.Evidence)
		evis, retry := evR.checkSendEvidenceMessage(peer, ev)
		if evis != nil {
			msgBytes, err := encodeMsg(evis)
			if err != nil {
				panic(err)
			}
			success := peer.Send(EvidenceChannel, msgBytes)
			retry = !success
		}

		if retry {
			time.Sleep(peerCatchupSleepIntervalMS * time.Millisecond)
			continue
		}

		afterCh := time.After(time.Second * broadcastEvidenceIntervalS)
		select {
		case <-afterCh:
			// start from the beginning every tick.
			// TODO: only do this if we're at the end of the list!
			next = nil
		case <-next.NextWaitChan():
			// see the start of the for loop for nil check
			next = next.Next()
		}
	}
}

// Returns the message to send the peer, or nil if the evidence is invalid for the peer.
// If message is nil, return true if we should sleep and try again.
func (evR Reactor) checkSendEvidenceMessage(
	peer p2p.Peer,
	ev types.Evidence,
) (evis []types.Evidence, retry bool) {

	// make sure the peer is up to date
	evHeight := ev.Height()
	peerState, ok := peer.Get(types.PeerStateKey).(PeerState)
	if !ok {
		// Peer does not have a state yet. We set it in the consensus reactor, but
		// when we add peer in Switch, the order we call reactors#AddPeer is
		// different every time due to us using a map. Sometimes other reactors
		// will be initialized before the consensus reactor. We should wait a few
		// milliseconds and retry.
		return nil, true
	}

	// NOTE: We only send evidence to peers where
	// peerHeight - maxAge < evidenceHeight < peerHeight
	// and
	// lastBlockTime - maxDuration < evidenceTime
	var (
		peerHeight = peerState.GetHeight()

		params = evR.evpool.State().ConsensusParams.Evidence

		ageDuration  = evR.evpool.State().LastBlockTime.Sub(ev.Time())
		ageNumBlocks = int64(peerHeight) - int64(evHeight)
	)

	if peerHeight < evHeight { // peer is behind. sleep while he catches up
		return nil, true
	} else if ageNumBlocks > params.MaxAgeNumBlocks ||
		ageDuration > time.Duration(params.MaxAgeDuration)*time.Millisecond { // evidence is too old, skip

		// NOTE: if evidence is too old for an honest peer, then we're behind and
		// either it already got committed or it never will!
		evR.Logger.Info("Not sending peer old evidence",
			"peerHeight", peerHeight,
			"evHeight", evHeight,
			"maxAgeNumBlocks", params.MaxAgeNumBlocks,
			"lastBlockTime", evR.evpool.State().LastBlockTime,
			"evTime", ev.Time(),
			"maxAgeDuration", params.MaxAgeDuration,
			"peer", peer,
		)

		return nil, false
	}

	// send evidence
	return []types.Evidence{ev}, false
}

// PeerState describes the state of a peer.
type PeerState interface {
	GetHeight() uint64
}

//-----------------------------------------------------------------------------
// Messages

// encodemsg takes a array of evidence
// returns the byte encoding of the List Message
func encodeMsg(evis []types.Evidence) ([]byte, error) {
	evi := make([]*kproto.Evidence, len(evis))
	for i := 0; i < len(evis); i++ {
		ev, err := types.EvidenceToProto(evis[i])
		if err != nil {
			return nil, err
		}
		evi[i] = ev
	}

	epl := ep.List{
		Evidence: evi,
	}

	return epl.Marshal()
}

// decodemsg takes an array of bytes
// returns an array of evidence
func decodeMsg(bz []byte) (evis []types.Evidence, err error) {
	lm := ep.List{}
	if err := lm.Unmarshal(bz); err != nil {
		return nil, err
	}

	evis = make([]types.Evidence, len(lm.Evidence))
	for i := 0; i < len(lm.Evidence); i++ {
		ev, err := types.EvidenceFromProto(lm.Evidence[i])
		if err != nil {
			return nil, err
		}
		evis[i] = ev
	}

	for i, ev := range evis {
		if err := ev.ValidateBasic(); err != nil {
			return nil, fmt.Errorf("invalid evidence (#%d): %v", i, err)
		}
	}

	return evis, nil
}
