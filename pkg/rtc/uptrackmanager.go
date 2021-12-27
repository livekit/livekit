package rtc

import (
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

type UptrackManagerParams struct {
	Identity       string
	SID            string
	Config         *WebRTCConfig
	AudioConfig    config.AudioConfig
	Telemetry      telemetry.TelemetryService
	ThrottleConfig config.PLIThrottleConfig
	Logger         logger.Logger
}

type UptrackManager struct {
	params      UptrackManagerParams
	rtcpCh      chan []rtcp.Packet
	pliThrottle *pliThrottle

	closed bool

	// hold reference for MediaTrack
	twcc *twcc.Responder

	// publishedTracks that participant is publishing
	publishedTracks map[livekit.TrackSid]types.PublishedTrack
	// client intended to publish, yet to be reconciled
	pendingTracks map[string]*livekit.TrackInfo
	// keeps track of subscriptions that are awaiting permissions
	subscriptionPermissions map[string]*livekit.TrackPermission // subscriberID => *livekit.TrackPermission
	// keeps tracks of track specific subscribers who are awaiting permission
	pendingSubscriptions map[livekit.TrackSid][]string // trackSid => []subscriberID

	lock sync.RWMutex

	// callbacks & handlers
	onTrackPublished             func(track types.PublishedTrack)
	onTrackUpdated               func(track types.PublishedTrack, onlyIfReady bool)
	onWriteRTCP                  func(pkts []rtcp.Packet)
	onSubscribedMaxQualityChange func(trackSid string, subscribedQualities []*livekit.SubscribedQuality) error
}

func NewUptrackManager(params UptrackManagerParams) *UptrackManager {
	return &UptrackManager{
		params:               params,
		rtcpCh:               make(chan []rtcp.Packet, 50),
		pliThrottle:          newPLIThrottle(params.ThrottleConfig),
		publishedTracks:      make(map[livekit.TrackSid]types.PublishedTrack, 0),
		pendingTracks:        make(map[string]*livekit.TrackInfo),
		pendingSubscriptions: make(map[livekit.TrackSid][]string),
	}
}

func (u *UptrackManager) Start() {
	go u.rtcpSendWorker()
}

func (u *UptrackManager) Close() {
	u.lock.Lock()
	defer u.lock.Unlock()

	u.closed = true

	// remove all subscribers
	for _, t := range u.publishedTracks {
		t.RemoveAllSubscribers()
	}

	if len(u.publishedTracks) == 0 {
		close(u.rtcpCh)
	}
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

func (u *UptrackManager) OnTrackPublished(f func(track types.PublishedTrack)) {
	u.onTrackPublished = f
}

func (u *UptrackManager) OnTrackUpdated(f func(track types.PublishedTrack, onlyIfReady bool)) {
	u.onTrackUpdated = f
}

func (u *UptrackManager) OnWriteRTCP(f func(pkts []rtcp.Packet)) {
	u.onWriteRTCP = f
}

func (u *UptrackManager) OnSubscribedMaxQualityChange(f func(trackSid livekit.TrackSid, subscribedQualities []*livekit.SubscribedQuality) error) {
	u.onSubscribedMaxQualityChange = f
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
func (u *UptrackManager) AddSubscriber(sub types.Participant, params types.AddSubscriberParams) (int, error) {
	var tracks []types.PublishedTrack
	if params.AllTracks {
		tracks = u.GetPublishedTracks()
	} else {
		for _, trackSid := range params.TrackSids {
			track := u.getPublishedTrack(trackSid)
			if track == nil {
				continue
			}

			tracks = append(tracks, track)
		}
	}
	if len(tracks) == 0 {
		return 0, nil
	}

	u.params.Logger.Debugw("subscribing new participant to tracks",
		"subscriber", sub.Identity(),
		"subscriberID", sub.ID(),
		"numTracks", len(tracks))

	n := 0
	for _, track := range tracks {
		trackSid := track.ID()
		subscriberID := sub.ID()
		if !u.hasPermission(trackSid, subscriberID) {
			u.maybeAddPendingSubscription(trackSid, sub)
			continue
		}

		if err := track.AddSubscriber(sub); err != nil {
			return n, err
		}
		n += 1
	}
	return n, nil
}

func (u *UptrackManager) RemoveSubscriber(sub types.Participant, trackSid livekit.TrackSid) {
	u.lock.Lock()
	defer u.lock.Unlock()

	track := u.getPublishedTrack(trackSid)
	if track != nil {
		track.RemoveSubscriber(sub.ID())
	}

	u.maybeRemovePendingSubscription(trackSid, sub)
}

func (u *UptrackManager) SetTrackMuted(trackSid livekit.TrackSid, muted bool) {
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
	currentMuted := track.IsMuted()
	track.SetMuted(muted)

	if currentMuted != track.IsMuted() && u.onTrackUpdated != nil {
		u.params.Logger.Debugw("mute status changed",
			"track", trackSid,
			"muted", track.IsMuted())
		u.onTrackUpdated(track, false)
	}
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

func (u *UptrackManager) GetPublishedTrack(sid livekit.TrackSid) types.PublishedTrack {
	u.lock.RLock()
	defer u.lock.RUnlock()

	return u.getPublishedTrack(sid)
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

func (u *UptrackManager) UpdateSubscriptionPermissions(
	permissions *livekit.UpdateSubscriptionPermissions,
	resolver func(participantSid string) types.Participant,
) error {
	u.lock.Lock()
	defer u.lock.Unlock()

	u.updateSubscriptionPermissions(permissions)

	u.processPendingSubscriptions(resolver)

	u.maybeRevokeSubscriptions(resolver)

	return nil
}

// when a new remoteTrack is created, creates a Track and adds it to room
func (u *UptrackManager) MediaTrackReceived(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
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
			TrackInfo:           ti,
			SignalCid:           signalCid,
			SdpCid:              track.ID(),
			ParticipantID:       u.params.SID,
			ParticipantIdentity: u.params.Identity,
			RTCPChan:            u.rtcpCh,
			BufferFactory:       u.params.Config.BufferFactory,
			ReceiverConfig:      u.params.Config.Receiver,
			AudioConfig:         u.params.AudioConfig,
			Telemetry:           u.params.Telemetry,
			Logger:              u.params.Logger,
		})
		mt.OnSubscribedMaxQualityChange(u.onSubscribedMaxQualityChange)

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
			if u.onWriteRTCP != nil {
				u.onWriteRTCP([]rtcp.Packet{&pkt})
			}
		})
	}
	u.lock.Unlock()

	mt.AddReceiver(rtpReceiver, track, u.twcc)

	if newTrack {
		u.handleTrackPublished(mt)
	}
}

