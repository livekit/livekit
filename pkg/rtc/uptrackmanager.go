// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rtc

import (
	"errors"
	"maps"
	"slices"
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/utils"
)

var (
	ErrSubscriptionPermissionNeedsId = errors.New("either participant identity or SID needed")
)

type UpTrackManagerParams struct {
	Logger           logger.Logger
	VersionGenerator utils.TimedVersionGenerator
}

// UpTrackManager manages all uptracks from a participant
type UpTrackManager struct {
	// utils.TimedVersion is a atomic. To be correctly aligned also on 32bit archs
	// 64it atomics need to be at the front of a struct
	subscriptionPermissionVersion utils.TimedVersion

	params UpTrackManagerParams

	closed bool

	// publishedTracks that participant is publishing
	publishedTracks        map[livekit.TrackID]types.MediaTrack
	subscriptionPermission *livekit.SubscriptionPermission
	// subscriber permission for published tracks (both media and data)
	subscriberPermissions map[livekit.ParticipantIdentity]*livekit.TrackPermission // subscriberIdentity => *livekit.TrackPermission

	lock sync.RWMutex

	// callbacks & handlers
	onClose        func()
	onTrackUpdated func(track types.MediaTrack)
}

func NewUpTrackManager(params UpTrackManagerParams) *UpTrackManager {
	return &UpTrackManager{
		params:          params,
		publishedTracks: make(map[livekit.TrackID]types.MediaTrack),
	}
}

func (u *UpTrackManager) Close(isExpectedToResume bool) {
	u.lock.Lock()
	if u.closed {
		u.lock.Unlock()
		return
	}

	u.closed = true

	publishedTracks := u.publishedTracks
	u.publishedTracks = make(map[livekit.TrackID]types.MediaTrack)
	u.lock.Unlock()

	for _, t := range publishedTracks {
		t.Close(isExpectedToResume)
	}

	if onClose := u.getOnUpTrackManagerClose(); onClose != nil {
		onClose()
	}
}

func (u *UpTrackManager) OnUpTrackManagerClose(f func()) {
	u.lock.Lock()
	u.onClose = f
	u.lock.Unlock()
}

