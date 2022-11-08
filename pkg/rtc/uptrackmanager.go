package rtc

import (
	"errors"
	"sync"
	"time"

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
	publishedTracks               map[livekit.TrackID]types.MediaTrack
	subscriptionPermission        *livekit.SubscriptionPermission
	subscriptionPermissionVersion *utils.TimedVersion
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

func (u *UpTrackManager) Close(willBeResumed bool) {
	u.opsQueue.Stop()

	u.lock.Lock()
	u.closed = true
	notify := len(u.publishedTracks) == 0
	u.lock.Unlock()

	// remove all subscribers
	for _, t := range u.GetPublishedTracks() {
		t.ClearAllReceivers(willBeResumed)
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
	u.lock.Lock()
	defer u.lock.Unlock()

	var tracks []types.MediaTrack
	if params.AllTracks {
		for _, t := range u.publishedTracks {
			tracks = append(tracks, t)
		}
	} else {
		for _, trackID := range params.TrackIDs {
			track := u.getPublishedTrackLocked(trackID)
			if track == nil {
				continue
			}

			tracks = append(tracks, track)
		}
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
		if !u.hasPermissionLocked(trackID, subscriberIdentity) {
			u.maybeAddPendingSubscriptionLocked(trackID, subscriberIdentity, sub, nil)
			continue
		}

		if err := track.AddSubscriber(sub); err != nil {
			return n, err
		}
		n += 1

		u.maybeRemovePendingSubscriptionLocked(trackID, sub, true)
	}
	return n, nil
}

func (u *UpTrackManager) RemoveSubscriber(sub types.LocalParticipant, trackID livekit.TrackID, willBeResumed bool) {
	u.lock.Lock()
	defer u.lock.Unlock()

	track := u.getPublishedTrackLocked(trackID)
	if track != nil {
		track.RemoveSubscriber(sub.ID(), willBeResumed)
	}

	u.maybeRemovePendingSubscriptionLocked(trackID, sub, false)
}

func (u *UpTrackManager) SetPublishedTrackMuted(trackID livekit.TrackID, muted bool) types.MediaTrack {
	u.lock.RLock()
	track := u.publishedTracks[trackID]
	u.lock.RUnlock()

	if track != nil {
		currentMuted := track.IsMuted()
		track.SetMuted(muted)

		if currentMuted != track.IsMuted() {
			u.params.Logger.Infow("publisher mute status changed", "trackID", trackID, "muted", track.IsMuted())
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

	return u.getPublishedTrackLocked(trackID)
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
	timedVersion *livekit.TimedVersion,
	resolverByIdentity func(participantIdentity livekit.ParticipantIdentity) types.LocalParticipant,
	resolverBySid func(participantID livekit.ParticipantID) types.LocalParticipant,
) error {
	u.lock.Lock()
	if timedVersion != nil {
		if u.subscriptionPermissionVersion != nil {
			tv := utils.NewTimedVersionFromProto(timedVersion)
			// ignore older version
			if !tv.After(u.subscriptionPermissionVersion) {
				perms := ""
				if u.subscriptionPermission != nil {
					perms = u.subscriptionPermission.String()
				}
				u.params.Logger.Infow(
					"skipping older subscription permission version",
					"existingValue", perms,
					"existingVersion", u.subscriptionPermissionVersion.ToProto().String(),
					"requestingValue", subscriptionPermission.String(),
					"requestingVersion", timedVersion.String(),
				)
				u.lock.Unlock()
				return nil
			}
			u.subscriptionPermissionVersion.Update(time.UnixMicro(timedVersion.UnixMicro))
		} else {
			u.subscriptionPermissionVersion = utils.NewTimedVersionFromProto(timedVersion)
		}
	} else {
		// use current time as the new/updated version
		if u.subscriptionPermissionVersion == nil {
			u.subscriptionPermissionVersion = utils.NewTimedVersion(time.Now(), 0)
		} else {
			u.subscriptionPermissionVersion.Update(time.Now())
		}
	}

	// store as is for use when migrating
	u.subscriptionPermission = subscriptionPermission
	if subscriptionPermission == nil {
		u.params.Logger.Infow(
			"updating subscription permission, setting to nil",
			"version", u.subscriptionPermissionVersion.ToProto().String(),
		)
		// possible to get a nil when migrating
		u.lock.Unlock()
		return nil
	}

	u.params.Logger.Infow(
		"updating subscription permission",
		"permissions", u.subscriptionPermission.String(),
		"version", u.subscriptionPermissionVersion.ToProto().String(),
	)
	if err := u.parseSubscriptionPermissions(subscriptionPermission, resolverBySid); err != nil {
		// when failed, do not override previous permissions
		u.params.Logger.Errorw("failed updating subscription permission", err)
		u.lock.Unlock()
		return err
	}
	u.lock.Unlock()

	u.processPendingSubscriptions(resolverByIdentity)
	u.maybeRevokeSubscriptions(resolverByIdentity)

	return nil
}

func (u *UpTrackManager) SubscriptionPermission() (*livekit.SubscriptionPermission, *livekit.TimedVersion) {
	u.lock.RLock()
	defer u.lock.RUnlock()

	if u.subscriptionPermissionVersion == nil {
		return nil, nil
	}

	return u.subscriptionPermission, u.subscriptionPermissionVersion.ToProto()
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

func (u *UpTrackManager) RemovePublishedTrack(track types.MediaTrack, willBeResumed bool, shouldClose bool) {
	if shouldClose {
		track.Close(willBeResumed)
	} else {
		track.ClearAllReceivers(willBeResumed)
	}
	u.lock.Lock()
	delete(u.publishedTracks, track.ID())
	u.lock.Unlock()
}

func (u *UpTrackManager) getPublishedTrackLocked(trackID livekit.TrackID) types.MediaTrack {
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

func (u *UpTrackManager) hasPermissionLocked(trackID livekit.TrackID, subscriberIdentity livekit.ParticipantIdentity) bool {
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

func (u *UpTrackManager) getAllowedSubscribersLocked(trackID livekit.TrackID) []livekit.ParticipantIdentity {
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

func (u *UpTrackManager) maybeAddPendingSubscriptionLocked(
	trackID livekit.TrackID,
	subscriberIdentity livekit.ParticipantIdentity,
	sub types.LocalParticipant,
	resolver func(participantIdentity livekit.ParticipantIdentity) types.LocalParticipant,
) {
	pending := u.pendingSubscriptions[trackID]
	for _, identity := range pending {
		if identity == subscriberIdentity {
			// already pending
			return
		}
	}

	u.pendingSubscriptions[trackID] = append(u.pendingSubscriptions[trackID], subscriberIdentity)
	u.params.Logger.Debugw("adding pending subscription", "subscriberIdentity", subscriberIdentity, "trackID", trackID)
	u.opsQueue.Enqueue(func() {
		if sub == nil {
			if resolver == nil {
				u.params.Logger.Warnw("no resolver", nil)
			} else {
				sub = resolver(subscriberIdentity)
			}
		}

		if sub != nil {
			sub.SubscriptionPermissionUpdate(u.params.SID, trackID, false)
		} else {
			u.params.Logger.Warnw("could not send subscription permission update, no subscriber", nil, "subscriberIdentity", subscriberIdentity)
		}
	})
}

func (u *UpTrackManager) maybeRemovePendingSubscriptionLocked(trackID livekit.TrackID, sub types.LocalParticipant, sendUpdate bool) {
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
	type ResolvedInfo struct {
		sub   types.LocalParticipant
		state livekit.ParticipantInfo_State
	}

	// gather all identites that need resolving
	resolvedInfos := make(map[livekit.ParticipantIdentity]*ResolvedInfo)
	u.lock.RLock()
	for trackID, pending := range u.pendingSubscriptions {
		track := u.getPublishedTrackLocked(trackID)
		if track == nil {
			// published track is gone
			continue
		}

		for _, identity := range pending {
			resolvedInfos[identity] = nil
		}
	}
	u.lock.RUnlock()

	for identity := range resolvedInfos {
		sub := resolver(identity)
		if sub != nil {
			resolvedInfos[identity] = &ResolvedInfo{
				sub:   sub,
				state: sub.State(),
			}
		}
	}

	// check for subscriptions that can be reinstated
	u.lock.Lock()
	updatedPendingSubscriptions := make(map[livekit.TrackID][]livekit.ParticipantIdentity)
	for trackID, pending := range u.pendingSubscriptions {
		track := u.getPublishedTrackLocked(trackID)
		if track == nil {
			// published track is gone
			continue
		}

		var updatedPending []livekit.ParticipantIdentity
		for _, identity := range pending {
			resolvedInfo := resolvedInfos[identity]
			if resolvedInfo == nil || resolvedInfo.sub == nil || resolvedInfo.state == livekit.ParticipantInfo_DISCONNECTED {
				// do not keep this pending subscription as subscriber may be gone
				continue
			}

			if !u.hasPermissionLocked(trackID, identity) {
				updatedPending = append(updatedPending, identity)
				continue
			}

			sub := resolvedInfo.sub
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
	u.lock.Unlock()
}

func (u *UpTrackManager) maybeRevokeSubscriptions(resolver func(participantIdentity livekit.ParticipantIdentity) types.LocalParticipant) {
	u.lock.Lock()
	defer u.lock.Unlock()

	for trackID, track := range u.publishedTracks {
		allowed := u.getAllowedSubscribersLocked(trackID)
		if allowed == nil {
			// no restrictions
			continue
		}

		revoked := track.RevokeDisallowedSubscribers(allowed)
		for _, subIdentity := range revoked {
			u.maybeAddPendingSubscriptionLocked(trackID, subIdentity, nil, resolver)
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
