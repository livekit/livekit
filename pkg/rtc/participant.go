package rtc

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	placeholderDataChannel = "_private"
	sdBatchSize            = 20
	negotiationFrequency   = 100 * time.Millisecond
)

type ParticipantImpl struct {
	id               string
	peerConn         types.PeerConnection
	sigConn          types.SignalConnection
	ctx              context.Context
	cancel           context.CancelFunc
	mediaEngine      *webrtc.MediaEngine
	name             string
	state            livekit.ParticipantInfo_State
	rtcpCh           chan []rtcp.Packet
	subscribedTracks map[string][]*sfu.DownTrack
	// publishedTracks that participant is publishing
	publishedTracks map[string]types.PublishedTrack
	// client intended to publish, yet to be reconciled
	pendingTracks      map[string]*livekit.TrackInfo
	debouncedNegotiate func(func())

	lock sync.RWMutex
	once sync.Once

	// callbacks & handlers
	onTrackPublished func(types.Participant, types.PublishedTrack)
	onTrackUpdated   func(types.Participant, types.PublishedTrack)
	onICECandidate   func(c *webrtc.ICECandidateInit)
	onStateChange    func(p types.Participant, oldState livekit.ParticipantInfo_State)
	onClose          func(types.Participant)
}

func NewPeerConnection(conf *WebRTCConfig) (*webrtc.PeerConnection, error) {
	me := &webrtc.MediaEngine{}
	me.RegisterDefaultCodecs()
	se := conf.SettingEngine
	se.BufferFactory = bufferFactory.GetOrNew

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))
	return api.NewPeerConnection(conf.Configuration)
}

func NewParticipant(pc types.PeerConnection, sc types.SignalConnection, name string) (*ParticipantImpl, error) {
	me := &webrtc.MediaEngine{}
	me.RegisterDefaultCodecs()

	ctx, cancel := context.WithCancel(context.Background())
	participant := &ParticipantImpl{
		id:                 utils.NewGuid(utils.ParticipantPrefix),
		name:               name,
		peerConn:           pc,
		sigConn:            sc,
		ctx:                ctx,
		cancel:             cancel,
		rtcpCh:             make(chan []rtcp.Packet, 10),
		subscribedTracks:   make(map[string][]*sfu.DownTrack),
		state:              livekit.ParticipantInfo_JOINING,
		lock:               sync.RWMutex{},
		publishedTracks:    make(map[string]types.PublishedTrack, 0),
		pendingTracks:      make(map[string]*livekit.TrackInfo),
		mediaEngine:        me,
		debouncedNegotiate: debounce.New(negotiationFrequency),
	}

	log := logger.GetLogger()
	pc.ConnectionState()
	pc.OnTrack(participant.onMediaTrack)

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}

		ci := c.ToJSON()

		// write candidate
		logger.GetLogger().Debugw("sending ice candidates")
		err := sc.WriteResponse(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Trickle{
				Trickle: ToProtoTrickle(ci),
			},
		})
		if err != nil {
			log.Errorw("could not send trickle", "err", err)
		}

		if participant.onICECandidate != nil {
			participant.onICECandidate(&ci)
		}
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		logger.GetLogger().Debugw("ICE connection state changed", "state", state.String())
		if state == webrtc.ICEConnectionStateConnected {
			participant.updateState(livekit.ParticipantInfo_ACTIVE)
		}
	})

	// TODO: handle data channel
	pc.OnDataChannel(participant.onDataChannel)

	return participant, nil
}

func (p *ParticipantImpl) ID() string {
	return p.id
}

func (p *ParticipantImpl) Name() string {
	return p.name
}

func (p *ParticipantImpl) State() livekit.ParticipantInfo_State {
	return p.state
}

func (p *ParticipantImpl) ToProto() *livekit.ParticipantInfo {
	info := &livekit.ParticipantInfo{
		Sid:   p.id,
		Name:  p.name,
		State: p.state,
	}

	for _, t := range p.publishedTracks {
		info.Tracks = append(info.Tracks, ToProtoTrack(t))
	}
	return info
}