func (u *UpTrackManager) getOnUpTrackManagerClose() func() {
	u.lock.RLock()
	defer u.lock.RUnlock()

	return u.onClose
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

func (u *UpTrackManager) OnPublishedTrackUpdated(f func(track types.MediaTrack)) {
	u.onTrackUpdated = f
}

func (u *UpTrackManager) SetPublishedTrackMuted(trackID livekit.TrackID, muted bool) (types.MediaTrack, bool) {
	changed := false
	track := u.GetPublishedTrack(trackID)
	if track != nil {
		currentMuted := track.IsMuted()
		track.SetMuted(muted)

		if currentMuted != track.IsMuted() {
			changed = true
			u.params.Logger.Debugw("publisher mute status changed", "trackID", trackID, "muted", track.IsMuted())
			if u.onTrackUpdated != nil {
				u.onTrackUpdated(track)
			}
		}
	}

	return track, changed
}

func (u *UpTrackManager) GetPublishedTrack(trackID livekit.TrackID) types.MediaTrack {
	u.lock.RLock()
	defer u.lock.RUnlock()

	return u.getPublishedTrackLocked(trackID)
}

func (u *UpTrackManager) GetPublishedTracks() []types.MediaTrack {
	u.lock.RLock()
	defer u.lock.RUnlock()

	return slices.Collect(maps.Values(u.publishedTracks))
}

func (u *UpTrackManager) isStaleSubscriptionPermissionLocked(timedVersion utils.TimedVersion) bool {
	return !timedVersion.IsZero() && !timedVersion.After(u.subscriptionPermissionVersion)
}

func (u *UpTrackManager) logOlderSubscriptionPermissionLocked(subscriptionPermission *livekit.SubscriptionPermission, timedVersion utils.TimedVersion) {
	u.params.Logger.Debugw(
		"skipping older subscription permission version",
		"existingValue", logger.Proto(u.subscriptionPermission),
		"existingVersion", &u.subscriptionPermissionVersion,
		"requestingValue", logger.Proto(subscriptionPermission),
		"requestingVersion", &timedVersion,
	)
}

func (u *UpTrackManager) UpdateSubscriptionPermission(
	subscriptionPermission *livekit.SubscriptionPermission,
	timedVersion utils.TimedVersion,
	resolverBySid func(participantID livekit.ParticipantID) types.LocalParticipant,
) error {
	return u.updateSubscriptionPermission(subscriptionPermission, timedVersion, resolverBySid, nil)
}

func (u *UpTrackManager) updateSubscriptionPermission(
	subscriptionPermission *livekit.SubscriptionPermission,
	timedVersion utils.TimedVersion,
	resolverBySid func(participantID livekit.ParticipantID) types.LocalParticipant,
	onCommittedLocked func(getAllowedSubscribers func(trackID livekit.TrackID) []livekit.ParticipantIdentity),
) error {
	if !timedVersion.IsZero() {
		u.lock.RLock()
		stale := u.isStaleSubscriptionPermissionLocked(timedVersion)
		if stale {
			u.logOlderSubscriptionPermissionLocked(subscriptionPermission, timedVersion)
		}
		u.lock.RUnlock()
		if stale {
			return nil
		}
	}

	var (
		parsedPermissions map[livekit.ParticipantIdentity]*livekit.TrackPermission
		err               error
	)
	if subscriptionPermission != nil {
		// Validate and resolve participant SIDs before taking u.lock so resolverBySid
		// never runs under u.lock.
		parsedPermissions, err = parseSubscriptionPermissions(subscriptionPermission, resolverBySid, u.params.Logger)
		if err != nil {
			u.params.Logger.Errorw("failed updating subscription permission", err)
			return err
		}
	}

	u.lock.Lock()
	defer u.lock.Unlock()

	if !timedVersion.IsZero() {
		// it's possible for permission updates to come from another node. In that case
		// they would be the authority for this participant's permissions
		// we do not want to initialize subscriptionPermissionVersion too early since if another machine is the
		// owner for the data, we'd prefer to use their TimedVersion
		// ignore older version
		if u.isStaleSubscriptionPermissionLocked(timedVersion) {
			u.logOlderSubscriptionPermissionLocked(subscriptionPermission, timedVersion)
			return nil
		}
		u.subscriptionPermissionVersion.Update(timedVersion)
	} else {
		// for requests coming from the current node, use local versions
		// Advance past remote versions even when the local clock is behind.
		u.subscriptionPermissionVersion.Update(max(u.params.VersionGenerator.Next(), u.subscriptionPermissionVersion.Next()))
	}

	// store as is for use when migrating
	u.subscriptionPermission = subscriptionPermission
	if subscriptionPermission == nil {
		u.params.Logger.Debugw(
			"updating subscription permission, setting to nil",
			"version", u.subscriptionPermissionVersion,
		)
		// possible to get a nil when migrating
		return nil
	}

	u.subscriberPermissions = parsedPermissions
	u.params.Logger.Debugw(
		"updating subscription permission",
		"permissions", logger.Proto(u.subscriptionPermission),
		"version", u.subscriptionPermissionVersion,
	)

	u.maybeRevokeSubscriptionsLocked()
	if onCommittedLocked != nil && u.subscriberPermissions != nil {
		onCommittedLocked(u.getAllowedSubscribersLocked)
	}

	return nil
}

func (u *UpTrackManager) SubscriptionPermission() (*livekit.SubscriptionPermission, utils.TimedVersion) {
	u.lock.RLock()
	defer u.lock.RUnlock()

	if u.subscriptionPermissionVersion.IsZero() {
		return nil, u.subscriptionPermissionVersion.Load()
	}

	return u.subscriptionPermission, u.subscriptionPermissionVersion.Load()
}

func (u *UpTrackManager) HasPermission(trackID livekit.TrackID, subIdentity livekit.ParticipantIdentity) bool {
	u.lock.RLock()
	defer u.lock.RUnlock()

	return u.hasPermissionLocked(trackID, subIdentity)
}

func (u *UpTrackManager) GetAllowedSubscribers(trackID livekit.TrackID) []livekit.ParticipantIdentity {
	u.lock.RLock()
	defer u.lock.RUnlock()

	return u.getAllowedSubscribersLocked(trackID)
}

func (u *UpTrackManager) UpdatePublishedAudioTrack(update *livekit.UpdateLocalAudioTrack) types.MediaTrack {
	track := u.GetPublishedTrack(livekit.TrackID(update.TrackSid))
	if track != nil {
		track.UpdateAudioTrack(update)
		if u.onTrackUpdated != nil {
			u.onTrackUpdated(track)
		}
	}

	return track
}

func (u *UpTrackManager) UpdatePublishedVideoTrack(update *livekit.UpdateLocalVideoTrack) types.MediaTrack {
	track := u.GetPublishedTrack(livekit.TrackID(update.TrackSid))
	if track != nil {
		track.UpdateVideoTrack(update)
		if u.onTrackUpdated != nil {
			u.onTrackUpdated(track)
		}
	}

	return track
}

func (u *UpTrackManager) AddPublishedTrack(track types.MediaTrack) {
	u.lock.Lock()
	if _, ok := u.publishedTracks[track.ID()]; !ok {
		u.publishedTracks[track.ID()] = track
	}
	u.lock.Unlock()
	u.params.Logger.Debugw("added published track", "trackID", track.ID(), "trackInfo", logger.Proto(track.ToProto()))

	track.AddOnClose(func(_isExpectedToResume bool) {
		u.lock.Lock()
		delete(u.publishedTracks, track.ID())
		// not modifying subscription permissions, will get reset on next update from participant
		u.lock.Unlock()
	})
}

func (u *UpTrackManager) RemovePublishedTrack(track types.MediaTrack, isExpectedToResume bool) {
	track.Close(isExpectedToResume)

	u.lock.Lock()
	delete(u.publishedTracks, track.ID())
	u.lock.Unlock()
}

func (u *UpTrackManager) getPublishedTrackLocked(trackID livekit.TrackID) types.MediaTrack {
	return u.publishedTracks[trackID]
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

// returns a list of participants that are allowed to subscribe to the track. if nil is returned, it means everyone is
// allowed to subscribe to this track
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

func (u *UpTrackManager) maybeRevokeSubscriptionsLocked() {
	if u.subscriberPermissions == nil {
		return
	}

	for trackID, track := range u.publishedTracks {
		allowed := u.getAllowedSubscribersLocked(trackID)
		if allowed == nil {
			// no restrictions
			continue
		}

		track.RevokeDisallowedSubscribers(allowed)
	}
}

func (u *UpTrackManager) DebugInfo() map[string]any {
	info := map[string]any{}
	publishedTrackInfo := make(map[livekit.TrackID]any)

	u.lock.RLock()
	for trackID, track := range u.publishedTracks {
		if mt, ok := track.(*MediaTrack); ok {
			publishedTrackInfo[trackID] = mt.DebugInfo()
		} else {
			publishedTrackInfo[trackID] = map[string]any{
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

func (u *UpTrackManager) GetAudioLevel() (level float64, active bool) {
	level = 0
	for _, pt := range u.GetPublishedTracks() {
		if pt.Source() == livekit.TrackSource_MICROPHONE {
			tl, ta := pt.GetAudioLevel()
			if ta {
				active = true
				if tl > level {
					level = tl
				}
			}
		}
	}
	return
}

func parseSubscriptionPermissions(
	subscriptionPermission *livekit.SubscriptionPermission,
	resolver func(participantID livekit.ParticipantID) types.LocalParticipant,
	log logger.Logger,
) (map[livekit.ParticipantIdentity]*livekit.TrackPermission, error) {
	// every update overrides the existing

	// all_participants takes precedence
	if subscriptionPermission.AllParticipants {
		// everything is allowed, nothing else to do
		return nil, nil
	}

	resolve := func(pID livekit.ParticipantID) types.LocalParticipant {
		if resolver == nil {
			return nil
		}
		return resolver(pID)
	}

	// per participant permissions
	subscriberPermissions := make(map[livekit.ParticipantIdentity]*livekit.TrackPermission, len(subscriptionPermission.TrackPermissions))
	for _, trackPerms := range subscriptionPermission.TrackPermissions {
		if trackPerms == nil {
			return nil, ErrSubscriptionPermissionNeedsId
		}

		subscriberIdentity := livekit.ParticipantIdentity(trackPerms.ParticipantIdentity)
		if subscriberIdentity == "" {
			if trackPerms.ParticipantSid == "" {
				return nil, ErrSubscriptionPermissionNeedsId
			}

			sub := resolve(livekit.ParticipantID(trackPerms.ParticipantSid))
			if sub == nil {
				log.Warnw(
					"could not find subscriber for permissions update", nil,
					"subscriberID", trackPerms.ParticipantSid,
					"subscriptionPermission", logger.Proto(subscriptionPermission),
				)
				continue
			}

			subscriberIdentity = sub.Identity()
		} else if trackPerms.ParticipantSid != "" {
			sub := resolve(livekit.ParticipantID(trackPerms.ParticipantSid))
			if sub != nil && sub.Identity() != subscriberIdentity {
				log.Errorw(
					"participant identity mismatch", nil,
					"expected", subscriberIdentity,
					"got", sub.Identity(),
					"subscriptionPermission", logger.Proto(subscriptionPermission),
				)
			} else if sub == nil {
				log.Warnw(
					"could not find subscriber for permissions update", nil,
					"subscriberID", trackPerms.ParticipantSid,
					"subscriptionPermission", logger.Proto(subscriptionPermission),
				)
			}
		}

		subscriberPermissions[subscriberIdentity] = trackPerms
	}

	return subscriberPermissions, nil
}
