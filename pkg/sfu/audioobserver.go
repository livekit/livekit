package sfu

import (
	"sort"
	"sync"
)

type audioStream struct {
	id    string
	sum   int
	total int
}

type AudioObserver struct {
	sync.RWMutex
	streams   []*audioStream
	expected  int
	threshold uint8
	previous  []string
}

func NewAudioObserver(threshold uint8, interval, filter int) *AudioObserver {
	if threshold > 127 {
		threshold = 127
	}
	if filter < 0 {
		filter = 0
	}
	if filter > 100 {
		filter = 100
	}

	return &AudioObserver{
		threshold: threshold,
		expected:  interval * filter / 2000,
	}
}

func (a *AudioObserver) addStream(streamID string) {
	a.Lock()
	a.streams = append(a.streams, &audioStream{id: streamID})
	a.Unlock()
}

func (a *AudioObserver) removeStream(streamID string) {
	a.Lock()
	defer a.Unlock()
	idx := -1
	for i, s := range a.streams {
		if s.id == streamID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return
	}
	a.streams[idx] = a.streams[len(a.streams)-1]
	a.streams[len(a.streams)-1] = nil
	a.streams = a.streams[:len(a.streams)-1]
}

func (a *AudioObserver) observe(streamID string, dBov uint8) {
	a.RLock()
	defer a.RUnlock()
	for _, as := range a.streams {
		if as.id == streamID {
			if dBov <= a.threshold {
				as.sum += int(dBov)
				as.total++
			}
			return
		}
	}
}

func (a *AudioObserver) Calc() []string {
	a.Lock()
	defer a.Unlock()

	sort.Slice(a.streams, func(i, j int) bool {
		si, sj := a.streams[i], a.streams[j]
		switch {
		case si.total != sj.total:
			return si.total > sj.total
		default:
			return si.sum < sj.sum
		}
	})

	streamIDs := make([]string, 0, len(a.streams))
	for _, s := range a.streams {
		if s.total >= a.expected {
			streamIDs = append(streamIDs, s.id)
		}
		s.total = 0
		s.sum = 0
	}

	if len(a.previous) == len(streamIDs) {
		for i, s := range a.previous {
			if s != streamIDs[i] {
				a.previous = streamIDs
				return streamIDs
			}
		}
		return nil
	}

	a.previous = streamIDs
	return streamIDs
}