// should be called with lock held
func (u *UptrackManager) getPublishedTrack(sid livekit.TrackSid) types.PublishedTrack {
	return u.publishedTracks[sid]
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

	if trackInfo == nil {
		//
		// If no match on client id, find first one matching type
		// as MediaStreamTrack can change client id when transceiver
		// is added to peer connection.
		//
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
		trackSid := track.ID()
		delete(u.publishedTracks, trackSid)
		delete(u.pendingSubscriptions, trackSid)
		// not modifying subscription permissions, will get reset on next update from participant

		// as rtcpCh handles RTCP for all published tracks, close only after all published tracks are closed
		if u.closed && len(u.publishedTracks) == 0 {
			close(u.rtcpCh)
		}
		u.lock.Unlock()
		// only send this when client is in a ready state
		if u.onTrackUpdated != nil {
			u.onTrackUpdated(track, true)
		}
	})

	if u.onTrackPublished != nil {
		u.onTrackPublished(track)
	}
}

func (u *UptrackManager) updateSubscriptionPermissions(permissions *livekit.UpdateSubscriptionPermissions) {
	// every update overrides the existing

	// all_participants takes precedence
	if permissions.AllParticipants {
		// everything is allowed, nothing else to do
		u.subscriptionPermissions = nil
		return
	}

	// per participant permissions
	u.subscriptionPermissions = make(map[string]*livekit.TrackPermission)
	for _, trackPerms := range permissions.TrackPermissions {
		u.subscriptionPermissions[trackPerms.ParticipantSid] = trackPerms
	}
}

