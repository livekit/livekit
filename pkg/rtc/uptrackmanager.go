package rtc

import (
	"errors"
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/utils"
)

var (
	ErrSubscriptionPermissionNeedsId = errors.New("either participant identity or SID needed")
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
	subscriberPermissions map[livekit.ParticipantIdentity]*livekit.TrackPermission // subscriberIdentity => *livekit.TrackPermission
	// keeps tracks of track specific subscribers who are awaiting permission
	pendingSubscriptions map[livekit.TrackID][]livekit.ParticipantIdentity // trackID => []subscriberIdentity

	opsQueue *utils.OpsQueue

	lock sync.RWMutex

	// callbacks & handlers
	onClose        func()
	onTrackUpdated func(track types.MediaTrack, onlyIfReady bool)
}

func NewUpTrackManager(params UpTrackManagerParams) *UpTrackManager {
	return &UpTrackManager{
		params:               params,
		publishedTracks:      make(map[livekit.TrackID]types.MediaTrack),
		pendingSubscriptions: make(map[livekit.TrackID][]livekit.ParticipantIdentity),
		opsQueue:             utils.NewOpsQueue(params.Logger, "utm", 20),
	}
}

func (u *UpTrackManager) Start() {
	u.opsQueue.Start()
}

func (u *UpTrackManager) Restart() {
	for _, t := range u.GetPublishedTracks() {
		t.Restart()
	}
}

func (u *UpTrackManager) Close(willBeResumed bool) {
	u.opsQueue.Stop()

	u.lock.Lock()
	u.closed = true
	notify := len(u.publishedTracks) == 0
	u.lock.Unlock()

	// remove all subscribers
	for _, t := range u.GetPublishedTracks() {
		t.RemoveAllSubscribers(willBeResumed)
	}

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

	var trackIDs []livekit.TrackID
	for _, track := range tracks {
		trackIDs = append(trackIDs, track.ID())
	}
	u.params.Logger.Debugw("subscribing participant to tracks",
		"subscriber", sub.Identity(),
		"subscriberID", sub.ID(),
		"trackIDs", trackIDs)

	n := 0
	for _, track := range tracks {
		trackID := track.ID()
		subscriberIdentity := sub.Identity()
		if !u.hasPermission(trackID, subscriberIdentity) {
			u.lock.Lock()
			u.maybeAddPendingSubscription(trackID, sub)
			u.lock.Unlock()
			continue
		}

		if err := track.AddSubscriber(sub); err != nil {
			return n, err
		}
		n += 1

		u.lock.Lock()
		u.maybeRemovePendingSubscription(trackID, sub, true)
		u.lock.Unlock()
	}
	return n, nil
}

