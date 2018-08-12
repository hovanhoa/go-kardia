package consensus

import (
	"fmt"
	"sync"
	"time"

	cstypes "github.com/kardiachain/go-kardia/consensus/types"
	"github.com/kardiachain/go-kardia/kai"
	// TODO(namdoh): Remove kai/common dependency
	kcmn "github.com/kardiachain/go-kardia/kai/common"
	cmn "github.com/kardiachain/go-kardia/lib/common"
	libevents "github.com/kardiachain/go-kardia/lib/events"
	"github.com/kardiachain/go-kardia/lib/log"
	"github.com/kardiachain/go-kardia/p2p"
	"github.com/kardiachain/go-kardia/p2p/discover"
	"github.com/kardiachain/go-kardia/types"
)

const (
	StateChannel       = byte(0x20)
	DataChannel        = byte(0x21)
	VoteChannel        = byte(0x22)
	VoteSetBitsChannel = byte(0x23)

	maxMsgSize = 1048576 // 1MB; NOTE/TODO: keep in sync with types.PartSet sizes.

	blocksToContributeToBecomeGoodPeer = 10000
)

type PeerConnection struct {
	peer *p2p.Peer
	rw   p2p.MsgReadWriter
}

func (pc *PeerConnection) SendConsensusMessage(msg ConsensusMessage) error {
	return p2p.Send(pc.rw, kcmn.CsNewRoundStepMsg, msg)
}

// ConsensusReactor defines a reactor for the consensus service.
type ConsensusReactor struct {
	kai.BaseReactor

	conS *ConsensusState

	mtx sync.RWMutex
	//eventBus *types.EventBus

	running bool
}

// NewConsensusReactor returns a new ConsensusReactor with the given
// consensusState.
func NewConsensusReactor(consensusState *ConsensusState) *ConsensusReactor {
	return &ConsensusReactor{
		conS: consensusState,
	}
	// TODO(namdoh): Re-anable this.
	//conR := &ConsensusReactor{
	//	conS:     consensusState,
	//	fastSync: fastSync,
	//}
	//conR.BaseReactor = *p2p.NewBaseReactor("ConsensusReactor", conR)
	//r eturn conR
}

func (conR *ConsensusReactor) SetNodeID(nodeID discover.NodeID) {
	conR.conS.SetNodeID(nodeID)
}

func (conR *ConsensusReactor) SetPrivValidator(priv *types.PrivValidator) {
	conR.conS.SetPrivValidator(priv)
}

func (conR *ConsensusReactor) Start() {
	conR.running = true

	conR.subscribeToBroadcastEvents()
	conR.conS.Start()
}

func (conR *ConsensusReactor) Stop() {

	conR.conS.Stop()
	conR.unsubscribeFromBroadcastEvents()

	conR.running = false
}

// AddPeer implements Reactor
func (conR *ConsensusReactor) AddPeer(p *p2p.Peer, rw p2p.MsgReadWriter) {
	log.Info("Add peer to reactor.")
	peerConnection := PeerConnection{peer: p, rw: rw}
	conR.sendNewRoundStepMessages(peerConnection)

	if !conR.running {
		return
	}

	//// Create peerState for peer
	peerState := NewPeerState(p).SetLogger(conR.conS.Logger)
	p.Set(p2p.PeerStateKey, peerState)

	// Begin routines for this peer.
	go conR.gossipDataRoutine(&peerConnection, peerState)
	//go conR.gossipVotesRoutine(p, peerState)
	//go conR.queryMaj23Routine(p, peerState)

	//// Send our state to peer.
	//// If we're fast_syncing, broadcast a RoundStepMessage later upon SwitchToConsensus().
	//if !conR.FastSync() {
	//	conR.sendNewRoundStepMessages(peer)
	//}
}

func (conR *ConsensusReactor) RemovePeer(p *p2p.Peer, reason interface{}) {
	log.Error("ConsensusReactor.RemovePeer - not yet implemented")
}

