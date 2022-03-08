package rtc

import (
	"errors"
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

type UpTrackManagerParams struct {
	SID    livekit.ParticipantID
	Logger logger.Logger
}

type UpTrackManager struct {
	params UpTrackManagerParams

	closed bool

	// publishedTracks that participant is publishing
	publishedTracks        map[livekit.TrackID]types.MediaTrack
	subscriptionPermission *livekit.SubscriptionPermission
	// subscriber permission for published tracks
	subscriberPermissions map[livekit.ParticipantID]*livekit.TrackPermission // subscriberID => *livekit.TrackPermission
	// keeps tracks of track specific subscribers who are awaiting permission
	pendingSubscriptions map[livekit.TrackID][]livekit.ParticipantID // trackID => []subscriberID

	lock sync.RWMutex

	// callbacks & handlers
	onClose        func()
	onTrackUpdated func(track types.MediaTrack, onlyIfReady bool)
}

func NewUpTrackManager(params UpTrackManagerParams) *UpTrackManager {
	return &UpTrackManager{
		params:               params,
		publishedTracks:      make(map[livekit.TrackID]types.MediaTrack),
		pendingSubscriptions: make(map[livekit.TrackID][]livekit.ParticipantID),
	}
}

func (u *UpTrackManager) Start() {
}

func (u *UpTrackManager) Close() {
	u.lock.Lock()
	u.closed = true

	// remove all subscribers
	for _, t := range u.publishedTracks {
		t.RemoveAllSubscribers()
	}

	notify := len(u.publishedTracks) == 0
	u.lock.Unlock()

	if notify && u.onClose != nil {
		u.onClose()
	}
}

func (u *UpTrackManager) OnUpTrackManagerClose(f func()) {
	u.onClose = f
}

func (u *UpTrackManager) ToProto() []*livekit.TrackInfo {
	u.lock.RLock()
	defer u.lock.RUnlock()

	var trackInfos []*livekit.TrackInfo
	for _, t := range u.publishedTracks {
		trackInfos = append(trackInfos, t.ToProto())
	}

	return trackInfos
}

func (u *UpTrackManager) OnPublishedTrackUpdated(f func(track types.MediaTrack, onlyIfReady bool)) {
	u.onTrackUpdated = f
}

// AddSubscriber subscribes op to all publishedTracks
func (u *UpTrackManager) AddSubscriber(sub types.LocalParticipant, params types.AddSubscriberParams) (int, error) {
	var tracks []types.MediaTrack
	if params.AllTracks {
		tracks = u.GetPublishedTracks()
	} else {
		u.lock.RLock()
		for _, trackID := range params.TrackIDs {
			track := u.getPublishedTrack(trackID)
			if track == nil {
				continue
			}

			tracks = append(tracks, track)
		}
		u.lock.RUnlock()
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
		trackID := track.ID()
		subscriberID := sub.ID()
		if !u.hasPermission(trackID, subscriberID) {
			u.lock.Lock()
			u.maybeAddPendingSubscription(trackID, sub)
			u.lock.Unlock()
			continue
		}

		if err := track.AddSubscriber(sub); err != nil {
			return n, err
		}
		n += 1
	}
	return n, nil
}

func (u *UpTrackManager) RemoveSubscriber(sub types.LocalParticipant, trackID livekit.TrackID, resume bool) {
	track := u.GetPublishedTrack(trackID)
	if track != nil {
		track.RemoveSubscriber(sub.ID(), resume)
	}

	u.lock.Lock()
	u.maybeRemovePendingSubscription(trackID, sub)
	u.lock.Unlock()
}

func (u *UpTrackManager) SetPublishedTrackMuted(trackID livekit.TrackID, muted bool) types.MediaTrack {
	u.lock.RLock()
	track := u.publishedTracks[trackID]
	u.lock.RUnlock()

	if track != nil {
		currentMuted := track.IsMuted()
		track.SetMuted(muted)

		if currentMuted != track.IsMuted() {
			u.params.Logger.Debugw("mute status changed", "trackID", trackID, "muted", track.IsMuted())
			if u.onTrackUpdated != nil {
				u.onTrackUpdated(track, false)
			}
		}
	}

	return track
}

func (u *UpTrackManager) GetPublishedTrack(trackID livekit.TrackID) types.MediaTrack {
	u.lock.RLock()
	defer u.lock.RUnlock()

	return u.getPublishedTrack(trackID)
}

func (u *UpTrackManager) GetPublishedTracks() []types.MediaTrack {
	u.lock.RLock()
	defer u.lock.RUnlock()

	tracks := make([]types.MediaTrack, 0, len(u.publishedTracks))
	for _, t := range u.publishedTracks {
		tracks = append(tracks, t)
	}
	return tracks
}

func (u *UpTrackManager) UpdateSubscriptionPermission(
	subscriptionPermission *livekit.SubscriptionPermission,
	resolver func(participantID livekit.ParticipantID) types.LocalParticipant,
) error {
	u.lock.Lock()
	defer u.lock.Unlock()

	// store as is for use when migrating
	u.subscriptionPermission = subscriptionPermission
	if subscriptionPermission == nil {
		// possible to get a nil when migrating
		return nil
	}

	u.parseSubscriptionPermissions(subscriptionPermission)

	u.processPendingSubscriptions(resolver)

	u.maybeRevokeSubscriptions(resolver)

	return nil
}

func (u *UpTrackManager) SubscriptionPermission() *livekit.SubscriptionPermission {
	u.lock.RLock()
	defer u.lock.RUnlock()

	return u.subscriptionPermission
}

func (u *UpTrackManager) UpdateVideoLayers(updateVideoLayers *livekit.UpdateVideoLayers) error {
	track := u.GetPublishedTrack(livekit.TrackID(updateVideoLayers.TrackSid))
	if track == nil {
		u.params.Logger.Warnw("could not find track", nil, "trackID", livekit.TrackID(updateVideoLayers.TrackSid))
		return errors.New("could not find published track")
	}

	track.UpdateVideoLayers(updateVideoLayers.Layers)
	if u.onTrackUpdated != nil {
		u.onTrackUpdated(track, false)
	}

	return nil
}

func (u *UpTrackManager) UpdateSubscribedQuality(nodeID livekit.NodeID, trackID livekit.TrackID, maxQuality livekit.VideoQuality) error {
	track := u.GetPublishedTrack(trackID)
	if track == nil {
		u.params.Logger.Warnw("could not find track", nil, "trackID", trackID)
		return errors.New("could not find published track")
	}

	track.NotifySubscriberNodeMaxQuality(nodeID, maxQuality)
	return nil
}

func (u *UpTrackManager) UpdateMediaLoss(nodeID livekit.NodeID, trackID livekit.TrackID, fractionalLoss uint32) error {
	track := u.GetPublishedTrack(trackID)
	if track == nil {
		u.params.Logger.Warnw("could not find track", nil, "trackID", trackID)
		return errors.New("could not find published track")
	}

	track.NotifySubscriberNodeMediaLoss(nodeID, uint8(fractionalLoss))
	return nil
}

func (u *UpTrackManager) AddPublishedTrack(track types.MediaTrack) {
	u.lock.Lock()
	if _, ok := u.publishedTracks[track.ID()]; !ok {
		u.publishedTracks[track.ID()] = track
	}
	u.lock.Unlock()

	track.AddOnClose(func() {
		notifyClose := false

		// cleanup
		u.lock.Lock()
		trackID := track.ID()
		delete(u.publishedTracks, trackID)
		delete(u.pendingSubscriptions, trackID)
		// not modifying subscription permissions, will get reset on next update from participant

		if u.closed && len(u.publishedTracks) == 0 {
			notifyClose = true
		}
		u.lock.Unlock()

		// only send this when client is in a ready state
		if u.onTrackUpdated != nil {
			u.onTrackUpdated(track, true)
		}

		if notifyClose && u.onClose != nil {
			u.onClose()
		}
	})
}

func (u *UpTrackManager) RemovePublishedTrack(track types.MediaTrack) {
	track.RemoveAllSubscribers()
}

// should be called with lock held
func (u *UpTrackManager) getPublishedTrack(trackID livekit.TrackID) types.MediaTrack {
	return u.publishedTracks[trackID]
}

func (u *UpTrackManager) parseSubscriptionPermissions(subscriptionPermission *livekit.SubscriptionPermission) {
	// every update overrides the existing

	// all_participants takes precedence
	if subscriptionPermission.AllParticipants {
		// everything is allowed, nothing else to do
		u.subscriberPermissions = nil
		return
	}

	// per participant permissions
	u.subscriberPermissions = make(map[livekit.ParticipantID]*livekit.TrackPermission)
	for _, trackPerms := range subscriptionPermission.TrackPermissions {
		u.subscriberPermissions[livekit.ParticipantID(trackPerms.ParticipantSid)] = trackPerms
	}
}

func (u *UpTrackManager) hasPermission(trackID livekit.TrackID, subscriberID livekit.ParticipantID) bool {
	if u.subscriberPermissions == nil {
		return true
	}

	perms, ok := u.subscriberPermissions[subscriberID]
	if !ok {
		return false
	}

	if perms.AllTracks {
		return true
	}

	for _, sid := range perms.TrackSids {
		if livekit.TrackID(sid) == trackID {
			return true
		}
	}

	return false
}

func (u *UpTrackManager) getAllowedSubscribers(trackID livekit.TrackID) []livekit.ParticipantID {
	if u.subscriberPermissions == nil {
		return nil
	}

	allowed := make([]livekit.ParticipantID, 0)
	for subscriberID, perms := range u.subscriberPermissions {
		if perms.AllTracks {
			allowed = append(allowed, subscriberID)
			continue
		}

		for _, sid := range perms.TrackSids {
			if livekit.TrackID(sid) == trackID {
				allowed = append(allowed, subscriberID)
				break
			}
		}
	}

	return allowed
}

func (u *UpTrackManager) maybeAddPendingSubscription(trackID livekit.TrackID, sub types.LocalParticipant) {
	subscriberID := sub.ID()

	pending := u.pendingSubscriptions[trackID]
	for _, sid := range pending {
		if sid == subscriberID {
			// already pending
			return
		}
	}

	u.pendingSubscriptions[trackID] = append(u.pendingSubscriptions[trackID], subscriberID)
	go sub.SubscriptionPermissionUpdate(u.params.SID, trackID, false)
}

func (u *UpTrackManager) maybeRemovePendingSubscription(trackID livekit.TrackID, sub types.LocalParticipant) {
	subscriberID := sub.ID()

	pending := u.pendingSubscriptions[trackID]
	n := len(pending)
	for idx, sid := range pending {
		if sid == subscriberID {
			u.pendingSubscriptions[trackID][idx] = u.pendingSubscriptions[trackID][n-1]
			u.pendingSubscriptions[trackID] = u.pendingSubscriptions[trackID][:n-1]
			break
		}
	}
	if len(u.pendingSubscriptions[trackID]) == 0 {
		delete(u.pendingSubscriptions, trackID)
	}
}

func (u *UpTrackManager) processPendingSubscriptions(resolver func(participantID livekit.ParticipantID) types.LocalParticipant) {
	updatedPendingSubscriptions := make(map[livekit.TrackID][]livekit.ParticipantID)
	for trackID, pending := range u.pendingSubscriptions {
		track := u.getPublishedTrack(trackID)
		if track == nil {
			continue
		}

		var updatedPending []livekit.ParticipantID
		for _, sid := range pending {
			var sub types.LocalParticipant
			if resolver != nil {
				sub = resolver(sid)
			}
			if sub == nil || sub.State() == livekit.ParticipantInfo_DISCONNECTED {
				// do not keep this pending subscription as subscriber may be gone
				continue
			}

			if !u.hasPermission(trackID, sid) {
				updatedPending = append(updatedPending, sid)
				continue
			}

			if err := track.AddSubscriber(sub); err != nil {
				u.params.Logger.Errorw("error reinstating pending subscription", err)
				// keep it in pending on error in case the error is transient
				updatedPending = append(updatedPending, sid)
				continue
			}

			go sub.SubscriptionPermissionUpdate(u.params.SID, trackID, true)
		}

		updatedPendingSubscriptions[trackID] = updatedPending
	}

	u.pendingSubscriptions = updatedPendingSubscriptions
}

func (u *UpTrackManager) maybeRevokeSubscriptions(resolver func(participantID livekit.ParticipantID) types.LocalParticipant) {
	for _, track := range u.publishedTracks {
		trackID := track.ID()
		allowed := u.getAllowedSubscribers(trackID)
		if allowed == nil {
			// no restrictions
			continue
		}

		revokedSubscribers := track.RevokeDisallowedSubscribers(allowed)
		for _, subID := range revokedSubscribers {
			var sub types.LocalParticipant
			if resolver != nil {
				sub = resolver(subID)
			}
			if sub == nil {
				continue
			}

			u.maybeAddPendingSubscription(trackID, sub)
		}
	}
}

func (u *UpTrackManager) DebugInfo() map[string]interface{} {
	info := map[string]interface{}{}
	publishedTrackInfo := make(map[livekit.TrackID]interface{})

	u.lock.RLock()
	for trackID, track := range u.publishedTracks {
		if mt, ok := track.(*MediaTrack); ok {
			publishedTrackInfo[trackID] = mt.DebugInfo()
		} else {
			publishedTrackInfo[trackID] = map[string]interface{}{
				"ID":       track.ID(),
				"Kind":     track.Kind().String(),
				"PubMuted": track.IsMuted(),
			}
		}
	}
	u.lock.RUnlock()

	info["PublishedTracks"] = publishedTrackInfo

	return info
}