func (u *UpTrackManager) RemoveSubscriber(sub types.LocalParticipant, trackID livekit.TrackID, willBeResumed bool) {
	track := u.GetPublishedTrack(trackID)
	if track != nil {
		track.RemoveSubscriber(sub.ID(), willBeResumed)
	}

	u.lock.Lock()
	u.maybeRemovePendingSubscription(trackID, sub, false)
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
	resolverByIdentity func(participantIdentity livekit.ParticipantIdentity) types.LocalParticipant,
	resolverBySid func(participantID livekit.ParticipantID) types.LocalParticipant,
) error {
	u.lock.Lock()
	defer u.lock.Unlock()

	u.params.Logger.Debugw("updating subscription permission", "permissions", subscriptionPermission)
	if subscriptionPermission == nil {
		// store as is for use when migrating
		u.subscriptionPermission = subscriptionPermission
		// possible to get a nil when migrating
		return nil
	}

	if err := u.parseSubscriptionPermissions(subscriptionPermission, resolverBySid); err != nil {
		// when failed, do not override previous permissions
		return err
	}

	// store as is for use when migrating
	u.subscriptionPermission = subscriptionPermission
	u.processPendingSubscriptions(resolverByIdentity)
	u.maybeRevokeSubscriptions(resolverByIdentity)

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

func (u *UpTrackManager) UpdateSubscribedQuality(nodeID livekit.NodeID, trackID livekit.TrackID, maxQualities []types.SubscribedCodecQuality) error {
	track := u.GetPublishedTrack(trackID)
	if track == nil {
		u.params.Logger.Warnw("could not find track", nil, "trackID", trackID)
		return errors.New("could not find published track")
	}

	track.NotifySubscriberNodeMaxQuality(nodeID, maxQualities)
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
	u.params.Logger.Debugw("added published track", "trackID", track.ID(), "trackInfo", track.ToProto().String())

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

func (u *UpTrackManager) RemovePublishedTrack(track types.MediaTrack, willBeResumed bool) {
	track.RemoveAllSubscribers(willBeResumed)
	u.lock.Lock()
	delete(u.publishedTracks, track.ID())
	u.lock.Unlock()
}

// should be called with lock held
func (u *UpTrackManager) getPublishedTrack(trackID livekit.TrackID) types.MediaTrack {
	return u.publishedTracks[trackID]
}

func (u *UpTrackManager) parseSubscriptionPermissions(
	subscriptionPermission *livekit.SubscriptionPermission,
	resolver func(participantID livekit.ParticipantID) types.LocalParticipant,
) error {
	// every update overrides the existing

	// all_participants takes precedence
	if subscriptionPermission.AllParticipants {
		// everything is allowed, nothing else to do
		u.subscriberPermissions = nil
		return nil
	}

	// per participant permissions
	subscriberPermissions := make(map[livekit.ParticipantIdentity]*livekit.TrackPermission)
	for _, trackPerms := range subscriptionPermission.TrackPermissions {
		subscriberIdentity := livekit.ParticipantIdentity(trackPerms.ParticipantIdentity)
		if subscriberIdentity == "" {
			if trackPerms.ParticipantSid == "" {
				return ErrSubscriptionPermissionNeedsId
			}

			sub := resolver(livekit.ParticipantID(trackPerms.ParticipantSid))
			if sub == nil {
				u.params.Logger.Warnw("could not find subscriber for permissions update", nil, "subscriberID", trackPerms.ParticipantSid)
				continue
			}

			subscriberIdentity = sub.Identity()
		} else {
			if trackPerms.ParticipantSid != "" {
				sub := resolver(livekit.ParticipantID(trackPerms.ParticipantSid))
				if sub != nil && sub.Identity() != subscriberIdentity {
					u.params.Logger.Errorw("participant identity mismatch", nil, "expected", subscriberIdentity, "got", sub.Identity())
				}
				if sub == nil {
					u.params.Logger.Warnw("could not find subscriber for permissions update", nil, "subscriberID", trackPerms.ParticipantSid)
				}
			}
		}

		subscriberPermissions[subscriberIdentity] = trackPerms
	}

	u.subscriberPermissions = subscriberPermissions

	return nil
}

func (u *UpTrackManager) hasPermission(trackID livekit.TrackID, subscriberIdentity livekit.ParticipantIdentity) bool {
	if u.subscriberPermissions == nil {
		return true
	}

	perms, ok := u.subscriberPermissions[subscriberIdentity]
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

func (u *UpTrackManager) getAllowedSubscribers(trackID livekit.TrackID) []livekit.ParticipantIdentity {
	if u.subscriberPermissions == nil {
		return nil
	}

	allowed := make([]livekit.ParticipantIdentity, 0)
	for subscriberIdentity, perms := range u.subscriberPermissions {
		if perms.AllTracks {
			allowed = append(allowed, subscriberIdentity)
			continue
		}

		for _, sid := range perms.TrackSids {
			if livekit.TrackID(sid) == trackID {
				allowed = append(allowed, subscriberIdentity)
				break
			}
		}
	}

	return allowed
}

func (u *UpTrackManager) maybeAddPendingSubscription(trackID livekit.TrackID, sub types.LocalParticipant) {
	subscriberIdentity := sub.Identity()

	pending := u.pendingSubscriptions[trackID]
	for _, identity := range pending {
		if identity == subscriberIdentity {
			// already pending
			return
		}
	}

	u.pendingSubscriptions[trackID] = append(u.pendingSubscriptions[trackID], subscriberIdentity)
	u.params.Logger.Debugw("adding pending subscription", "subscriberID", sub.ID(), "trackID", trackID)
	u.opsQueue.Enqueue(func() {
		sub.SubscriptionPermissionUpdate(u.params.SID, trackID, false)
	})
}

func (u *UpTrackManager) maybeRemovePendingSubscription(trackID livekit.TrackID, sub types.LocalParticipant, sendUpdate bool) {
	subscriberIdentity := sub.Identity()

	found := false

	pending := u.pendingSubscriptions[trackID]
	n := len(pending)
	for idx, identity := range pending {
		if identity == subscriberIdentity {
			found = true
			u.pendingSubscriptions[trackID][idx] = u.pendingSubscriptions[trackID][n-1]
			u.pendingSubscriptions[trackID] = u.pendingSubscriptions[trackID][:n-1]
			break
		}
	}
	if len(u.pendingSubscriptions[trackID]) == 0 {
		delete(u.pendingSubscriptions, trackID)
	}

	if found && sendUpdate {
		u.params.Logger.Debugw("removing pending subscription", "subscriberID", sub.ID(), "trackID", trackID)
		u.opsQueue.Enqueue(func() {
			sub.SubscriptionPermissionUpdate(u.params.SID, trackID, true)
		})
	}
}

// creates subscriptions for tracks if permissions have been granted
func (u *UpTrackManager) processPendingSubscriptions(resolver func(participantIdentity livekit.ParticipantIdentity) types.LocalParticipant) {
	updatedPendingSubscriptions := make(map[livekit.TrackID][]livekit.ParticipantIdentity)
	for trackID, pending := range u.pendingSubscriptions {
		track := u.getPublishedTrack(trackID)
		if track == nil {
			// published track is gone
			continue
		}

		var updatedPending []livekit.ParticipantIdentity
		for _, identity := range pending {
			var sub types.LocalParticipant
			if resolver != nil {
				sub = resolver(identity)
			}
			if sub == nil || sub.State() == livekit.ParticipantInfo_DISCONNECTED {
				// do not keep this pending subscription as subscriber may be gone
				continue
			}

			if !u.hasPermission(trackID, identity) {
				updatedPending = append(updatedPending, identity)
				continue
			}

			if err := track.AddSubscriber(sub); err != nil {
				u.params.Logger.Errorw("error reinstating subscription", err, "subscirberID", sub.ID(), "trackID", trackID)
				// keep it in pending on error in case the error is transient
				updatedPending = append(updatedPending, identity)
				continue
			}

			u.params.Logger.Debugw("reinstating subscription", "subscriberID", sub.ID(), "trackID", trackID)
			u.opsQueue.Enqueue(func() {
				sub.SubscriptionPermissionUpdate(u.params.SID, trackID, true)
			})
		}

		updatedPendingSubscriptions[trackID] = updatedPending
	}

	u.pendingSubscriptions = updatedPendingSubscriptions
}

func (u *UpTrackManager) maybeRevokeSubscriptions(resolver func(participantIdentity livekit.ParticipantIdentity) types.LocalParticipant) {
	for _, track := range u.publishedTracks {
		trackID := track.ID()
		allowed := u.getAllowedSubscribers(trackID)
		if allowed == nil {
			// no restrictions
			continue
		}

		revokedSubscribers := track.RevokeDisallowedSubscribers(allowed)
		for _, subIdentity := range revokedSubscribers {
			sub := resolver(subIdentity)
			if sub == nil {
				// participant may have disconnected
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