// subscribeToBroadcastEvents subscribes for new round steps, votes and
// proposal heartbeats using internal pubsub defined on state to broadcast
// them to peers upon receiving.
func (conR *ConsensusReactor) subscribeToBroadcastEvents() {
	const subscriber = "consensus-reactor"
	conR.conS.evsw.AddListenerForEvent(subscriber, types.EventNewRoundStep,
		func(data libevents.EventData) {
			conR.broadcastNewRoundStepMessages(data.(*cstypes.RoundState))
		})

	conR.conS.evsw.AddListenerForEvent(subscriber, types.EventVote,
		func(data libevents.EventData) {
			conR.broadcastHasVoteMessage(data.(*types.Vote))
		})

	//namdoh@ conR.conS.evsw.AddListenerForEvent(subscriber, types.EventProposalHeartbeat,
	//namdoh@ 	func(data libevents.EventData) {
	//namdoh@ 		conR.broadcastProposalHeartbeatMessage(data.(*types.Heartbeat))
	//namdoh@ 	})
}

func (conR *ConsensusReactor) unsubscribeFromBroadcastEvents() {
	const subscriber = "consensus-reactor"
	conR.conS.evsw.RemoveListener(subscriber)
}

// ------------ Message handlers ---------

// Handles received NewRoundStepMessage
func (conR *ConsensusReactor) ReceiveNewRoundStep(generalMsg p2p.Msg, src *p2p.Peer) {
	conR.conS.Logger.Trace("Consensus reactor received NewRoundStep", "src", src, "msg", generalMsg)

	if !conR.running {
		conR.conS.Logger.Trace("Consensus reactor isn't running.")
		return
	}

	var msg NewRoundStepMessage
	if err := generalMsg.Decode(&msg); err != nil {
		conR.conS.Logger.Error("Invalid message", "msg", generalMsg, "err", err)
		return
	}
	conR.conS.Logger.Trace("Decoded msg", "msg", msg)

	// Get peer states
	ps, ok := src.Get(p2p.PeerStateKey).(*PeerState)
	if !ok {
		conR.conS.Logger.Error("Downcast failed!!")
		return
	}

	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	// Ignore duplicates or decreases
	if CompareHRS(msg.Height, msg.Round, msg.Step, ps.PRS.Height, ps.PRS.Round, ps.PRS.Step) <= 0 {
		return
	}

	// Just remember these values.
	psHeight := ps.PRS.Height
	psRound := ps.PRS.Round
	//psStep := ps.PRS.Step
	psCatchupCommitRound := ps.PRS.CatchupCommitRound
	psCatchupCommit := ps.PRS.CatchupCommit

	startTime := time.Now().Add(-1 * time.Duration(msg.SecondsSinceStartTime) * time.Second)
	ps.PRS.Height = msg.Height
	ps.PRS.Round = msg.Round
	ps.PRS.Step = msg.Step
	ps.PRS.StartTime = startTime
	if !psHeight.Equals(msg.Height) || !psRound.Equals(msg.Round) {
		ps.PRS.Proposal = false
		ps.PRS.ProposalBlockHeader = cmn.Hash{}
		ps.PRS.ProposalPOLRound = cmn.NewBigInt(-1)
		ps.PRS.ProposalPOL = nil
		// We'll update the BitArray capacity later.
		ps.PRS.Prevotes = nil
		ps.PRS.Precommits = nil
	}
	if psHeight == msg.Height && psRound != msg.Round && msg.Round == psCatchupCommitRound {
		// Peer caught up to CatchupCommitRound.
		// Preserve psCatchupCommit!
		// NOTE: We prefer to use prs.Precommits if
		// pr.Round matches pr.CatchupCommitRound.
		ps.PRS.Precommits = psCatchupCommit
	}
	if psHeight != msg.Height {
		// Shift Precommits to LastCommit.
		if psHeight.Add(1).Equals(msg.Height) && psRound.Equals(msg.LastCommitRound) {
			ps.PRS.LastCommitRound = msg.LastCommitRound
			ps.PRS.LastCommit = ps.PRS.Precommits
		} else {
			ps.PRS.LastCommitRound = msg.LastCommitRound
			ps.PRS.LastCommit = nil
		}
		// We'll update the BitArray capacity later.
		ps.PRS.CatchupCommitRound = cmn.NewBigInt(-1)
		ps.PRS.CatchupCommit = nil
	}
}

