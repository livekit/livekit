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
	"math/rand"
	"time"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

type DataDownTrackParams struct {
	Logger           logger.Logger
	SubscriberID     livekit.ParticipantID
	PublishDataTrack types.DataTrack
}

type DataDownTrack struct {
	params    DataDownTrackParams
	dti       *livekit.DataTrackInfo
	handle    uint16
	createdAt int64
}

func NewDataDownTrack(params DataDownTrackParams, dti *livekit.DataTrackInfo) (*DataDownTrack, error) {
	d := &DataDownTrack{
		params:    params,
		dti:       dti,
		handle:    uint16(rand.Intn(256)),
		createdAt: time.Now().UnixNano(),
	}

	if err := d.params.PublishDataTrack.AddDataDownTrack(d); err != nil {
		d.params.Logger.Warnw("could not add data down track", err)
		return nil, err
	}

	return d, nil
}

func (d *DataDownTrack) Close() {
	d.params.Logger.Infow("closing data down track", "id", d.ID(), "name", d.Name())
	d.params.PublishDataTrack.DeleteDataDownTrack(d.SubscriberID())
}

func (d *DataDownTrack) Handle() uint16 {
	return d.handle
}

func (d *DataDownTrack) PublishDataTrack() types.DataTrack {
	return d.params.PublishDataTrack
}

func (d *DataDownTrack) PubHandle() uint16 {
	return uint16(d.dti.PubHandle)
}

func (d *DataDownTrack) ID() livekit.TrackID {
	return livekit.TrackID(d.dti.Sid)
}

func (d *DataDownTrack) Name() string {
	return d.dti.Name
}

func (d *DataDownTrack) SubscriberID() livekit.ParticipantID {
	// add `createdAt` to ensure repeated subscriptions from same subscriber to same publisher does not collide
	return livekit.ParticipantID(fmt.Sprintf("%s:%d", d.params.SubscriberID, d.createdAt))
}
