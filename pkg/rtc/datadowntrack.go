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
	"fmt"
	"time"

	"github.com/livekit/livekit-server/pkg/rtc/datatrack"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

var _ types.DataDownTrack = (*DataDownTrack)(nil)
var _ types.DataTrackSender = (*DataDownTrack)(nil)

type DataDownTrackParams struct {
	Logger           logger.Logger
	SubscriberID     livekit.ParticipantID
	PublishDataTrack types.DataTrack
	Handle           uint16
	Transport        types.DataTrackTransport
}

type DataDownTrack struct {
	params    DataDownTrackParams
	createdAt int64
}

func NewDataDownTrack(params DataDownTrackParams) (*DataDownTrack, error) {
	d := &DataDownTrack{
		params:    params,
		createdAt: time.Now().UnixNano(),
	}

	if err := d.params.PublishDataTrack.AddDataDownTrack(d); err != nil {
		d.params.Logger.Warnw("could not add data down track", err)
		return nil, err
	}

	d.params.Logger.Infow("created data down track", "name", d.Name())
	return d, nil
}

func (d *DataDownTrack) Close() {
	d.params.Logger.Infow("closing data down track", "name", d.Name())
	d.params.PublishDataTrack.DeleteDataDownTrack(d.SubscriberID())
}

func (d *DataDownTrack) Handle() uint16 {
	return d.params.Handle
}

func (d *DataDownTrack) PublishDataTrack() types.DataTrack {
	return d.params.PublishDataTrack
}

func (d *DataDownTrack) ID() livekit.TrackID {
	return d.params.PublishDataTrack.ID()
}

func (d *DataDownTrack) Name() string {
	return d.params.PublishDataTrack.Name()
}

func (d *DataDownTrack) SubscriberID() livekit.ParticipantID {
	// add `createdAt` to ensure repeated subscriptions from same subscriber to same publisher does not collide
	return livekit.ParticipantID(fmt.Sprintf("%s:%d", d.params.SubscriberID, d.createdAt))
}

func (d *DataDownTrack) WritePacket(data []byte, packet *datatrack.Packet, _arrivalTime int64) {
	forwardedPacket := *packet
	forwardedPacket.Handle = d.params.Handle
	buf, err := forwardedPacket.Marshal()
	if err != nil {
		d.params.Logger.Warnw("could not marshal data track message", err)
		return
	}
	if err := d.params.Transport.SendDataTrackMessage(buf); err != nil {
		d.params.Logger.Warnw("could not send data track message", err, "handle", d.params.Handle)
	}
}

func (d *DataDownTrack) UpdateSubscriptionOptions(subscriptionOptions *livekit.DataTrackSubscriptionOptions) {
	// DT-TODO
}