func (conR *ConsensusReactor) ReceiveNewProposal(generalMsg p2p.Msg, src *p2p.Peer) {
	conR.conS.Logger.Trace("Consensus reactor received Proposal", "src", src, "msg", generalMsg)

	if !conR.running {
		conR.conS.Logger.Trace("Consensus reactor isn't running.")
		return
	}

	var msg ProposalMessage
	if err := generalMsg.Decode(&msg); err != nil {
		conR.conS.Logger.Error("Invalid proposal message", "msg", generalMsg, "err", err)
		return
	}
	conR.conS.Logger.Trace("Decoded msg", "msg", msg)

	// Get peer states
	ps, ok := src.Get(p2p.PeerStateKey).(*PeerState)
	if !ok {
		conR.conS.Logger.Error("Downcast failed!!")
		return
	}

	ps.SetHasProposal(msg.Proposal)
	conR.conS.peerMsgQueue <- msgInfo{&msg, src.ID()}
}

// dummy handler to handle new vote
func (conR *ConsensusReactor) ReceiveNewVote(generalMsg p2p.Msg, src *p2p.Peer) {
	conR.conS.Logger.Trace("Consensus reactor received NewVote", "src", src, "msg", generalMsg)

	if !conR.running {
		conR.conS.Logger.Trace("Consensus reactor isn't running.")
		return
	}

	var msg VoteMessage
	if err := generalMsg.Decode(&msg); err != nil {
		conR.conS.Logger.Error("Invalid vote message", "msg", generalMsg, "err", err)
		return
	}
	conR.conS.Logger.Trace("Decoded msg", "msg", msg)

	// Get peer states
	ps, ok := src.Get(p2p.PeerStateKey).(*PeerState)
	if !ok {
		conR.conS.Logger.Error("Downcast failed!!")
		return
	}
	ps.mtx.Lock()
	//handle vote logic
	return
	defer ps.mtx.Unlock()
}

func (conR *ConsensusReactor) ReceiveHasVote(generalMsg p2p.Msg, src *p2p.Peer) {
	conR.conS.Logger.Trace("Consensus reactor received HasVote", "src", src, "msg", generalMsg)

	if !conR.running {
		conR.conS.Logger.Trace("Consensus reactor isn't running.")
		return
	}

	var msg HasVoteMessage
	if err := generalMsg.Decode(&msg); err != nil {
		conR.conS.Logger.Error("Invalid proposal message", "msg", generalMsg, "err", err)
		return
	}
	conR.conS.Logger.Trace("Decoded msg", "msg", msg)

	// Get peer states
	ps, ok := src.Get(p2p.PeerStateKey).(*PeerState)
	if !ok {
		conR.conS.Logger.Error("Downcast failed!!")
		return
	}

	ps.ApplyHasVoteMessage(&msg)
}

// dummy handler to handle new commit
func (conR *ConsensusReactor) ReceiveNewCommit(generalMsg p2p.Msg, src *p2p.Peer) {
	conR.conS.Logger.Trace("Consensus reactor received vote", "src", src, "msg", generalMsg)

	if !conR.running {
		conR.conS.Logger.Trace("Consensus reactor isn't running.")
		return
	}

	var msg CommitStepMessage
	if err := generalMsg.Decode(&msg); err != nil {
		conR.conS.Logger.Error("Invalid commit step message", "msg", generalMsg, "err", err)
		return
	}
	conR.conS.Logger.Trace("Decoded msg", "msg", msg)

	// Get peer states
	ps, ok := src.Get(p2p.PeerStateKey).(*PeerState)
	if !ok {
		conR.conS.Logger.Error("Downcast failed!!")
		return
	}
	ps.mtx.Lock()
	//handle commit logic
	return
	defer ps.mtx.Unlock()
}

// ------------ Broadcast messages ------------

func (conR *ConsensusReactor) broadcastNewRoundStepMessages(rs *cstypes.RoundState) {
	nrsMsg, csMsg := makeRoundStepMessages(rs)
	conR.conS.Logger.Trace("broadcastNewRoundStepMessages", "nrsMsg", nrsMsg)
	if nrsMsg != nil {
		conR.ProtocolManager.Broadcast(nrsMsg, kcmn.CsNewRoundStepMsg)
	}
	if csMsg != nil {
		conR.ProtocolManager.Broadcast(csMsg, kcmn.CsCommitStepMsg)
	}
}

