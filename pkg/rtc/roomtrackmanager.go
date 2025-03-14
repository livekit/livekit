/*
 * Copyright 2023 LiveKit, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package rtc

import (
	"sync"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/livekit"
	"golang.org/x/exp/slices"
)

// RoomTrackManager holds tracks that are published to the room
type RoomTrackManager struct {
	lock            sync.RWMutex
	changedNotifier *utils.ChangeNotifierManager
	removedNotifier *utils.ChangeNotifierManager
	tracks          map[livekit.TrackID][]*TrackInfo
}

type TrackInfo struct {
	Track             types.MediaTrack
	PublisherIdentity livekit.ParticipantIdentity
	PublisherID       livekit.ParticipantID
}

func NewRoomTrackManager() *RoomTrackManager {
	return &RoomTrackManager{
		tracks:          make(map[livekit.TrackID][]*TrackInfo),
		changedNotifier: utils.NewChangeNotifierManager(),
		removedNotifier: utils.NewChangeNotifierManager(),
	}
}

func (r *RoomTrackManager) AddTrack(track types.MediaTrack, publisherIdentity livekit.ParticipantIdentity, publisherID livekit.ParticipantID) {
	trackID := track.ID()
	r.lock.Lock()
	r.tracks[trackID] = append(r.tracks[trackID], &TrackInfo{
		Track:             track,
		PublisherIdentity: publisherIdentity,
		PublisherID:       publisherID,
	})
	r.lock.Unlock()

	r.NotifyTrackChanged(trackID)
}

func (r *RoomTrackManager) RemoveTrack(track types.MediaTrack) {
	trackID := track.ID()
	r.lock.Lock()
	// ensure we are removing the same track as added
	infos, ok := r.tracks[trackID]
	if !ok {
		r.lock.Unlock()
		return
	}

	found := false
	for idx, info := range infos {
		if info.Track == track {
			r.tracks[trackID] = slices.Delete(r.tracks[trackID], idx, idx+1)
			if len(r.tracks[trackID]) == 0 {
				delete(r.tracks, trackID)
			}
			found = true
			break
		}
	}
	r.lock.Unlock()

	if !found {
		return
	}

	n := r.removedNotifier.GetNotifier(string(trackID))
	if n != nil {
		n.NotifyChanged()
	}

	r.changedNotifier.RemoveNotifier(string(trackID), true)
	r.removedNotifier.RemoveNotifier(string(trackID), true)
}

func (r *RoomTrackManager) GetTrackInfo(trackID livekit.TrackID) *TrackInfo {
	r.lock.RLock()
	defer r.lock.RUnlock()

	infos := r.tracks[trackID]
	if len(infos) == 0 {
		return nil
	}

	// earliest added track is used till it is removed
	info := infos[0]

	// when track is about to close, do not resolve
	if info.Track != nil && !info.Track.IsOpen() {
		return nil
	}
	return info
}

func (r *RoomTrackManager) NotifyTrackChanged(trackID livekit.TrackID) {
	n := r.changedNotifier.GetNotifier(string(trackID))
	if n != nil {
		n.NotifyChanged()
	}
}

// HasObservers lets caller know if the current media track has any observers
// this is used to signal interest in the track. when another MediaTrack with the same
// trackID is being used, track is not considered to be observed.
func (r *RoomTrackManager) HasObservers(track types.MediaTrack) bool {
	n := r.changedNotifier.GetNotifier(string(track.ID()))
	if n == nil || !n.HasObservers() {
		return false
	}

	info := r.GetTrackInfo(track.ID())
	if info == nil || info.Track != track {
		return false
	}
	return true
}

func (r *RoomTrackManager) GetOrCreateTrackChangeNotifier(trackID livekit.TrackID) *utils.ChangeNotifier {
	return r.changedNotifier.GetOrCreateNotifier(string(trackID))
}

func (r *RoomTrackManager) GetOrCreateTrackRemoveNotifier(trackID livekit.TrackID) *utils.ChangeNotifier {
	return r.removedNotifier.GetOrCreateNotifier(string(trackID))
}
