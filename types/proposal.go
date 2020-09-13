/*
 *  Copyright 2018 KardiaChain
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

package types

import (
	"fmt"
	"time"

	cmn "github.com/kardiachain/go-kardiamain/lib/common"
	"github.com/kardiachain/go-kardiamain/lib/rlp"
)

// Proposal defines a block proposal for the consensus.
// It must be signed by the correct proposer for the given Height/Round
// to be considered valid. It may depend on votes from a previous round,
// a so-called Proof-of-Lock (POL) round, as noted in the POLRound and POLBlockID.
type Proposal struct {
	Height     uint64  `json:"height"`
	Round      uint32  `json:"round"`
	POLRound   uint32  `json:"pol_round"`
	Timestamp  uint64  `json:"timestamp"`    // -1 if null.
	POLBlockID BlockID `json:"pol_block_id"` // zero if null.
	Signature  []byte  `json:"signature"`
}

// NewProposal returns a new Proposal.
// If there is no POLRound, polRound should be -1.
func NewProposal(height uint64, round uint32, polRound uint32, polBlockID BlockID) *Proposal {
	return &Proposal{
		Height:     height,
		Round:      round,
		Timestamp:  uint64(time.Now().Unix()),
		POLRound:   polRound,
		POLBlockID: polBlockID,
	}
}

// SignBytes returns the Proposal bytes for signing
func (p *Proposal) SignBytes(chainID string) []byte {
	bz, err := rlp.EncodeToBytes(CreateCanonicalProposal(chainID, p))
	if err != nil {
		panic(err)
	}
	return bz
}

// String returns a short string representing the Proposal
func (p *Proposal) String() string {
	return fmt.Sprintf("Proposal{%v/%v %v (%v) %X @%v}",
		p.Height, p.Round, p.POLRound,
		p.POLBlockID,
		cmn.Fingerprint(p.Signature[:]),
		time.Unix(int64(p.Timestamp), 0))
}