// Broadcasts HasVoteMessage to peers that care.
func (conR *ConsensusReactor) broadcastHasVoteMessage(vote *types.Vote) {
	msg := &HasVoteMessage{
		Height: vote.Height,
		Round:  vote.Round,
		Type:   vote.Type,
		Index:  vote.ValidatorIndex,
	}
	conR.conS.Logger.Trace("broadcastHasVoteMessage", "msg", msg)
	conR.ProtocolManager.Broadcast(msg, kcmn.CsHasVoteMsg)
	/*
		// TODO: Make this broadcast more selective.
		for _, peer := range conR.Switch.Peers().List() {
			ps := peer.Get(PeerStateKey).(*PeerState)
			prs := ps.GetRoundState()
			if prs.Height == vote.Height {
				// TODO: Also filter on round?
				peer.TrySend(StateChannel, struct{ ConsensusMessage }{msg})
			} else {
				// Height doesn't match
				// TODO: check a field, maybe CatchupCommitRound?
				// TODO: But that requires changing the struct field comment.
			}
		}
	*/
}

// ------------ Send message helpers -----------

func (conR *ConsensusReactor) sendNewRoundStepMessages(pc PeerConnection) {
	conR.conS.Logger.Debug("reactor - sendNewRoundStepMessages")

	rs := conR.conS.GetRoundState()
	nrsMsg, _ := makeRoundStepMessages(rs)
	conR.conS.Logger.Trace("makeRoundStepMessages", "nrsMsg", nrsMsg)
	if nrsMsg != nil {
		if err := pc.SendConsensusMessage(nrsMsg); err != nil {
			conR.conS.Logger.Debug("sendNewRoundStepMessages failed", "err", err)
		} else {
			conR.conS.Logger.Debug("sendNewRoundStepMessages success")
		}
	}

	// TODO(namdoh): Re-anable this.
	//rs := conR.conS.GetRoundState()
	//nrsMsg, csMsg := makeRoundStepMessages(rs)
	//if nrsMsg != nil {
	//	peer.Send(StateChannel, cdc.MustMarshalBinaryBare(nrsMsg))
	//}
	//if csMsg != nil {
	//	peer.Send(StateChannel, cdc.MustMarshalBinaryBare(csMsg))
	//}
}

// ------------ Helpers to create messages -----
func makeRoundStepMessages(rs *cstypes.RoundState) (nrsMsg *NewRoundStepMessage, csMsg *CommitStepMessage) {
	nrsMsg = &NewRoundStepMessage{
		Height: rs.Height,
		Round:  rs.Round,
		Step:   rs.Step,
		SecondsSinceStartTime: uint(time.Since(rs.StartTime).Seconds()),
		LastCommitRound:       rs.LastCommit.Round(),
	}
	if rs.Step == cstypes.RoundStepCommit {
		csMsg = &CommitStepMessage{
			Height: rs.Height,
			Block:  rs.ProposalBlock,
		}
	}
	return
}

// ----------- Gossip routines ---------------
func (conR *ConsensusReactor) gossipDataRoutine(peerConn *PeerConnection, ps *PeerState) {
	peer := peerConn.peer
	logger := conR.conS.Logger.New("peer", peer)
	logger.Trace("Start gossipDataRoutine for peer")

OUTER_LOOP:
	for {
		// Manage disconnects from self or peer.
		if !peer.IsAlive || !conR.running {
			logger.Info("Stopping gossipDataRoutine for peer")
			return
		}
		rs := conR.conS.GetRoundState()
		prs := ps.GetRoundState()

		// If the peer is on a previous height, help catch up.
		if (prs.Height.IsGreaterThanInt(0)) && (prs.Height.IsLessThan(rs.Height)) {
			//heightLogger := logger.New("height", prs.Height)

			panic("gossipDataRoutine - not yet implemented")
			//// if we never received the commit message from the peer, the block parts wont be initialized
			//if prs.ProposalBlockParts == nil {
			//	blockMeta := conR.conS.blockStore.LoadBlockMeta(prs.Height)
			//	if blockMeta == nil {
			//		cmn.PanicCrisis(cmn.Fmt("Failed to load block %d when blockStore is at %d",
			//			prs.Height, conR.conS.blockStore.Height()))
			//	}
			//	ps.InitProposalBlockParts(blockMeta.BlockID.PartsHeader)
			//	// continue the loop since prs is a copy and not effected by this initialization
			//	continue OUTER_LOOP
			//}
			//conR.gossipDataForCatchup(heightLogger, rs, prs, ps, peer)
			//continue OUTER_LOOP
		}

		// If height and round don't match, sleep.
		if !rs.Height.Equals(prs.Height) || !rs.Round.Equals(prs.Round) {
			//logger.Trace("Peer Height|Round mismatch, sleeping", "peerHeight", prs.Height, "peerRound", prs.Round, "peer", peer)
			time.Sleep(conR.conS.config.PeerGossipSleep())
			continue OUTER_LOOP
		}

		// By here, height and round match.
		// Proposal block parts were already matched and sent if any were wanted.
		// (These can match on hash so the round doesn't matter)
		// Now consider sending other things, like the Proposal itself.

		// Send Proposal && ProposalPOL BitArray?
		if rs.Proposal != nil && !prs.Proposal {
			// Proposal: share the proposal metadata with peer.
			{
				msg := &ProposalMessage{Proposal: rs.Proposal}
				logger.Debug("Sending proposal", "height", prs.Height, "round", prs.Round)
				if err := p2p.Send(peerConn.rw, kcmn.CsProposalMsg, msg); err != nil {
					logger.Trace("Sending proposal failed", "err", err)
				}
				ps.SetHasProposal(rs.Proposal)
			}
			// ProposalPOL: lets peer know which POL votes we have so far.
			// Peer must receive ProposalMessage first.
			// rs.Proposal was validated, so rs.Proposal.POLRound <= rs.Round,
			// so we definitely have rs.Votes.Prevotes(rs.Proposal.POLRound).
			if rs.Proposal.POLRound.IsGreaterThanOrEqualThanInt(0) {
				msg := &ProposalPOLMessage{
					Height:           rs.Height,
					ProposalPOLRound: rs.Proposal.POLRound,
					ProposalPOL:      rs.Votes.Prevotes(rs.Proposal.POLRound.Int32()).BitArray(),
				}
				logger.Debug("Sending POL", "height", prs.Height, "round", prs.Round)
				p2p.Send(peer.GetRW(), kcmn.CsProposalPOLMsg, msg)
			}
			continue OUTER_LOOP
		}

		// Nothing to do. Sleep.
		time.Sleep(conR.conS.config.PeerGossipSleep())
		continue OUTER_LOOP
	}
}