// callbacks for clients
func (p *ParticipantImpl) OnTrackPublished(callback func(types.Participant, types.PublishedTrack)) {
	p.onTrackPublished = callback
}

func (p *ParticipantImpl) OnICECandidate(callback func(c *webrtc.ICECandidateInit)) {
	p.onICECandidate = callback
}

func (p *ParticipantImpl) OnStateChange(callback func(p types.Participant, oldState livekit.ParticipantInfo_State)) {
	p.onStateChange = callback
}

func (p *ParticipantImpl) OnTrackUpdated(callback func(types.Participant, types.PublishedTrack)) {
	p.onTrackUpdated = callback
}

func (p *ParticipantImpl) OnClose(callback func(types.Participant)) {
	p.onClose = callback
}

// Answer an offer from remote participant, used when clients make the initial connection
func (p *ParticipantImpl) Answer(sdp webrtc.SessionDescription) (answer webrtc.SessionDescription, err error) {
	if err = p.peerConn.SetRemoteDescription(sdp); err != nil {
		return
	}

	answer, err = p.peerConn.CreateAnswer(nil)
	if err != nil {
		err = errors.Wrap(err, "could not create answer")
		return
	}

	if err = p.peerConn.SetLocalDescription(answer); err != nil {
		err = errors.Wrap(err, "could not set local description")
		return
	}

	// only set after answered
	p.peerConn.OnNegotiationNeeded(func() {
		logger.GetLogger().Debugw("negotiation needed", "participantId", p.ID())
		p.scheduleNegotiate()
	})

	err = p.sigConn.WriteResponse(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: ToProtoSessionDescription(answer),
		},
	})

	if p.state == livekit.ParticipantInfo_JOINING {
		p.updateState(livekit.ParticipantInfo_JOINED)
	}
	return
}

// client intends to publish track, this function records track details and schedules negotiation
func (p *ParticipantImpl) AddTrack(clientId, name string, trackType livekit.TrackType) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.pendingTracks[clientId] = &livekit.TrackInfo{
		Type: trackType,
		Name: name,
	}

	p.scheduleNegotiate()
}

func (p *ParticipantImpl) RemoveTrack(sid string) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	//

	p.scheduleNegotiate()
	return nil
}

func (p *ParticipantImpl) HandleAnswer(sdp webrtc.SessionDescription) error {
	if sdp.Type != webrtc.SDPTypeAnswer {
		return ErrUnexpectedOffer
	}
	logger.GetLogger().Debugw("setting remote answer")
	if err := p.peerConn.SetRemoteDescription(sdp); err != nil {
		return errors.Wrap(err, "could not set remote description")
	}
	return nil
}

// AddICECandidate adds candidates for remote peer
func (p *ParticipantImpl) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if err := p.peerConn.AddICECandidate(candidate); err != nil {
		return err
	}
	return nil
}

func (p *ParticipantImpl) Start() {
	p.once.Do(func() {
		go p.rtcpSendWorker()
		go p.downTracksRTCPWorker()
	})
}

func (p *ParticipantImpl) Close() error {
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	close(p.rtcpCh)
	p.updateState(livekit.ParticipantInfo_DISCONNECTED)
	if p.onClose != nil {
		p.onClose(p)
	}
	p.cancel()
	return p.peerConn.Close()
}

// Subscribes otherPeer to all of the publishedTracks
func (p *ParticipantImpl) AddSubscriber(op types.Participant) error {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for _, track := range p.publishedTracks {
		logger.GetLogger().Debugw("subscribing to remoteTrack",
			"srcParticipant", p.ID(),
			"dstParticipant", op.ID(),
			"remoteTrack", track.ID())
		if err := track.AddSubscriber(op); err != nil {
			return err
		}
	}
	return nil
}

