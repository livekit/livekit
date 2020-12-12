package sfu

import (
	"container/list"
	"time"
)

const (
	ignoreRetransmission = 5e8 // Ignore packet retransmission after ignoreRetransmission milliseconds
	maxNackQueue         = 100
)

type NACK struct {
	SN  uint16
	LRX int64
}

type nackList struct {
	nacks map[uint16]*list.Element
	ll    *list.List
}

func newNACKList() *nackList {
	return &nackList{
		nacks: make(map[uint16]*list.Element),
		ll:    list.New(),
	}
}

func (n *nackList) getNACKSeqNo(seqno []uint16) []uint16 {
	packets := make([]uint16, 0, 17)
	for _, sn := range seqno {
		if nack, ok := n.nacks[sn]; !ok {
			n.nacks[sn] = n.ll.PushBack(NACK{sn, time.Now().UnixNano()})
			packets = append(packets, sn)
		} else if time.Now().UnixNano()-nack.Value.(NACK).LRX > ignoreRetransmission {
			nack.Value = NACK{sn, time.Now().UnixNano()}
			packets = append(packets, sn)
		}
	}

	for len(n.nacks) > maxNackQueue {
		el := n.ll.Back()
		delete(n.nacks, el.Value.(NACK).SN)
		n.ll.Remove(el)
	}

	return packets
}
