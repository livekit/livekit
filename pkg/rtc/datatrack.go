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

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

type DataTrackParams struct {
	Logger logger.Logger
}

type DataTrack struct {
	params DataTrackParams

	lock             sync.Mutex
	dti              *livekit.DataTrackInfo
	subscribedTracks map[livekit.ParticipantID]*DataDownTrack
}

func NewDataTrack(params DataTrackParams, dti *livekit.DataTrackInfo) *DataTrack {
	return &DataTrack{
		params:           params,
		dti:              dti,
		subscribedTracks: make(map[livekit.ParticipantID]*DataDownTrack),
	}
}

func (d *DataTrack) Close() {
	d.params.Logger.Infow("closing data track", "id", d.ID(), "name", d.Name())
}

func (d *DataTrack) ToProto() *livekit.DataTrackInfo {
	return utils.CloneProto(d.dti)
}

func (d *DataTrack) PubHandle() uint16 {
	return uint16(d.dti.PubHandle)
}

func (d *DataTrack) ID() livekit.TrackID {
	return livekit.TrackID(d.dti.Sid)
}

func (d *DataTrack) Name() string {
	return d.dti.Name
}

func (d *DataTrack) OnMessage(data []byte) {
	d.params.Logger.Infow("received data track message", "id", d.ID(), "name", d.Name(), "size", len(data))
	// DT-TODO
}

func (d *DataTrack) AddSubscriber(sub types.LocalParticipant) (types.DataDownTrack, error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	if _, ok := d.subscribedTracks[sub.ID()]; ok {
		return nil, errAlreadySubscribed
	}

	dataDownTrack := NewDataDownTrack(
		DataDownTrackParams{
			Logger:           d.params.Logger,
			SubscriberID:     sub.ID(),
			PublishDataTrack: d,
		},
		d.dti,
	)
	d.subscribedTracks[sub.ID()] = dataDownTrack
	return dataDownTrack, nil
}

func (d *DataTrack) RemoveSubscriber(subID livekit.ParticipantID) {
	d.lock.Lock()
	dataDownTrack, ok := d.subscribedTracks[subID]
	delete(d.subscribedTracks, subID)
	d.lock.Unlock()

	if ok {
		dataDownTrack.Close()
	}
}

func (d *DataTrack) IsSubscriber(subID livekit.ParticipantID) bool {
	d.lock.Lock()
	defer d.lock.Unlock()

	_, ok := d.subscribedTracks[subID]
	return ok
}
