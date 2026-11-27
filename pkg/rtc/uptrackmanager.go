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
	"maps"
	"slices"
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

type UpTrackManagerParams struct {
	Logger logger.Logger
}

// UpTrackManager manages all uptracks from a participant
type UpTrackManager struct {
	params UpTrackManagerParams

	closed bool

	// publishedTracks that participant is publishing
	publishedTracks map[livekit.TrackID]types.MediaTrack

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
		u.removePublishedTrackLocked(track)
		// not modifying subscription permissions, will get reset on next update from participant
		u.lock.Unlock()
	})
}

func (u *UpTrackManager) RemovePublishedTrack(track types.MediaTrack, isExpectedToResume bool) {
	track.Close(isExpectedToResume)

	u.lock.Lock()
	u.removePublishedTrackLocked(track)
	u.lock.Unlock()
}

func (u *UpTrackManager) removePublishedTrackLocked(track types.MediaTrack) {
	if trackID := track.ID(); u.publishedTracks[trackID] == track {
		delete(u.publishedTracks, trackID)
	}
}

func (u *UpTrackManager) getPublishedTrackLocked(trackID livekit.TrackID) types.MediaTrack {
	return u.publishedTracks[trackID]
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
