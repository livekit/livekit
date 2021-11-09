package buffer

import (
	"sort"

	"github.com/pion/rtcp"
)

const maxNackTimes = 3   // Max number of times a packet will be NACKed
const maxNackCache = 100 // Max NACK sn the sfu will keep reference

type nack struct {
	sn     uint32
	nacked uint8
}

type nackQueue struct {
	nacks []nack
	kfSN  uint32
}

func newNACKQueue() *nackQueue {
	return &nackQueue{
		nacks: make([]nack, 0, maxNackCache+1),
	}
}

func (n *nackQueue) remove(extSN uint32) {
	i := sort.Search(len(n.nacks), func(i int) bool { return n.nacks[i].sn >= extSN })
	if i >= len(n.nacks) || n.nacks[i].sn != extSN {
		return
	}
	copy(n.nacks[i:], n.nacks[i+1:])
	n.nacks = n.nacks[:len(n.nacks)-1]
}

func (n *nackQueue) push(extSN uint32) {
	i := sort.Search(len(n.nacks), func(i int) bool { return n.nacks[i].sn >= extSN })
	if i < len(n.nacks) && n.nacks[i].sn == extSN {
		return
	}

	nck := nack{
		sn:     extSN,
		nacked: 0,
	}
	if i == len(n.nacks) {
		n.nacks = append(n.nacks, nck)
	} else {
		n.nacks = append(n.nacks[:i+1], n.nacks[i:]...)
		n.nacks[i] = nck
	}

	if len(n.nacks) >= maxNackCache {
		copy(n.nacks, n.nacks[1:])
	}
}

func (n *nackQueue) pairs(headSN uint32) ([]rtcp.NackPair, bool) {
	if len(n.nacks) == 0 {
		return nil, false
	}
	i := 0
	askKF := false
	var np rtcp.NackPair
	var nps []rtcp.NackPair
	lostIdx := -1
	for _, nck := range n.nacks {
		if nck.nacked >= maxNackTimes {
			if nck.sn > n.kfSN {
				n.kfSN = nck.sn
				askKF = true
			}
			continue
		}
		if nck.sn >= headSN-2 {
			n.nacks[i] = nck
			i++
			continue
		}
		n.nacks[i] = nack{
			sn:     nck.sn,
			nacked: nck.nacked + 1,
		}
		i++

		// first nackpair or need a new nackpair
		if lostIdx < 0 || nck.sn > n.nacks[lostIdx].sn+16 {
			if lostIdx >= 0 {
				nps = append(nps, np)
			}
			np.PacketID = uint16(nck.sn)
			np.LostPackets = 0
			lostIdx = i - 1
			continue
		}
		np.LostPackets |= 1 << ((nck.sn) - n.nacks[lostIdx].sn - 1)
	}

	// append last nackpair
	if lostIdx != -1 {
		nps = append(nps, np)
	}
	n.nacks = n.nacks[:i]
	return nps, askKF
}
