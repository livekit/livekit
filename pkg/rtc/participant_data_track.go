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
	"github.com/livekit/livekit-server/pkg/rtc/datatrack"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"
)

func (p *ParticipantImpl) HandlePublishDataTrackRequest(req *livekit.PublishDataTrackRequest) {
	if !p.CanPublishData() || !p.params.EnableDataTracks {
		p.pubLogger.Warnw("no permission to publish data track", nil, "req", logger.Proto(req))
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason:  livekit.RequestResponse_NOT_ALLOWED,
			Message: "does not have permission to publish data",
			Request: &livekit.RequestResponse_PublishDataTrack{
				PublishDataTrack: utils.CloneProto(req),
			},
		})
		return
	}

	if req.PubHandle == 0 || req.PubHandle > 65535 {
		p.pubLogger.Warnw("invalid data track handle", nil, "req", logger.Proto(req))
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason:  livekit.RequestResponse_INVALID_HANDLE,
			Message: "handle should be > 0 AND < 65536",
			Request: &livekit.RequestResponse_PublishDataTrack{
				PublishDataTrack: utils.CloneProto(req),
			},
		})
		return
	}

	if len(req.Name) == 0 || len(req.Name) > 256 {
		p.pubLogger.Warnw("invalid data track name", nil, "req", logger.Proto(req))
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason:  livekit.RequestResponse_INVALID_NAME,
			Message: "name should not be empty and should not exceed 256 characters",
			Request: &livekit.RequestResponse_PublishDataTrack{
				PublishDataTrack: utils.CloneProto(req),
			},
		})
		return
	}

	publishedDataTracks := p.UpDataTrackManager.GetPublishedDataTracks()
	for _, dt := range publishedDataTracks {
		message := ""
		reason := livekit.RequestResponse_OK
		switch {
		case dt.PubHandle() == uint16(req.PubHandle):
			message = "a data track with same handle already exists"
			reason = livekit.RequestResponse_DUPLICATE_HANDLE
		case dt.Name() == req.Name:
			message = "a data track with same name already exists"
			reason = livekit.RequestResponse_DUPLICATE_NAME
		}
		if message != "" {
			p.pubLogger.Warnw(
				"cannot publish duplicate data track", nil,
				"req", logger.Proto(req),
				"existing", logger.Proto(dt.ToProto()),
			)
			p.sendRequestResponse(&livekit.RequestResponse{
				Reason:  reason,
				Message: message,
				Request: &livekit.RequestResponse_PublishDataTrack{
					PublishDataTrack: utils.CloneProto(req),
				},
			})
			return
		}
	}

	dti := &livekit.DataTrackInfo{
		PubHandle:  req.PubHandle,
		Sid:        guid.New(utils.DataTrackPrefix),
		Name:       req.Name,
		Encryption: req.Encryption,
	}
	dt := NewDataTrack(
		DataTrackParams{
			Logger:              p.params.Logger.WithValues("trackID", dti.Sid),
			ParticipantID:       p.ID,
			ParticipantIdentity: p.params.Identity,
		},
		dti,
	)

	p.UpDataTrackManager.AddPublishedDataTrack(dt)

	p.sendPublishDataTrackResponse(dti)

	p.setIsPublisher(true)
	p.dirty.Store(true)
}

func (p *ParticipantImpl) HandleUnpublishDataTrackRequest(req *livekit.UnpublishDataTrackRequest) {
	dt := p.UpDataTrackManager.GetPublishedDataTrack(uint16(req.PubHandle))
	if dt == nil {
		p.pubLogger.Warnw("unpublish data track not found", nil, "req", logger.Proto(req))
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason: livekit.RequestResponse_NOT_FOUND,
			Request: &livekit.RequestResponse_UnpublishDataTrack{
				UnpublishDataTrack: utils.CloneProto(req),
			},
		})
		return
	}

	p.UpDataTrackManager.RemovePublishedDataTrack(dt)

	p.sendUnpublishDataTrackResponse(dt.ToProto())

	p.dirty.Store(true)
}

func (p *ParticipantImpl) HandleUpdateDataSubscription(req *livekit.UpdateDataSubscription) {
	p.listener().OnUpdateDataSubscriptions(p, req)
}

func (p *ParticipantImpl) onReceivedDataTrackMessage(data []byte, arrivalTime int64) {
	var packet datatrack.Packet
	if err := packet.Unmarshal(data); err != nil {
		p.params.Logger.Errorw("could not unmarshal data track message", err)
		return
	}

	p.UpDataTrackManager.HandleReceivedDataTrackMessage(data, &packet, arrivalTime)

	p.listener().OnDataTrackMessage(p, data, &packet)
}

func (p *ParticipantImpl) GetNextSubscribedDataTrackHandle() uint16 {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.nextSubscribedDataTrackHandle++
	if p.nextSubscribedDataTrackHandle == 0 {
		p.nextSubscribedDataTrackHandle++
	}

	return p.nextSubscribedDataTrackHandle
}