func (u *UptrackManager) hasPermission(trackSid livekit.TrackSid, subscriberID string) bool {
	if u.subscriptionPermissions == nil {
		return true
	}

	perms, ok := u.subscriptionPermissions[subscriberID]
	if !ok {
		return false
	}

	if perms.AllTracks {
		return true
	}

	for _, sid := range perms.TrackSids {
		if sid == trackSid {
			return true
		}
	}

	return false
}

func (u *UptrackManager) getAllowedSubscribers(trackSid livekit.TrackSid) []string {
	if u.subscriptionPermissions == nil {
		return nil
	}

	allowed := []string{}
	for subscriberID, perms := range u.subscriptionPermissions {
		if perms.AllTracks {
			allowed = append(allowed, subscriberID)
			continue
		}

		for _, sid := range perms.TrackSids {
			if sid == trackSid {
				allowed = append(allowed, subscriberID)
				break
			}
		}
	}

	return allowed
}

func (u *UptrackManager) maybeAddPendingSubscription(trackSid livekit.TrackSid, sub types.Participant) {
	subscriberID := sub.ID()

	pending := u.pendingSubscriptions[trackSid]
	for _, sid := range pending {
		if sid == subscriberID {
			// already pending
			return
		}
	}

	u.pendingSubscriptions[trackSid] = append(u.pendingSubscriptions[trackSid], subscriberID)
	go sub.SubscriptionPermissionUpdate(u.params.SID, trackSid, false)
}

func (u *UptrackManager) maybeRemovePendingSubscription(trackSid livekit.TrackSid, sub types.Participant) {
	subscriberID := sub.ID()

	pending := u.pendingSubscriptions[trackSid]
	n := len(pending)
	for idx, sid := range pending {
		if sid == subscriberID {
			u.pendingSubscriptions[trackSid][idx] = u.pendingSubscriptions[trackSid][n-1]
			u.pendingSubscriptions[trackSid] = u.pendingSubscriptions[trackSid][:n-1]
			break
		}
	}
	if len(u.pendingSubscriptions[trackSid]) == 0 {
		delete(u.pendingSubscriptions, trackSid)
	}
}

func (u *UptrackManager) processPendingSubscriptions(resolver func(participantSid string) types.Participant) {
	updatedPendingSubscriptions := make(map[string][]string)
	for trackSid, pending := range u.pendingSubscriptions {
		track := u.getPublishedTrack(trackSid)
		if track == nil {
			continue
		}

		var updatedPending []string
		for _, sid := range pending {
			var sub types.Participant
			if resolver != nil {
				sub = resolver(sid)
			}
			if sub == nil || sub.State() == livekit.ParticipantInfo_DISCONNECTED {
				// do not keep this pending subscription as subscriber may be gone
				continue
			}

			if !u.hasPermission(trackSid, sid) {
				updatedPending = append(updatedPending, sid)
				continue
			}

			if err := track.AddSubscriber(sub); err != nil {
				u.params.Logger.Errorw("error reinstating pending subscription", err)
				// keep it in pending on error in case the error is transient
				updatedPending = append(updatedPending, sid)
				continue
			}

			go sub.SubscriptionPermissionUpdate(u.params.SID, trackSid, true)
		}

		updatedPendingSubscriptions[trackSid] = updatedPending
	}

	u.pendingSubscriptions = updatedPendingSubscriptions
}

func (u *UptrackManager) maybeRevokeSubscriptions(resolver func(participantSid string) types.Participant) {
	for _, track := range u.publishedTracks {
		trackSid := track.ID()
		allowed := u.getAllowedSubscribers(trackSid)
		if allowed == nil {
			// no restrictions
			continue
		}

		revokedSubscribers := track.RevokeDisallowedSubscribers(allowed)
		for _, subID := range revokedSubscribers {
			var sub types.Participant
			if resolver != nil {
				sub = resolver(subID)
			}
			if sub == nil {
				continue
			}

			u.maybeAddPendingSubscription(trackSid, sub)
		}
	}
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

		if len(fwdPkts) > 0 && u.onWriteRTCP != nil {
			u.onWriteRTCP(fwdPkts)
		}
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
