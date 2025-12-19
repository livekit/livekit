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
	"sync"

	"github.com/livekit/livekit-server/pkg/rtc/datatrack"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"golang.org/x/exp/maps"
)

type UpDataTrackManagerParams struct {
	Logger      logger.Logger
	Participant types.Participant
}

type UpDataTrackManager struct {
	params UpDataTrackManagerParams

	lock       sync.RWMutex
	dataTracks map[uint16]types.DataTrack

	onDataTrackPublished   func(types.Participant, types.DataTrack)
	onDataTrackUnpublished func(types.Participant, types.DataTrack)
}

func NewUpDataTrackManager(params UpDataTrackManagerParams) *UpDataTrackManager {
	return &UpDataTrackManager{
		params:     params,
		dataTracks: make(map[uint16]types.DataTrack),
	}
}

func (u *UpDataTrackManager) AddPublishedDataTrack(dt types.DataTrack) {
	u.lock.Lock()
	u.dataTracks[dt.PubHandle()] = dt
	u.lock.Unlock()

	u.params.Participant.GetParticipantListener().OnDataTrackPublished(u.params.Participant, dt)
}

func (u *UpDataTrackManager) RemovePublishedDataTrack(dt types.DataTrack) {
	var found bool
	pubHandle := dt.PubHandle()
	u.lock.Lock()
	if u.dataTracks[pubHandle] == dt {
		delete(u.dataTracks, pubHandle)
		found = true
	}
	u.lock.Unlock()

	if found {
		dt.Close()

		u.params.Participant.GetParticipantListener().OnDataTrackUnpublished(u.params.Participant, dt)
	}
}

func (u *UpDataTrackManager) GetPublishedDataTracks() []types.DataTrack {
	u.lock.RLock()
	defer u.lock.RUnlock()

	return maps.Values(u.dataTracks)
}

func (u *UpDataTrackManager) GetPublishedDataTrack(handle uint16) types.DataTrack {
	u.lock.RLock()
	defer u.lock.RUnlock()

	return u.dataTracks[handle]
}

func (u *UpDataTrackManager) HandleReceivedDataTrackMessage(data []byte, packet *datatrack.Packet, arrivalTime int64) {
	u.lock.RLock()
	dt := u.dataTracks[packet.Handle]
	u.lock.RUnlock()
	if dt == nil {
		return
	}

	dt.HandlePacket(data, packet, arrivalTime)
}

func (u *UpDataTrackManager) ToProto() []*livekit.DataTrackInfo {
	u.lock.RLock()
	defer u.lock.RUnlock()

	var dataTrackInfos []*livekit.DataTrackInfo
	for _, dt := range u.dataTracks {
		dataTrackInfos = append(dataTrackInfos, dt.ToProto())
	}

	return dataTrackInfos
}