// ----------- Consensus Messages ------------

// ConsensusMessage is a message that can be sent and received on the ConsensusReactor
type ConsensusMessage interface{}

// VoteMessage is sent when voting for a proposal (or lack thereof).
type VoteMessage struct {
	Vote *types.Vote
}

// ProposalMessage is sent when a new block is proposed.
type ProposalMessage struct {
	Proposal *types.Proposal
}

// ProposalPOLMessage is sent when a previous proposal is re-proposed.
type ProposalPOLMessage struct {
	Height           *cmn.BigInt
	ProposalPOLRound *cmn.BigInt
	ProposalPOL      *cmn.BitArray
}

// String returns a string representation.
func (m *ProposalPOLMessage) String() string {
	return fmt.Sprintf("[ProposalPOL H:%v POLR:%v POL:%v]", m.Height, m.ProposalPOLRound, m.ProposalPOL)
}

// NewRoundStepMessage is sent for every step taken in the ConsensusState.
// For every height/round/step transition
type NewRoundStepMessage struct {
	Height                *cmn.BigInt           `json:"height" gencodoc:"required"`
	Round                 *cmn.BigInt           `json:"round" gencodoc:"required"`
	Step                  cstypes.RoundStepType `json:"step" gencodoc:"required"`
	SecondsSinceStartTime uint                  `json:"elapsed" gencodoc:"required"`
	LastCommitRound       *cmn.BigInt           `json:"lastCommitRound" gencodoc:"required"`
}

// HasVoteMessage is sent to indicate that a particular vote has been received.
type HasVoteMessage struct {
	Height *cmn.BigInt
	Round  *cmn.BigInt
	Type   byte
	Index  *cmn.BigInt
}

// String returns a string representation.
func (m *HasVoteMessage) String() string {
	return fmt.Sprintf("[HasVote VI:%v V:{%v/%02d/%v}]", m.Index, m.Height, m.Round, m.Type)
}

// CommitStepMessage is sent when a block is committed.
type CommitStepMessage struct {
	Height *cmn.BigInt  `json:"height" gencodoc:"required"`
	Block  *types.Block `json:"block" gencodoc:"required"`
}

// ---------  PeerState ---------
// PeerState contains the known state of a peer, including its connection and
// threadsafe access to its PeerRoundState.
// NOTE: THIS GETS DUMPED WITH rpc/core/consensus.go.
// Be mindful of what you Expose.
type PeerState struct {
	peer   p2p.Peer
	logger log.Logger

	mtx sync.Mutex             `json:"-"`           // NOTE: Modify below using setters, never directly.
	PRS cstypes.PeerRoundState `json:"round_state"` // Exposed.
}

