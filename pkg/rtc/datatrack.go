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
	"sync"

	"github.com/frostbyte73/core"
	"github.com/livekit/livekit-server/pkg/rtc/datatrack"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	sfuutils "github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

var (
	errReceiverClosed = errors.New("datatrack is closed")
)

var _ types.DataTrack = (*DataTrack)(nil)

type DataTrackParams struct {
	Logger              logger.Logger
	ParticipantID       func() livekit.ParticipantID
	ParticipantIdentity livekit.ParticipantIdentity
}

type DataTrack struct {
	params DataTrackParams

	lock             sync.Mutex
	dti              *livekit.DataTrackInfo
	subscribedTracks map[livekit.ParticipantID]types.DataDownTrack

	downTrackSpreader *sfuutils.DownTrackSpreader[types.DataTrackSender]

	closed core.Fuse
}

func NewDataTrack(params DataTrackParams, dti *livekit.DataTrackInfo) *DataTrack {
	d := &DataTrack{
		params:           params,
		dti:              dti,
		subscribedTracks: make(map[livekit.ParticipantID]types.DataDownTrack),
		downTrackSpreader: sfuutils.NewDownTrackSpreader[types.DataTrackSender](sfuutils.DownTrackSpreaderParams{
			Threshold: 20,
			Logger:    params.Logger,
		}),
	}
	d.params.Logger.Infow("created data track", "name", d.Name())
	return d
}

func (d *DataTrack) Close() {
	d.params.Logger.Infow("closing data track", "name", d.Name())
	d.closed.Break()
}

func (d *DataTrack) PublisherID() livekit.ParticipantID {
	return d.params.ParticipantID()
}

func (d *DataTrack) PublisherIdentity() livekit.ParticipantIdentity {
	return d.params.ParticipantIdentity
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

func (d *DataTrack) AddSubscriber(sub types.LocalParticipant) (types.DataDownTrack, error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	if _, ok := d.subscribedTracks[sub.ID()]; ok {
		return nil, errAlreadySubscribed
	}

	dataDownTrack, err := NewDataDownTrack(DataDownTrackParams{
		Logger:           sub.GetLogger().WithValues("trackID", d.ID()),
		SubscriberID:     sub.ID(),
		PublishDataTrack: d,
		Handle:           sub.GetNextSubscribedDataTrackHandle(),
		Transport:        sub.GetDataTrackTransport(),
	})
	if err != nil {
		return nil, err
	}

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

func (d *DataTrack) AddDataDownTrack(dts types.DataTrackSender) error {
	if d.closed.IsBroken() {
		return errReceiverClosed
	}

	if d.downTrackSpreader.HasDownTrack(dts.SubscriberID()) {
		d.params.Logger.Infow("subscriberID already exists, replacing data downtrack", "subscriberID", dts.SubscriberID())
	}

	d.downTrackSpreader.Store(dts)
	d.params.Logger.Infow("data downtrack added", "subscriberID", dts.SubscriberID())
	return nil
}

func (d *DataTrack) DeleteDataDownTrack(subscriberID livekit.ParticipantID) {
	d.downTrackSpreader.Free(subscriberID)
	d.params.Logger.Infow("data downtrack deleted", "subscriberID", subscriberID)
}

func (d *DataTrack) HandlePacket(data []byte, packet *datatrack.Packet, arrivalTime int64) {
	d.downTrackSpreader.Broadcast(func(dts types.DataTrackSender) {
		dts.WritePacket(data, packet, arrivalTime)
	})
}