func (p *ParticipantImpl) RemoveSubscriber(participantId string) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for _, track := range p.publishedTracks {
		track.RemoveSubscriber(participantId)
	}
}

// signal connection methods
func (p *ParticipantImpl) SendJoinResponse(roomInfo *livekit.RoomInfo, otherParticipants []types.Participant) error {
	// send Join response
	return p.sigConn.WriteResponse(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{
			Join: &livekit.JoinResponse{
				Room:              roomInfo,
				Participant:       p.ToProto(),
				OtherParticipants: ToProtoParticipants(otherParticipants),
			},
		},
	})
}

func (p *ParticipantImpl) SendParticipantUpdate(participants []*livekit.ParticipantInfo) error {
	return p.sigConn.WriteResponse(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Update{
			Update: &livekit.ParticipantUpdate{
				Participants: participants,
			},
		},
	})
}

func (p *ParticipantImpl) SetTrackMuted(trackId string, muted bool) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	track := p.publishedTracks[trackId]
	if track == nil {
		return
	}
	updated := false

	switch t := track.(type) {
	case *MediaTrack:
		updated = t.muted != muted
		t.muted = muted
	}

	if updated && p.onTrackUpdated != nil {
		p.onTrackUpdated(p, track)
	}
}

func (p *ParticipantImpl) PeerConnection() types.PeerConnection {
	return p.peerConn
}

func (p *ParticipantImpl) AddDownTrack(streamId string, dt *sfu.DownTrack) {
	p.lock.Lock()
	p.subscribedTracks[streamId] = append(p.subscribedTracks[streamId], dt)
	p.lock.Unlock()
	dt.OnBind(func() {
		go p.scheduleDownTrackBindingReports(streamId)
	})
}

func (p *ParticipantImpl) RemoveDownTrack(streamId string, dt *sfu.DownTrack) {
	p.lock.Lock()
	defer p.lock.Unlock()
	tracks := p.subscribedTracks[streamId]
	newTracks := make([]*sfu.DownTrack, 0, len(tracks))
	for _, track := range tracks {
		if track != dt {
			newTracks = append(newTracks, track)
		}
	}
	p.subscribedTracks[streamId] = newTracks
}

func (p *ParticipantImpl) scheduleNegotiate() {
	p.debouncedNegotiate(p.negotiate)
}

func (p *ParticipantImpl) negotiate() {
	offer, err := p.peerConn.CreateOffer(nil)
	if err != nil {
		logger.GetLogger().Errorw("could not create offer", "err", err)
		return
	}

	if p.peerConn.SignalingState() != webrtc.SignalingStateStable {
		// try this again
		p.scheduleNegotiate()
		return
	}

	err = p.peerConn.SetLocalDescription(offer)
	if err != nil {
		logger.GetLogger().Errorw("could not set local description", "err", err)
		return
	}

	logger.GetLogger().Debugw("sending available offer to participant")
	err = p.sigConn.WriteResponse(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Offer{
			Offer: ToProtoSessionDescription(offer),
		},
	})
	if err != nil {
		logger.GetLogger().Errorw("could not send offer to peer",
			"err", err)
	}
}

func (p *ParticipantImpl) updateState(state livekit.ParticipantInfo_State) {
	if state == p.state {
		return
	}
	oldState := p.state
	p.state = state

	if p.onStateChange != nil {
		go func() {
			p.onStateChange(p, oldState)
		}()
	}
}

// when a new remoteTrack is created, creates a Track and adds it to room
func (p *ParticipantImpl) onMediaTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	logger.GetLogger().Debugw("remoteTrack added", "participantId", p.ID(), "remoteTrack", track.ID())

	// create ReceiverImpl
	receiver := NewReceiver(p.rtcpCh, rtpReceiver, track)
	mt := NewMediaTrack(p.id, p.rtcpCh, track, receiver)

	p.handleTrackPublished(track.ID(), mt)
}

