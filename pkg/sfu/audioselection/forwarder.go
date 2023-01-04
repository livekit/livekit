package audioselection

import (
	"sort"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	updateInterval = 100 * time.Millisecond
)

// type sfu.Receiver interface {
// 	Activate()
// 	Deactivate()
// 	GetAudioLevel() uint8
// 	ID() livekit.TrackID
// }

func AudioCodecCanbeMux(ti livekit.TrackInfo, codecs []webrtc.RTPCodecParameters) bool {
	if len(codecs) == 0 || ti.Stereo {
		return false
	}

	c := codecs[0]
	return c.MimeType == webrtc.MimeTypeOpus
}

type SelectionForwarderParams struct {
	ActiveDowntracks     int
	FadeDowntracks       int
	ActiveLevelThreshold float64
	FadeoutTime          time.Duration
	FadeinTime           time.Duration
	Logger               logger.Logger
	RequestDownTrack     func(sfu.TrackReceiver) *sfu.DownTrack
}

type sourceInfo struct {
	participantID livekit.ParticipantID
	trackID       livekit.TrackID
	receiver      sfu.TrackReceiver
	vad           bool
	active        bool
	audioLevel    float64
	downtrack     *sfu.DownTrack
}

type SelectionForwarder struct {
	lock           sync.RWMutex
	params         SelectionForwarderParams
	sources        []*sourceInfo
	idleDowntracks []*sfu.DownTrack
	downtracks     []*sfu.DownTrack
	close          chan struct{}

	onForwardMappingChanged func(muxInfo []*livekit.AudioTrackMuxInfo)
}

func NewSelectionForwarder(params SelectionForwarderParams) *SelectionForwarder {
	return &SelectionForwarder{
		params: params,
		close:  make(chan struct{}),
	}
}

func (f *SelectionForwarder) Start() {
	f.lock.Lock()
	for _, dt := range f.downtracks {
		dt.SetConnected()
	}
	f.lock.Unlock()
	go f.process()
}

func (f *SelectionForwarder) Stop() {
	close(f.close)
}

func (f *SelectionForwarder) AddDownTrack(dt *sfu.DownTrack) {
	f.lock.Lock()
	f.downtracks = append(f.downtracks, dt)
	f.idleDowntracks = append(f.idleDowntracks, dt)
	f.lock.Unlock()
}

func (f *SelectionForwarder) RemoveDownTrack(dt *sfu.DownTrack) {
}

// OnForwardMappingChanged is called when the forward mapping is changed, used to update the relationship between downtracks and sources
func (f *SelectionForwarder) OnForwardMappingChanged(h func(muxInfo []*livekit.AudioTrackMuxInfo)) {
	f.onForwardMappingChanged = h
}

func (f *SelectionForwarder) AddSource(participantID livekit.ParticipantID, trackID livekit.TrackID, source sfu.TrackReceiver) {
	f.params.Logger.Debugw("adding source", "trackID", trackID)
	f.lock.Lock()
	f.sources = append(f.sources, &sourceInfo{participantID: participantID, trackID: trackID, receiver: source})
	f.lock.Unlock()
}

func (f *SelectionForwarder) RemoveSource(trackID livekit.TrackID) {
	f.lock.Lock()
	for i, s := range f.sources {
		if s.trackID == trackID {
			if s.active {
				f.deactiveSource(s)
			}
			f.sources[i] = f.sources[len(f.sources)-1]
			f.sources = f.sources[:len(f.sources)-1]
			break
		}
	}
	f.lock.Unlock()
}

func (f *SelectionForwarder) MuteSource(source sfu.TrackReceiver, mute bool) {
}

func (f *SelectionForwarder) process() {
	ticker := time.NewTicker(updateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			f.updateForward()

		case <-f.close:
			return
		}
	}
}

func (f *SelectionForwarder) updateForward() {
	f.lock.Lock()
	defer f.lock.Unlock()
	if len(f.sources) == 0 {
		return
	}

	for _, source := range f.sources {
		source.audioLevel, _ = source.receiver.GetAudioLevel()
	}

	sort.Slice(f.sources, func(i, j int) bool {
		return f.sources[i].audioLevel > f.sources[j].audioLevel
	})

	var activeteSources, idleSources []*sourceInfo
	for i, source := range f.sources {
		if i >= f.params.ActiveDowntracks && source.active {
			idleSources = append(idleSources, source)
		} else {
			if source.audioLevel > f.params.ActiveLevelThreshold && !source.active {
				activeteSources = append(activeteSources, source)
			} else if source.audioLevel <= f.params.ActiveLevelThreshold && source.active {
				idleSources = append(idleSources, source)
			}
		}
	}

	var forwardChanged bool
	for _, source := range activeteSources {
		if len(f.idleDowntracks) == 0 && len(idleSources) > 0 {
			f.deactiveSource(idleSources[0])
			idleSources = idleSources[1:]
		}
		if f.activeSource(source) {
			forwardChanged = true
		}
	}

	if forwardChanged && f.onForwardMappingChanged != nil {
		muxInfo := make([]*livekit.AudioTrackMuxInfo, 0, len(f.sources))

		for _, source := range f.sources {
			if source.active && source.downtrack != nil {
				muxInfo = append(muxInfo, &livekit.AudioTrackMuxInfo{
					SdpTrackId:     source.downtrack.ID(),
					ParticipantSid: string(source.participantID),
					TrackSid:       string(source.trackID),
				})
			}
		}
		f.onForwardMappingChanged(muxInfo)
	}
}

func (f *SelectionForwarder) activeSource(source *sourceInfo) bool {
	if len(f.idleDowntracks) == 0 {
		if len(f.downtracks) < f.params.ActiveDowntracks {
			dt := f.params.RequestDownTrack(source.receiver)
			f.downtracks = append(f.downtracks, dt)
			f.idleDowntracks = append(f.idleDowntracks, dt)
		} else {
			f.params.Logger.Warnw("no idle downtracks for active source", nil, "trackID", source.receiver.TrackID())
			return false
		}
	}
	source.active = true
	source.downtrack = f.idleDowntracks[0]
	f.idleDowntracks = f.idleDowntracks[1:]
	source.downtrack.ResetReceiver(source.receiver)
	source.receiver.AddDownTrack(source.downtrack)
	f.params.Logger.Debugw("activating source", "trackID", source.receiver.TrackID(), "downtrack", source.downtrack.ID())
	return true
}

func (f *SelectionForwarder) deactiveSource(source *sourceInfo) {
	f.params.Logger.Debugw("deactivate source", "trackID", source.receiver.TrackID())
	source.active = false
	dt := source.downtrack
	source.downtrack = nil
	if dt != nil {
		dt.ResetReceiver(&NullReceiver{})
		source.receiver.DeleteDownTrack(dt.SubscriberID())
		f.idleDowntracks = append(f.idleDowntracks, dt)
	}
}
