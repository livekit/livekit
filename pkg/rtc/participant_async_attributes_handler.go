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
	"github.com/livekit/protocol/utils"
)

func (p *ParticipantImpl) HandleDefineDataTrackSchemaRequest(req *livekit.DefineDataTrackSchemaRequest) {
	if !p.params.EnableParticipantAsyncAttributes {
		p.pubLogger.Warnw("async attributes not enabled", nil, "req", logger.Proto(req))
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason:  livekit.RequestResponse_NOT_ALLOWED,
			Message: "async attributes not enabled",
			Request: &livekit.RequestResponse_DefineDataTrackSchema{
				DefineDataTrackSchema: utils.CloneProto(req),
			},
		})
		return
	}

	if req.SchemaDefinition == nil || req.SchemaDefinition.Id == nil || len(req.SchemaDefinition.Id.Name) == 0 || !p.params.LimitConfig.CheckAsyncAttributeNameLength(req.SchemaDefinition.Id.Name) {
		p.pubLogger.Warnw("aync attribute definition is invalid", nil, "req", logger.Proto(req))
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason:  livekit.RequestResponse_INVALID_REQUEST,
			Message: "async attribute definition is invalid",
			Request: &livekit.RequestResponse_DefineDataTrackSchema{
				DefineDataTrackSchema: utils.CloneProto(req),
			},
		})
		return
	}

	if len(req.SchemaDefinition.Definition) == 0 {
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason:  livekit.RequestResponse_INVALID_REQUEST,
			Message: "async attribute definition is empty",
			Request: &livekit.RequestResponse_DefineDataTrackSchema{
				DefineDataTrackSchema: utils.CloneProto(req),
			},
		})
		return
	}

	if !p.params.LimitConfig.CanAddAsyncAttribute(p.asyncAttributes.GetAll(), req.SchemaDefinition) {
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason:  livekit.RequestResponse_LIMIT_EXCEEDED,
			Message: "async attribute definition exceeds limit",
			Request: &livekit.RequestResponse_DefineDataTrackSchema{
				DefineDataTrackSchema: utils.CloneProto(req),
			},
		})
		return
	}

	p.AddDataTrackSchema(req.SchemaDefinition)
	p.listener().OnDefineDataTrackSchema(p, req.SchemaDefinition)
}

func (p *ParticipantImpl) HandleGetDataTrackSchemaRequest(req *livekit.GetDataTrackSchemaRequest) {
	if req.SchemaId == nil {
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason:  livekit.RequestResponse_INVALID_REQUEST,
			Message: "async attribute id is required",
			Request: &livekit.RequestResponse_GetDataTrackSchema{
				GetDataTrackSchema: utils.CloneProto(req),
			},
		})
		return
	}

	p.listener().OnGetDataTrackSchema(p, req)
}

func (p *ParticipantImpl) AddDataTrackSchema(definition *livekit.DataTrackSchemaDefinition) {
	p.asyncAttributes.Add(definition)
}

func (p *ParticipantImpl) GetDataTrackSchema(id *livekit.DataTrackSchemaId) *livekit.DataTrackSchemaDefinition {
	return p.asyncAttributes.Get(id)
}

func (p *ParticipantImpl) ProcessGetDataTrackSchemaRequest(req *livekit.GetDataTrackSchemaRequest, publisher types.Participant) {
	if publisher == nil {
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason:  livekit.RequestResponse_NOT_FOUND,
			Message: "participant not found",
			Request: &livekit.RequestResponse_GetDataTrackSchema{
				GetDataTrackSchema: utils.CloneProto(req),
			},
		})
		return
	}

	asyncAttribute := publisher.GetDataTrackSchema(req.SchemaId)
	if asyncAttribute == nil {
		p.sendRequestResponse(&livekit.RequestResponse{
			Reason:  livekit.RequestResponse_NOT_FOUND,
			Message: "async attribute not found",
			Request: &livekit.RequestResponse_GetDataTrackSchema{
				GetDataTrackSchema: utils.CloneProto(req),
			},
		})
		return
	}

	p.sendGetDataTrackSchemaResponse(asyncAttribute)
}

func (p *ParticipantImpl) GetAllAsyncAttributes() []*livekit.DataTrackSchemaDefinition {
	return p.asyncAttributes.GetAll()
}
