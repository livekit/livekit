package rtc

import (
	"errors"
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

type UptrackManagerParams struct {
	SID    livekit.ParticipantID
	Logger logger.Logger
}

type UptrackManager struct {
	params UptrackManagerParams

	closed bool

	// publishedTracks that participant is publishing
	publishedTracks map[livekit.TrackID]types.PublishedTrack
	// keeps track of subscriptions that are awaiting permissions
	subscriptionPermissions map[livekit.ParticipantID]*livekit.TrackPermission // subscriberID => *livekit.TrackPermission
	// keeps tracks of track specific subscribers who are awaiting permission
	pendingSubscriptions map[livekit.TrackID][]livekit.ParticipantID // trackID => []subscriberID

	lock sync.RWMutex

	// callbacks & handlers
	onClose                      func()
	onTrackUpdated               func(track types.PublishedTrack, onlyIfReady bool)
	onSubscribedMaxQualityChange func(trackID livekit.TrackID, subscribedQualities []*livekit.SubscribedQuality, maxSubscribedQuality livekit.VideoQuality) error
}

func NewUptrackManager(params UptrackManagerParams) *UptrackManager {
	return &UptrackManager{
		params:               params,
		publishedTracks:      make(map[livekit.TrackID]types.PublishedTrack, 0),
		pendingSubscriptions: make(map[livekit.TrackID][]livekit.ParticipantID),
	}
}

func (u *UptrackManager) Start() {
}

func (u *UptrackManager) Close() {
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

func (u *UptrackManager) OnUptrackManagerClose(f func()) {
	u.onClose = f
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

func (u *UptrackManager) OnPublishedTrackUpdated(f func(track types.PublishedTrack, onlyIfReady bool)) {
	u.onTrackUpdated = f
}

func (u *UptrackManager) OnSubscribedMaxQualityChange(f func(trackID livekit.TrackID, subscribedQualities []*livekit.SubscribedQuality, maxSubscribedQuality livekit.VideoQuality) error) {
	u.onSubscribedMaxQualityChange = f
}

// AddSubscriber subscribes op to all publishedTracks
func (u *UptrackManager) AddSubscriber(sub types.LocalParticipant, params types.AddSubscriberParams) (int, error) {
	var tracks []types.PublishedTrack
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

func (u *UptrackManager) RemoveSubscriber(sub types.LocalParticipant, trackID livekit.TrackID) {
	track := u.GetPublishedTrack(trackID)
	if track != nil {
		track.RemoveSubscriber(sub.ID())
	}

	u.lock.Lock()
	u.maybeRemovePendingSubscription(trackID, sub)
	u.lock.Unlock()
}

func (u *UptrackManager) SetPublishedTrackMuted(trackID livekit.TrackID, muted bool) types.PublishedTrack {
	u.lock.RLock()
	track := u.publishedTracks[trackID]
	u.lock.RUnlock()

	if track != nil {
		currentMuted := track.IsMuted()
		track.SetMuted(muted)

		if currentMuted != track.IsMuted() && u.onTrackUpdated != nil {
			u.params.Logger.Debugw("mute status changed",
				"track", trackID,
				"muted", track.IsMuted())
			u.onTrackUpdated(track, false)
		}
	}

	return track
}

func (u *UptrackManager) GetPublishedTrack(trackID livekit.TrackID) types.PublishedTrack {
	u.lock.RLock()
	defer u.lock.RUnlock()

	return u.getPublishedTrack(trackID)
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
	resolver func(participantID livekit.ParticipantID) types.LocalParticipant,
) error {
	u.lock.Lock()
	defer u.lock.Unlock()

	u.updateSubscriptionPermissions(permissions)

	u.processPendingSubscriptions(resolver)

	u.maybeRevokeSubscriptions(resolver)

	return nil
}

func (u *UptrackManager) UpdateVideoLayers(updateVideoLayers *livekit.UpdateVideoLayers) error {
	track := u.GetPublishedTrack(livekit.TrackID(updateVideoLayers.TrackSid))
	if track == nil {
		return errors.New("could not find published track")
	}

	track.UpdateVideoLayers(updateVideoLayers.Layers)
	return nil
}

func (u *UptrackManager) AddPublishedTrack(track types.PublishedTrack) {
	track.OnSubscribedMaxQualityChange(u.onSubscribedMaxQualityChange)

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

func (u *UptrackManager) RemovePublishedTrack(track types.PublishedTrack) {
	track.RemoveAllSubscribers()
}

// should be called with lock held
func (u *UptrackManager) getPublishedTrack(trackID livekit.TrackID) types.PublishedTrack {
	return u.publishedTracks[trackID]
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
	u.subscriptionPermissions = make(map[livekit.ParticipantID]*livekit.TrackPermission)
	for _, trackPerms := range permissions.TrackPermissions {
		u.subscriptionPermissions[livekit.ParticipantID(trackPerms.ParticipantSid)] = trackPerms
	}
}

func (u *UptrackManager) hasPermission(trackID livekit.TrackID, subscriberID livekit.ParticipantID) bool {
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
		if livekit.TrackID(sid) == trackID {
			return true
		}
	}

	return false
}

func (u *UptrackManager) getAllowedSubscribers(trackID livekit.TrackID) []livekit.ParticipantID {
	if u.subscriptionPermissions == nil {
		return nil
	}

	allowed := []livekit.ParticipantID{}
	for subscriberID, perms := range u.subscriptionPermissions {
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

func (u *UptrackManager) maybeAddPendingSubscription(trackID livekit.TrackID, sub types.LocalParticipant) {
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

func (u *UptrackManager) maybeRemovePendingSubscription(trackID livekit.TrackID, sub types.LocalParticipant) {
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

func (u *UptrackManager) processPendingSubscriptions(resolver func(participantID livekit.ParticipantID) types.LocalParticipant) {
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

func (u *UptrackManager) maybeRevokeSubscriptions(resolver func(participantID livekit.ParticipantID) types.LocalParticipant) {
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

func (u *UptrackManager) DebugInfo() map[string]interface{} {
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
