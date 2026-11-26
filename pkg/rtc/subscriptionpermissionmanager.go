// Copyright 2026 LiveKit, Inc.
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
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

var (
	ErrSubscriptionPermissionNeedsId = errors.New("either participant identity or SID needed")
)

type revocableTrack interface {
	ID() livekit.TrackID
	RevokeDisallowedSubscribers(allowedSubscriberIdentities []livekit.ParticipantIdentity) []livekit.ParticipantIdentity
}

type SubscriptionPermissionManagerParams struct {
	Logger           logger.Logger
	VersionGenerator utils.TimedVersionGenerator
}

// SubscriptionPermissionManager manages subscription permissions and enforces
// subscriber revocations across both published media and data tracks under a single lock.
type SubscriptionPermissionManager struct {
	// utils.TimedVersion is an atomic. To be correctly aligned also on 32bit archs
	// 64bit atomics need to be at the front of a struct
	subscriptionPermissionVersion utils.TimedVersion

	params SubscriptionPermissionManagerParams

	lock                   sync.RWMutex
	subscriptionPermission *livekit.SubscriptionPermission
	// subscriber permission for published tracks (both media and data)
	subscriberPermissions map[livekit.ParticipantIdentity]*livekit.TrackPermission // subscriberIdentity => *livekit.TrackPermission
	tracks                map[livekit.TrackID]revocableTrack
}

func NewSubscriptionPermissionManager(params SubscriptionPermissionManagerParams) *SubscriptionPermissionManager {
	if params.Logger == nil {
		params.Logger = logger.GetLogger()
	}
	if params.VersionGenerator == nil {
		params.VersionGenerator = utils.NewDefaultTimedVersionGenerator()
	}

	return &SubscriptionPermissionManager{
		params: params,
		tracks: make(map[livekit.TrackID]revocableTrack),
	}
}

func (m *SubscriptionPermissionManager) addTrack(track revocableTrack) {
	if track == nil {
		return
	}

	m.lock.Lock()
	m.tracks[track.ID()] = track
	m.lock.Unlock()
}

func (m *SubscriptionPermissionManager) removeTrack(track revocableTrack) {
	if track == nil {
		return
	}

	m.lock.Lock()
	trackID := track.ID()
	if m.tracks[trackID] == track {
		delete(m.tracks, trackID)
	}
	m.lock.Unlock()
}

func (m *SubscriptionPermissionManager) UpdateSubscriptionPermission(
	subscriptionPermission *livekit.SubscriptionPermission,
	timedVersion utils.TimedVersion,
	resolverBySid func(participantID livekit.ParticipantID) types.LocalParticipant,
) error {
	if !timedVersion.IsZero() {
		m.lock.RLock()
		stale := m.isStaleSubscriptionPermissionLocked(timedVersion)
		if stale {
			m.logOlderSubscriptionPermissionLocked(subscriptionPermission, timedVersion)
		}
		m.lock.RUnlock()
		if stale {
			return nil
		}
	}

	var (
		parsedPermissions map[livekit.ParticipantIdentity]*livekit.TrackPermission
		err               error
	)
	if subscriptionPermission != nil {
		// Validate and resolve participant SIDs before taking m.lock so resolverBySid
		// never runs under m.lock.
		parsedPermissions, err = parseSubscriptionPermissions(subscriptionPermission, resolverBySid, m.params.Logger)
		if err != nil {
			m.params.Logger.Errorw("failed updating subscription permission", err)
			return err
		}
	}

	m.lock.Lock()
	defer m.lock.Unlock()

	if !timedVersion.IsZero() {
		// it's possible for permission updates to come from another node. In that case
		// they would be the authority for this participant's permissions
		// we do not want to initialize subscriptionPermissionVersion too early since if another machine is the
		// owner for the data, we'd prefer to use their TimedVersion
		// ignore older version
		if m.isStaleSubscriptionPermissionLocked(timedVersion) {
			m.logOlderSubscriptionPermissionLocked(subscriptionPermission, timedVersion)
			return nil
		}
		m.subscriptionPermissionVersion.Update(timedVersion)
	} else {
		// for requests coming from the current node, use local versions
		// Advance past remote versions even when the local clock is behind.
		m.subscriptionPermissionVersion.Update(max(m.params.VersionGenerator.Next(), m.subscriptionPermissionVersion.Next()))
	}

	// store as is for use when migrating
	m.subscriptionPermission = subscriptionPermission
	if subscriptionPermission == nil {
		m.params.Logger.Debugw(
			"updating subscription permission, setting to nil",
			"version", m.subscriptionPermissionVersion,
		)
		// possible to get a nil when migrating
		return nil
	}

	m.subscriberPermissions = parsedPermissions
	m.params.Logger.Debugw(
		"updating subscription permission",
		"permissions", logger.Proto(m.subscriptionPermission),
		"version", m.subscriptionPermissionVersion,
	)

	m.maybeRevokeSubscriptionsLocked()
	return nil
}

func (m *SubscriptionPermissionManager) SubscriptionPermission() (*livekit.SubscriptionPermission, utils.TimedVersion) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	if m.subscriptionPermissionVersion.IsZero() {
		return nil, m.subscriptionPermissionVersion.Load()
	}

	return m.subscriptionPermission, m.subscriptionPermissionVersion.Load()
}

func (m *SubscriptionPermissionManager) HasPermission(trackID livekit.TrackID, subIdentity livekit.ParticipantIdentity) bool {
	m.lock.RLock()
	defer m.lock.RUnlock()

	return m.hasPermissionLocked(trackID, subIdentity)
}

func (m *SubscriptionPermissionManager) GetAllowedSubscribers(trackID livekit.TrackID) []livekit.ParticipantIdentity {
	m.lock.RLock()
	defer m.lock.RUnlock()

	return m.getAllowedSubscribersLocked(trackID)
}

func (m *SubscriptionPermissionManager) isStaleSubscriptionPermissionLocked(timedVersion utils.TimedVersion) bool {
	return !timedVersion.IsZero() && !timedVersion.After(m.subscriptionPermissionVersion)
}

func (m *SubscriptionPermissionManager) logOlderSubscriptionPermissionLocked(subscriptionPermission *livekit.SubscriptionPermission, timedVersion utils.TimedVersion) {
	m.params.Logger.Debugw(
		"skipping older subscription permission version",
		"existingValue", logger.Proto(m.subscriptionPermission),
		"existingVersion", &m.subscriptionPermissionVersion,
		"requestingValue", logger.Proto(subscriptionPermission),
		"requestingVersion", &timedVersion,
	)
}

func (m *SubscriptionPermissionManager) hasPermissionLocked(trackID livekit.TrackID, subscriberIdentity livekit.ParticipantIdentity) bool {
	if m.subscriberPermissions == nil {
		return true
	}

	perms, ok := m.subscriberPermissions[subscriberIdentity]
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
func (m *SubscriptionPermissionManager) getAllowedSubscribersLocked(trackID livekit.TrackID) []livekit.ParticipantIdentity {
	if m.subscriberPermissions == nil {
		return nil
	}

	allowed := make([]livekit.ParticipantIdentity, 0)
	for subscriberIdentity, perms := range m.subscriberPermissions {
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

func (m *SubscriptionPermissionManager) maybeRevokeSubscriptionsLocked() {
	if m.subscriberPermissions == nil {
		return
	}

	for trackID, track := range m.tracks {
		allowed := m.getAllowedSubscribersLocked(trackID)
		if allowed == nil {
			// no restrictions
			continue
		}

		track.RevokeDisallowedSubscribers(allowed)
	}
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