// NewPeerState returns a new PeerState for the given Peer
func NewPeerState(peer *p2p.Peer) *PeerState {
	return &PeerState{
		peer: *peer,
		PRS: cstypes.PeerRoundState{
			Height:             cmn.NewBigInt(0),
			Round:              cmn.NewBigInt(-1),
			ProposalPOLRound:   cmn.NewBigInt(-1),
			LastCommitRound:    cmn.NewBigInt(-1),
			CatchupCommitRound: cmn.NewBigInt(-1),
		},
	}
}

// SetLogger allows to set a logger on the peer state. Returns the peer state
// itself.
func (ps *PeerState) SetLogger(logger log.Logger) *PeerState {
	ps.logger = logger
	return ps
}

// GetRoundState returns an shallow copy of the PeerRoundState.
// There's no point in mutating it since it won't change PeerState.
func (ps *PeerState) GetRoundState() *cstypes.PeerRoundState {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	prs := ps.PRS // copy
	return &prs
}

// SetHasProposal sets the given proposal as known for the peer.
func (ps *PeerState) SetHasProposal(proposal *types.Proposal) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	if !ps.PRS.Height.Equals(proposal.Height) || !ps.PRS.Round.Equals(proposal.Round) {
		return
	}
	if ps.PRS.Proposal {
		return
	}

	ps.PRS.Proposal = true
	ps.PRS.ProposalBlockHeader = proposal.Block.Header().Hash()
	ps.PRS.ProposalPOLRound = proposal.POLRound
	ps.PRS.ProposalPOL = nil // Nil until ProposalPOLMessage received.
}

func (ps *PeerState) getVoteBitArray(height *cmn.BigInt, round *cmn.BigInt, type_ byte) *cmn.BitArray {
	if !types.IsVoteTypeValid(type_) {
		return nil
	}

	if ps.PRS.Height.Equals(height) {
		if ps.PRS.Round.Equals(round) {
			switch type_ {
			case types.VoteTypePrevote:
				return ps.PRS.Prevotes
			case types.VoteTypePrecommit:
				return ps.PRS.Precommits
			}
		}
		// TODO(namdoh): Re-eable this once catchup is turned on.
		//if ps.PRS.CatchupCommitRound.Equals(round) {
		//	switch type_ {
		//	case types.VoteTypePrevote:
		//		return nil
		//	case types.VoteTypePrecommit:
		//		return ps.PRS.CatchupCommit
		//	}
		//}
		if ps.PRS.ProposalPOLRound.Equals(round) {
			switch type_ {
			case types.VoteTypePrevote:
				return ps.PRS.ProposalPOL
			case types.VoteTypePrecommit:
				return nil
			}
		}
		return nil
	}
	if ps.PRS.Height.Equals(height.Add(1)) {
		if ps.PRS.LastCommitRound.Equals(round) {
			switch type_ {
			case types.VoteTypePrevote:
				return nil
			case types.VoteTypePrecommit:
				return ps.PRS.LastCommit
			}
		}
		return nil
	}
	return nil
}

// SetHasVote sets the given vote as known by the peer
func (ps *PeerState) SetHasVote(vote *types.Vote) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	ps.setHasVote(vote.Height, vote.Round, vote.Type, vote.ValidatorIndex)
}

func (ps *PeerState) setHasVote(height *cmn.BigInt, round *cmn.BigInt, type_ byte, index *cmn.BigInt) {
	logger := ps.logger.New("peerH/R", cmn.Fmt("%d/%d", ps.PRS.Height, ps.PRS.Round), "H/R", cmn.Fmt("%d/%d", height, round))
	logger.Debug("setHasVote", "type", type_, "index", index)

	// NOTE: some may be nil BitArrays -> no side effects.
	psVotes := ps.getVoteBitArray(height, round, type_)
	if psVotes != nil {
		psVotes.SetIndex(index.Int32(), true)
	}
}

// ------ Functions to apply to PeerState ----------
// ApplyHasVoteMessage updates the peer state for the new vote.
func (ps *PeerState) ApplyHasVoteMessage(msg *HasVoteMessage) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	if !ps.PRS.Height.Equals(msg.Height) {
		return
	}

	ps.setHasVote(msg.Height, msg.Round, msg.Type, msg.Index)
}
