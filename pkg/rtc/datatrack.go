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
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

// DataTrack represents a data stream identified by a handle ID.
type DataTrack struct {
	params DataTrackParams

	// DT-TODO - lock
	dti *livekit.DataTrackInfo
}

type DataTrackParams struct {
	Logger logger.Logger
}

func NewDataTrack(params DataTrackParams, dti *livekit.DataTrackInfo) *DataTrack {
	return &DataTrack{
		params: params,
	}
}

func (d *DataTrack) Close() {
	d.params.Logger.Infow("closing data track", "id", d.ID(), "name", d.Name())
	// DT-TODO: anything else?
}

func (d *DataTrack) ToProto() *livekit.DataTrackInfo {
	return utils.CloneProto(d.dti)
}

func (d *DataTrack) PubHandle() uint16 {
	return uint16(d.dti.PubHandle)
}

func (d *DataTrack) ID() livekit.DataTrackID {
	return livekit.DataTrackID(d.dti.Sid)
}

func (d *DataTrack) Name() string {
	return d.dti.Name
}

func (d *DataTrack) OnMessage(data []byte) {
	// DT-TODO d.params.Logger.Infow("received data track message", "id", d.ID(), "name", d.Name(), "size", len(data))
}
