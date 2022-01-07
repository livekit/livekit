package rtc

import (
	"errors"
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu/twcc"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

type LocalParticipantParams struct {
	Identity       livekit.ParticipantIdentity
	SID            livekit.ParticipantID
	Config         *WebRTCConfig
	AudioConfig    config.AudioConfig
	Telemetry      telemetry.TelemetryService
	ThrottleConfig config.PLIThrottleConfig
	Logger         logger.Logger
}

type LocalParticipant struct {
	params      LocalParticipantParams
	rtcpCh      chan []rtcp.Packet
	pliThrottle *pliThrottle

	// hold reference for MediaTrack
	twcc *twcc.Responder

	// client intended to publish, yet to be reconciled
	pendingTracksLock sync.RWMutex
	pendingTracks     map[string]*livekit.TrackInfo

	*UptrackManager

	// callbacks & handlers
	onWriteRTCP      func(pkts []rtcp.Packet)
	onTrackPublished func(track types.PublishedTrack)
}

func NewLocalParticipant(params LocalParticipantParams) *LocalParticipant {
	l := &LocalParticipant{
		params:        params,
		rtcpCh:        make(chan []rtcp.Packet, 50),
		pliThrottle:   newPLIThrottle(params.ThrottleConfig),
		pendingTracks: make(map[string]*livekit.TrackInfo),
	}

	l.setupUptrackManager()

	return l
}

func (l *LocalParticipant) Start() {
	l.UptrackManager.Start()
	go l.rtcpSendWorker()
}

func (l *LocalParticipant) Close() {
	l.UptrackManager.Close()

	l.pendingTracksLock.Lock()
	l.pendingTracks = make(map[string]*livekit.TrackInfo)
	l.pendingTracksLock.Unlock()
}

func (l *LocalParticipant) OnWriteRTCP(f func(pkts []rtcp.Packet)) {
	l.onWriteRTCP = f
}

func (l *LocalParticipant) OnTrackPublished(f func(track types.PublishedTrack)) {
	l.onTrackPublished = f
}

// AddTrack is called when client intends to publish track.
// records track details and lets client know it's ok to proceed
func (l *LocalParticipant) AddTrack(req *livekit.AddTrackRequest) *livekit.TrackInfo {
	l.pendingTracksLock.Lock()
	defer l.pendingTracksLock.Unlock()

	// if track is already published, reject
	if l.pendingTracks[req.Cid] != nil {
		return nil
	}

	if l.UptrackManager.GetPublishedTrackBySignalCidOrSdpCid(req.Cid) != nil {
		return nil
	}

	ti := &livekit.TrackInfo{
		Type:       req.Type,
		Name:       req.Name,
		Sid:        utils.NewGuid(utils.TrackPrefix),
		Width:      req.Width,
		Height:     req.Height,
		Muted:      req.Muted,
		DisableDtx: req.DisableDtx,
		Source:     req.Source,
		Layers:     req.Layers,
	}
	l.pendingTracks[req.Cid] = ti

	return ti
}

func (l *LocalParticipant) SetTrackMuted(trackID livekit.TrackID, muted bool) {
	track := l.UptrackManager.SetTrackMuted(trackID, muted)
	if track != nil {
		// handled in UptrackManager for a published track, no need to update state of pending track
		return
	}

	isPending := false
	l.pendingTracksLock.RLock()
	for _, ti := range l.pendingTracks {
		if livekit.TrackID(ti.Sid) == trackID {
			ti.Muted = muted
			isPending = true
			break
		}
	}
	l.pendingTracksLock.RUnlock()

	if !isPending {
		l.params.Logger.Warnw("could not locate track", nil, "track", trackID)
	}
}

func (l *LocalParticipant) GetConnectionQuality() (scores float64, numTracks int) {
	for _, pt := range l.UptrackManager.GetPublishedTracks() {
		if pt.IsMuted() {
			continue
		}
		scores += pt.GetConnectionScore()
		numTracks++
	}

	return
}

func (l *LocalParticipant) GetDTX() bool {
	l.pendingTracksLock.RLock()
	defer l.pendingTracksLock.RUnlock()

	var trackInfo *livekit.TrackInfo
	for _, ti := range l.pendingTracks {
		if ti.Type == livekit.TrackType_AUDIO {
			trackInfo = ti
			break
		}
	}

	if trackInfo == nil {
		return false
	}

	return !trackInfo.DisableDtx
}

func (l *LocalParticipant) MediaTrackReceived(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	l.pendingTracksLock.Lock()
	newTrack := false

	// use existing mediatrack to handle simulcast
	mt, ok := l.UptrackManager.GetPublishedTrackBySdpCid(track.ID()).(*MediaTrack)
	if !ok {
		signalCid, ti := l.getPendingTrack(track.ID(), ToProtoTrackKind(track.Kind()))
		if ti == nil {
			l.pendingTracksLock.Unlock()
			return
		}

		ti.MimeType = track.Codec().MimeType

		mt = NewMediaTrack(track, MediaTrackParams{
			TrackInfo:           ti,
			SignalCid:           signalCid,
			SdpCid:              track.ID(),
			ParticipantID:       l.params.SID,
			ParticipantIdentity: l.params.Identity,
			RTCPChan:            l.rtcpCh,
			BufferFactory:       l.params.Config.BufferFactory,
			ReceiverConfig:      l.params.Config.Receiver,
			AudioConfig:         l.params.AudioConfig,
			Telemetry:           l.params.Telemetry,
			Logger:              l.params.Logger,
			SubscriberConfig:    l.params.Config.Subscriber,
		})

		// add to published and clean up pending
		l.UptrackManager.AddPublishedTrack(mt)
		delete(l.pendingTracks, signalCid)

		newTrack = true
	}

	ssrc := uint32(track.SSRC())
	l.pliThrottle.addTrack(ssrc, track.RID())
	if l.twcc == nil {
		l.twcc = twcc.NewTransportWideCCResponder(ssrc)
		l.twcc.OnFeedback(func(pkt rtcp.RawPacket) {
			if l.onWriteRTCP != nil {
				l.onWriteRTCP([]rtcp.Packet{&pkt})
			}
		})
	}
	l.pendingTracksLock.Unlock()

	mt.AddReceiver(rtpReceiver, track, l.twcc)

	if newTrack {
		l.handleTrackPublished(mt)
	}
}

func (l *LocalParticipant) handleTrackPublished(track types.PublishedTrack) {
	if l.onTrackPublished != nil {
		l.onTrackPublished(track)
	}
}

func (l *LocalParticipant) UpdateSubscribedQuality(nodeID string, trackID livekit.TrackID, maxQuality livekit.VideoQuality) error {
	track := l.UptrackManager.GetPublishedTrack(trackID)
	if track == nil {
		l.params.Logger.Warnw("could not find track", nil, "trackID", trackID)
		return errors.New("could not find track")
	}

	if mt, ok := track.(*MediaTrack); ok {
		mt.NotifySubscriberNodeMaxQuality(nodeID, maxQuality)
	}

	return nil
}

func (l *LocalParticipant) UpdateMediaLoss(nodeID string, trackID livekit.TrackID, fractionalLoss uint32) error {
	track := l.UptrackManager.GetPublishedTrack(trackID)
	if track == nil {
		l.params.Logger.Warnw("could not find track", nil, "trackID", trackID)
		return errors.New("could not find track")
	}

	if mt, ok := track.(*MediaTrack); ok {
		mt.NotifySubscriberNodeMediaLoss(nodeID, uint8(fractionalLoss))
	}

	return nil
}

func (l *LocalParticipant) DebugInfo() map[string]interface{} {
	info := map[string]interface{}{}
	pendingTrackInfo := make(map[string]interface{})

	l.pendingTracksLock.RLock()
	for clientID, ti := range l.pendingTracks {
		pendingTrackInfo[clientID] = map[string]interface{}{
			"Sid":       ti.Sid,
			"Type":      ti.Type.String(),
			"Simulcast": ti.Simulcast,
		}
	}
	l.pendingTracksLock.RUnlock()

	info["PendingTracks"] = pendingTrackInfo
	info["UptrackManager"] = l.UptrackManager.DebugInfo()

	return info
}

func (l *LocalParticipant) setupUptrackManager() {
	l.UptrackManager = NewUptrackManager(UptrackManagerParams{
		SID:    l.params.SID,
		Logger: l.params.Logger,
	})

	l.UptrackManager.OnClose(l.onUptrackManagerClose)
}

func (l *LocalParticipant) onUptrackManagerClose() {
	close(l.rtcpCh)
}

func (l *LocalParticipant) getPendingTrack(clientId string, kind livekit.TrackType) (string, *livekit.TrackInfo) {
	signalCid := clientId
	trackInfo := l.pendingTracks[clientId]

	if trackInfo == nil {
		//
		// If no match on client id, find first one matching type
		// as MediaStreamTrack can change client id when transceiver
		// is added to peer connection.
		//
		for cid, ti := range l.pendingTracks {
			if ti.Type == kind {
				trackInfo = ti
				signalCid = cid
				break
			}
		}
	}

	// if still not found, we are done
	if trackInfo == nil {
		l.params.Logger.Errorw("track info not published prior to track", nil, "clientId", clientId)
	}
	return signalCid, trackInfo
}

func (l *LocalParticipant) rtcpSendWorker() {
	defer Recover()

	// read from rtcpChan
	for pkts := range l.rtcpCh {
		if pkts == nil {
			return
		}

		fwdPkts := make([]rtcp.Packet, 0, len(pkts))
		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication:
				mediaSSRC := pkt.(*rtcp.PictureLossIndication).MediaSSRC
				if l.pliThrottle.canSend(mediaSSRC) {
					fwdPkts = append(fwdPkts, pkt)
				}
			case *rtcp.FullIntraRequest:
				mediaSSRC := pkt.(*rtcp.FullIntraRequest).MediaSSRC
				if l.pliThrottle.canSend(mediaSSRC) {
					fwdPkts = append(fwdPkts, pkt)
				}
			default:
				fwdPkts = append(fwdPkts, pkt)
			}
		}

		if len(fwdPkts) > 0 && l.onWriteRTCP != nil {
			l.onWriteRTCP(fwdPkts)
		}
	}
}
