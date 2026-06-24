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
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

func (p *ParticipantImpl) HandleStoreDataBlobRequest(req *livekit.StoreDataBlobRequest) {
	if !p.params.EnableParticipantDataBlob {
		p.pubLogger.Warnw("data blob not enabled", nil, "req", logger.Proto(req))
		p.sendRequestResponse(&livekit.RequestResponse{
			RequestId: req.RequestId,
			Reason:    livekit.RequestResponse_NOT_ALLOWED,
			Message:   "data blob not enabled",
		})
		return
	}

	if req.Blob == nil || req.Blob.Key == nil || len(req.Blob.Key.String()) == 0 || !p.params.LimitConfig.CheckDataBlobKeyLength(req.Blob.Key.String()) {
		p.pubLogger.Warnw("data blob is invalid", nil, "req", logger.Proto(req))
		p.sendRequestResponse(&livekit.RequestResponse{
			RequestId: req.RequestId,
			Reason:    livekit.RequestResponse_INVALID_REQUEST,
			Message:   "data blob is invalid",
		})
		return
	}

	if !p.params.LimitConfig.CheckDataTrackSchemaID(req.Blob.Key.GetSchemaId()) {
		p.pubLogger.Warnw("data blob key schema id is invalid", nil, "req", logger.Proto(req))
		p.sendRequestResponse(&livekit.RequestResponse{
			RequestId: req.RequestId,
			Reason:    livekit.RequestResponse_INVALID_REQUEST,
			Message:   "custom encoding identifier is empty or exceeds the maximum length",
		})
		return
	}

	if len(req.Blob.Contents) == 0 {
		p.sendRequestResponse(&livekit.RequestResponse{
			RequestId: req.RequestId,
			Reason:    livekit.RequestResponse_INVALID_REQUEST,
			Message:   "data blob is empty",
		})
		return
	}

	if !p.params.LimitConfig.CanAddDataBlob(p.dataBlob.GetAll(), req.Blob) {
		p.sendRequestResponse(&livekit.RequestResponse{
			RequestId: req.RequestId,
			Reason:    livekit.RequestResponse_LIMIT_EXCEEDED,
			Message:   "async attribute definition exceeds limit",
		})
		return
	}

	p.AddDataBlob(req.Blob)
	p.listener().OnStoreDataBlob(p, req.Blob)
	p.sendStoreDataBlobResponse(req.RequestId, req.Blob.Key)
}

func (p *ParticipantImpl) HandleGetDataBlobRequest(req *livekit.GetDataBlobRequest) {
	if req.Key == nil {
		p.sendRequestResponse(&livekit.RequestResponse{
			RequestId: req.RequestId,
			Reason:    livekit.RequestResponse_INVALID_REQUEST,
			Message:   "data blob key is required",
		})
		return
	}

	p.listener().OnGetDataBlob(p, req)
}

func (p *ParticipantImpl) AddDataBlob(dataBlob *livekit.DataBlob) {
	p.dataBlob.Add(dataBlob)
}

func (p *ParticipantImpl) GetDataBlob(key *livekit.DataBlobKey) *livekit.DataBlob {
	return p.dataBlob.Get(key)
}

func (p *ParticipantImpl) ProcessGetDataBlobRequest(req *livekit.GetDataBlobRequest, publisher types.Participant) {
	if publisher == nil {
		p.sendRequestResponse(&livekit.RequestResponse{
			RequestId: req.RequestId,
			Reason:    livekit.RequestResponse_NOT_FOUND,
			Message:   "participant not found",
		})
		return
	}

	dataBlob := publisher.GetDataBlob(req.Key)
	if dataBlob == nil {
		p.sendRequestResponse(&livekit.RequestResponse{
			RequestId: req.RequestId,
			Reason:    livekit.RequestResponse_NOT_FOUND,
			Message:   "data blob not found",
		})
		return
	}

	p.sendGetDataBlobResponse(req.RequestId, dataBlob)
}

func (p *ParticipantImpl) GetAllDataBlob() []*livekit.DataBlob {
	return p.dataBlob.GetAll()
}