func (p *ParticipantImpl) onDataChannel(dc *webrtc.DataChannel) {
	if dc.Label() == placeholderDataChannel {
		return
	}
	logger.GetLogger().Debugw("dataChannel added", "participantId", p.ID(), "label", dc.Label())

	dt := NewDataTrack(p.id, dc)
	p.lock.Lock()
	p.publishedTracks[dt.id] = dt
	p.lock.Unlock()

	dt.Start()

	cid := fmt.Sprintf("%d", dc.ID())
	p.handleTrackPublished(cid, dt)
}

func (p *ParticipantImpl) handleTrackPublished(clientId string, track types.PublishedTrack) {
	// fill in
	p.lock.Lock()
	defer p.lock.Unlock()
	ti := p.pendingTracks[clientId]
	if ti == nil {
		logger.GetLogger().Errorw("track info not published prior to track", "clientId", clientId)
	} else {
		track.SetName(ti.Name)
		delete(p.pendingTracks, clientId)
	}
	p.publishedTracks[track.ID()] = track

	track.Start()

	// confirm publication
	p.sigConn.WriteResponse(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackPublished{
			TrackPublished: ToProtoTrack(track),
		},
	})
	if p.onTrackPublished != nil {
		go p.onTrackPublished(p, track)
	}
}

func (p *ParticipantImpl) scheduleDownTrackBindingReports(streamId string) {
	var sd []rtcp.SourceDescriptionChunk

	p.lock.RLock()
	dts := p.subscribedTracks[streamId]
	for _, dt := range dts {
		if !dt.IsBound() {
			continue
		}
		chunks := dt.CreateSourceDescriptionChunks()
		if chunks != nil {
			sd = append(sd, chunks...)
		}
	}
	p.lock.RUnlock()

	pkts := []rtcp.Packet{
		&rtcp.SourceDescription{Chunks: sd},
	}

	go func() {
		batch := pkts
		i := 0
		for {
			if err := p.peerConn.WriteRTCP(batch); err != nil {
				logger.GetLogger().Debugw("Sending track binding reports",
					"participant", p.id,
					"err", err)
			}
			if i > 5 {
				return
			}
			i++
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

// downTracksRTCPWorker sends SenderReports periodically when the participant is subscribed to
// other publishedTracks in the room.
func (p *ParticipantImpl) downTracksRTCPWorker() {
	for {
		time.Sleep(5 * time.Second)

		var pkts []rtcp.Packet
		var sd []rtcp.SourceDescriptionChunk
		p.lock.RLock()
		for _, dts := range p.subscribedTracks {
			for _, dt := range dts {
				if !dt.IsBound() {
					continue
				}
				pkts = append(pkts, dt.CreateSenderReport())
				chunks := dt.CreateSourceDescriptionChunks()
				if chunks != nil {
					sd = append(sd, chunks...)
				}
			}
		}
		p.lock.RUnlock()

		// now send in batches of sdBatchSize
		// first batch will contain the sender reports too
		var batch []rtcp.SourceDescriptionChunk
		for len(sd) > 0 {
			size := len(sd)
			if size > sdBatchSize {
				size = sdBatchSize
			}
			batch = sd[:size]
			sd = sd[size:]
			pkts = append(pkts, &rtcp.SourceDescription{Chunks: batch})
			if err := p.peerConn.WriteRTCP(pkts); err != nil {
				if err == io.EOF || err == io.ErrClosedPipe {
					return
				}
				logger.GetLogger().Errorw("could not send downtrack reports",
					"participant", p.id,
					"err", err)
			}
			pkts = pkts[:0]
		}
	}
}

func (p *ParticipantImpl) rtcpSendWorker() {
	// read from rtcpChan
	for pkts := range p.rtcpCh {
		for _, pkt := range pkts {
			logger.GetLogger().Debugw("writing RTCP", "packet", pkt)
		}
		if err := p.peerConn.WriteRTCP(pkts); err != nil {
			logger.GetLogger().Errorw("could not write RTCP to participant",
				"participant", p.id,
				"err", err)
		}
	}
}
