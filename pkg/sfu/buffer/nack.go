package buffer

import (
	"time"

	"github.com/pion/rtcp"
)

const maxNackTimes = 3   // Max number of times a packet will be NACKed
const maxNackCache = 100 // Max NACK sn the sfu will keep reference

type nack struct {
	seqNum       uint16
	nacked       uint8
	lastNackTime time.Time
}

type NackQueue struct {
	nacks []*nack
	rtt   time.Duration
}

func NewNACKQueue() *NackQueue {
	return &NackQueue{
		nacks: make([]*nack, 0, maxNackCache),
	}
}

func (n *NackQueue) SetRTT(rtt uint32) {
	n.rtt = time.Duration(rtt) * time.Millisecond
}

func (n *NackQueue) Remove(sn uint16) {
	for idx, nack := range n.nacks {
		if nack.seqNum != sn {
			continue
		}

		copy(n.nacks[idx:], n.nacks[idx+1:])
		n.nacks = n.nacks[:len(n.nacks)-1]
		break
	}
}

func (n *NackQueue) Push(sn uint16) {
	// if at capacity, pop the first one
	if len(n.nacks) == cap(n.nacks) {
		copy(n.nacks[0:], n.nacks[1:])
		n.nacks = n.nacks[:len(n.nacks)-1]
	}

	n.nacks = append(n.nacks, &nack{seqNum: sn, nacked: 0, lastNackTime: time.Now()})
}

func (n *NackQueue) Pairs() ([]rtcp.NackPair, int) {
	if len(n.nacks) == 0 {
		return nil, 0
	}

	now := time.Now()

	// set it far back to get the first pair
	baseSN := n.nacks[0].seqNum - 17

	snsToPurge := []uint16{}

	numSeqNumsNacked := 0
	isPairActive := false
	var np rtcp.NackPair
	var nps []rtcp.NackPair
	for _, nack := range n.nacks {
		if nack.nacked >= maxNackTimes || now.Sub(nack.lastNackTime) < n.rtt {
			if nack.nacked >= maxNackTimes {
				snsToPurge = append(snsToPurge, nack.seqNum)
			}
			continue
		}

		nack.nacked++
		nack.lastNackTime = now
		numSeqNumsNacked++

		if (nack.seqNum - baseSN) > 16 {
			// need a new nack pair
			if isPairActive {
				nps = append(nps, np)
				isPairActive = false
			}

			baseSN = nack.seqNum

			np.PacketID = nack.seqNum
			np.LostPackets = 0
			isPairActive = true
		} else {
			np.LostPackets |= 1 << (nack.seqNum - baseSN - 1)
		}
	}

	// add any left over
	if isPairActive {
		nps = append(nps, np)
	}

	for _, sn := range snsToPurge {
		n.Remove(sn)
	}

	return nps, numSeqNumsNacked
}
