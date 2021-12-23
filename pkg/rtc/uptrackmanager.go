package rtc

import (
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu/twcc"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

type UptrackManagerParams struct {
	Identity        string
	SID             string
	Config          *WebRTCConfig
	Sink            routing.MessageSink
	AudioConfig     config.AudioConfig
	ProtocolVersion types.ProtocolVersion
	Telemetry       telemetry.TelemetryService
	ThrottleConfig  config.PLIThrottleConfig
	EnabledCodecs   []*livekit.Codec
	Hidden          bool
	Recorder        bool
	Logger          logger.Logger
}

type UptrackManager struct {
	params      UptrackManagerParams
	rtcpCh      chan []rtcp.Packet
	pliThrottle *pliThrottle

	// hold reference for MediaTrack
	twcc *twcc.Responder

	// publishedTracks that participant is publishing
	publishedTracks map[string]types.PublishedTrack
	// client intended to publish, yet to be reconciled
	pendingTracks map[string]*livekit.TrackInfo
	// keeps track of subscriptions that are awaiting permissions
	subscriptionPermissions map[string][]string
	// keeps tracks of track specific subscribers who are awaiting permission
	pendingSubscriptions map[string][]string

	lock sync.RWMutex
}

func NewUptrackManager(params UptrackManagerParams) *UptrackManager {
	return &UptrackManager{
		params:                  params,
		rtcpCh:                  make(chan []rtcp.Packet, 50),
		pliThrottle:             newPLIThrottle(params.ThrottleConfig),
		publishedTracks:         make(map[string]types.PublishedTrack, 0),
		pendingTracks:           make(map[string]*livekit.TrackInfo),
		subscriptionPermissions: make(map[string][]string),
		pendingSubscriptions:    make(map[string][]string),
	}
}

func (u *UptrackManager) Start() {
	go u.rtcpSendWorker()
}

func (u *UptrackManager) Close() {
	u.lock.Lock()
	defer u.lock.Unlock()

	// remove all subscribers
	for _, t := range u.publishedTracks {
		// skip updates
		t.RemoveAllSubscribers()
	}

	close(u.rtcpCh)
}

func (u *UptrackManager) ToProto() []*livekit.TrackInfo {
	u.lock.RLock()
	defer u.lock.RUnlock()

	var trackInfos []*livekit.TrackInfo
	for _, t := range u.publishedTracks {
		trackInfos = append(trackInfos, t.ToProto())
	}

	return trackInfos
}

// AddTrack is called when client intends to publish track.
// records track details and lets client know it's ok to proceed
func (u *UptrackManager) AddTrack(req *livekit.AddTrackRequest) *livekit.TrackInfo {
	u.lock.Lock()
	defer u.lock.Unlock()

	// if track is already published, reject
	if u.pendingTracks[req.Cid] != nil {
		return nil
	}

	if u.getPublishedTrackBySignalCid(req.Cid) != nil || u.getPublishedTrackBySdpCid(req.Cid) != nil {
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
	u.pendingTracks[req.Cid] = ti

	return ti
}

// AddSubscriber subscribes op to all publishedTracks
func (u *UptrackManager) AddSubscriber(sub types.Participant) (int, error) {
	tracks := u.GetPublishedTracks()
	if len(tracks) == 0 {
		return 0, nil
	}

	u.params.Logger.Debugw("subscribing new participant to tracks",
		"subscriber", sub.Identity(),
		"subscriberID", sub.ID(),
		"numTracks", len(tracks))
	// RAJA-TODO: check permissions and add to pending if necessary

	n := 0
	for _, track := range tracks {
		if err := track.AddSubscriber(sub); err != nil {
			return n, err
		}
		if track.IsSubscriber(sub.ID()) {
			n += 1
		}
	}
	return n, nil
}

func (u *UptrackManager) SetTrackMuted(trackSid string, muted bool, fromAdmin bool) {
	isPending := false
	u.lock.RLock()
	for _, ti := range u.pendingTracks {
		if ti.Sid == trackSid {
			ti.Muted = muted
			isPending = true
		}
	}
	track := u.publishedTracks[trackSid]
	u.lock.RUnlock()

	if track == nil {
		if !isPending {
			u.params.Logger.Warnw("could not locate track", nil, "track", trackSid)
		}
		return
	}
	// RAJA-TODO currentMuted := track.IsMuted()
	track.SetMuted(muted)

	/* RAJA_TODO
	// when request is coming from admin, send message to current participant
	if fromAdmin {
		_ = p.writeMessage(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Mute{
				Mute: &livekit.MuteTrackRequest{
					Sid:   trackSid,
					Muted: muted,
				},
			},
		})
	}

	if currentMuted != track.IsMuted() && p.onTrackUpdated != nil {
		u.params.Logger.Debugw("mute status changed",
			"track", trackSid,
			"muted", track.IsMuted())
		p.onTrackUpdated(p, track)
	}
	*/
}

func (u *UptrackManager) GetAudioLevel() (level uint8, active bool) {
	u.lock.RLock()
	defer u.lock.RUnlock()

	level = silentAudioLevel
	for _, pt := range u.publishedTracks {
		if mt, ok := pt.(*MediaTrack); ok {
			if mt.audioLevel == nil {
				continue
			}
			tl, ta := mt.audioLevel.GetLevel()
			if ta {
				active = true
				if tl < level {
					level = tl
				}
			}
		}
	}
	return
}

func (u *UptrackManager) GetConnectionQuality() (scores float64, numTracks int) {
	u.lock.RLock()
	defer u.lock.RUnlock()

	for _, pt := range u.publishedTracks {
		if pt.IsMuted() {
			continue
		}
		scores += pt.GetConnectionScore()
		numTracks++
	}
	return
}

func (u *UptrackManager) GetPublishedTrack(sid string) types.PublishedTrack {
	u.lock.RLock()
	defer u.lock.RUnlock()

	return u.publishedTracks[sid]
}

func (u *UptrackManager) GetPublishedTracks() []types.PublishedTrack {
	u.lock.RLock()
	defer u.lock.RUnlock()

	tracks := make([]types.PublishedTrack, 0, len(u.publishedTracks))
	for _, t := range u.publishedTracks {
		tracks = append(tracks, t)
	}
	return tracks
}

func (u *UptrackManager) UpdateSubscriptionPermissions(
	permissions *livekit.UpdateSubscriptionPermissions,
	resolver func(participantSid string) types.Participant,
) {
	u.lock.Lock()
	defer u.lock.Unlock()

	// every update overrides the existing
	u.subscriptionPermissions = make(map[string][]string)

	// all_participants takes precedence
	if permissions.AllParticipants {
		// everything is allowed, nothing else to do
		return
	}

	// if track_permissions is empty, nobody is allowed to subscribe to any tracks
	if permissions.TrackPermissions != nil && len(permissions.TrackPermissions) == 0 {
		for trackSid, _ := range u.publishedTracks {
			u.subscriptionPermissions[trackSid] = nil
		}

		for _, ti := range u.pendingTracks {
			u.subscriptionPermissions[ti.Sid] = nil
		}

		return
	}

	// per participant permissions
	for _, trackPerms := range permissions.TrackPermissions {
		// all_tracks get precedence
		if trackPerms.AllTracks {
			// no restriction for participant, nothing else to do
			for trackSid, _ := range u.publishedTracks {
				u.subscriptionPermissions[trackSid] = append(u.subscriptionPermissions[trackSid], trackPerms.ParticipantSid)
			}

			for _, ti := range u.pendingTracks {
				u.subscriptionPermissions[ti.Sid] = append(u.subscriptionPermissions[ti.Sid], trackPerms.ParticipantSid)
			}
		} else {
			for _, trackSid := range trackPerms.TrackSids {
				u.subscriptionPermissions[trackSid] = append(u.subscriptionPermissions[trackSid], trackPerms.ParticipantSid)
			}
		}
	}
}

// when a new remoteTrack is created, creates a Track and adds it to room
func (u *UptrackManager) onMediaTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	var newTrack bool

	// use existing mediatrack to handle simulcast
	u.lock.Lock()
	mt, ok := u.getPublishedTrackBySdpCid(track.ID()).(*MediaTrack)
	if !ok {
		signalCid, ti := u.getPendingTrack(track.ID(), ToProtoTrackKind(track.Kind()))
		if ti == nil {
			u.lock.Unlock()
			return
		}

		mt = NewMediaTrack(track, MediaTrackParams{
			TrackInfo: ti,
			SignalCid: signalCid,
			SdpCid:    track.ID(),
			// RAJA-TODO ParticipantID:       p.params.SID,
			// RAJA-TODO ParticipantIdentity: p.Identity(),
			RTCPChan:       u.rtcpCh,
			BufferFactory:  u.params.Config.BufferFactory,
			ReceiverConfig: u.params.Config.Receiver,
			AudioConfig:    u.params.AudioConfig,
			Telemetry:      u.params.Telemetry,
			Logger:         u.params.Logger,
		})
		// RAJA-TODO mt.OnSubscribedMaxQualityChange(p.onSubscribedMaxQualityChange)

		// add to published and clean up pending
		u.publishedTracks[mt.ID()] = mt
		delete(u.pendingTracks, signalCid)

		newTrack = true
	}

	ssrc := uint32(track.SSRC())
	u.pliThrottle.addTrack(ssrc, track.RID())
	if u.twcc == nil {
		u.twcc = twcc.NewTransportWideCCResponder(ssrc)
		u.twcc.OnFeedback(func(pkt rtcp.RawPacket) {
			// RAJA-TODO _ = p.publisher.pc.WriteRTCP([]rtcp.Packet{&pkt})
		})
	}
	u.lock.Unlock()

	mt.AddReceiver(rtpReceiver, track, u.twcc)

	if newTrack {
		u.handleTrackPublished(mt)
	}
}

// should be called with lock held
func (u *UptrackManager) getPublishedTrackBySignalCid(clientId string) types.PublishedTrack {
	for _, publishedTrack := range u.publishedTracks {
		if publishedTrack.SignalCid() == clientId {
			return publishedTrack
		}
	}

	return nil
}

// should be called with lock held
func (u *UptrackManager) getPublishedTrackBySdpCid(clientId string) types.PublishedTrack {
	for _, publishedTrack := range u.publishedTracks {
		if publishedTrack.SdpCid() == clientId {
			return publishedTrack
		}
	}

	return nil
}

// should be called with lock held
func (u *UptrackManager) getPendingTrack(clientId string, kind livekit.TrackType) (string, *livekit.TrackInfo) {
	signalCid := clientId
	trackInfo := u.pendingTracks[clientId]

	// then find the first one that matches type. with MediaStreamTrack, it's possible for the client id to
	// change after being added to SubscriberPC
	if trackInfo == nil {
		for cid, ti := range u.pendingTracks {
			if ti.Type == kind {
				trackInfo = ti
				signalCid = cid
				break
			}
		}
	}

	// if still not found, we are done
	if trackInfo == nil {
		u.params.Logger.Errorw("track info not published prior to track", nil, "clientId", clientId)
	}
	return signalCid, trackInfo
}

func (u *UptrackManager) handleTrackPublished(track types.PublishedTrack) {
	u.lock.Lock()
	if _, ok := u.publishedTracks[track.ID()]; !ok {
		u.publishedTracks[track.ID()] = track
	}
	u.lock.Unlock()

	track.AddOnClose(func() {
		// cleanup
		u.lock.Lock()
		delete(u.publishedTracks, track.ID())
		u.lock.Unlock()
		// only send this when client is in a ready state
		/* RAJA-TODO
		if p.IsReady() && p.onTrackUpdated != nil {
			p.onTrackUpdated(p, track)
		}
		RAJA-TODO*/
	})

	/* RAJA-TODO
	if u.onTrackPublished != nil {
		u.onTrackPublished(p, track)
	}
	RAJA-TODO */
}

func (u *UptrackManager) GetDTX() bool {
	u.lock.RLock()
	defer u.lock.RUnlock()

	var trackInfo *livekit.TrackInfo
	for _, ti := range u.pendingTracks {
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

func (u *UptrackManager) rtcpSendWorker() {
	defer Recover()

	// read from rtcpChan
	for pkts := range u.rtcpCh {
		if pkts == nil {
			return
		}

		fwdPkts := make([]rtcp.Packet, 0, len(pkts))
		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication:
				mediaSSRC := pkt.(*rtcp.PictureLossIndication).MediaSSRC
				if u.pliThrottle.canSend(mediaSSRC) {
					fwdPkts = append(fwdPkts, pkt)
				}
			case *rtcp.FullIntraRequest:
				mediaSSRC := pkt.(*rtcp.FullIntraRequest).MediaSSRC
				if u.pliThrottle.canSend(mediaSSRC) {
					fwdPkts = append(fwdPkts, pkt)
				}
			default:
				fwdPkts = append(fwdPkts, pkt)
			}
		}

		/* RAJA-TODO
		if len(fwdPkts) > 0 {
			if err := p.publisher.pc.WriteRTCP(fwdPkts); err != nil {
				p.params.Logger.Errorw("could not write RTCP to participant", err)
			}
		}
		*/
	}
}

func (u *UptrackManager) DebugInfo() map[string]interface{} {
	info := map[string]interface{}{}
	publishedTrackInfo := make(map[string]interface{})
	pendingTrackInfo := make(map[string]interface{})

	u.lock.RLock()
	for trackSid, track := range u.publishedTracks {
		if mt, ok := track.(*MediaTrack); ok {
			publishedTrackInfo[trackSid] = mt.DebugInfo()
		} else {
			publishedTrackInfo[trackSid] = map[string]interface{}{
				"ID":       track.ID(),
				"Kind":     track.Kind().String(),
				"PubMuted": track.IsMuted(),
			}
		}
	}

	for clientID, ti := range u.pendingTracks {
		pendingTrackInfo[clientID] = map[string]interface{}{
			"Sid":       ti.Sid,
			"Type":      ti.Type.String(),
			"Simulcast": ti.Simulcast,
		}
	}
	u.lock.RUnlock()

	info["PublishedTracks"] = publishedTrackInfo
	info["PendingTracks"] = pendingTrackInfo

	return info
}
