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
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

type ParticipantAsyncAttributesParams struct {
	Logger logger.Logger
}

type ParticipantAsyncAttributes struct {
	params     ParticipantAsyncAttributesParams
	lock       sync.Mutex
	attributes map[string]*livekit.DataTrackSchemaDefinition
}

func NewParticipantAsyncAttributes(params ParticipantAsyncAttributesParams) *ParticipantAsyncAttributes {
	return &ParticipantAsyncAttributes{
		params:     params,
		attributes: make(map[string]*livekit.DataTrackSchemaDefinition),
	}
}

func (p *ParticipantAsyncAttributes) Add(aa *livekit.DataTrackSchemaDefinition) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if aa.Id == nil {
		return
	}

	p.attributes[ToParticipantAsyncAttributeKey(aa.Id)] = utils.CloneProto(aa)
}

func (p *ParticipantAsyncAttributes) Delete(id *livekit.DataTrackSchemaId) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if id == nil {
		return
	}

	delete(p.attributes, ToParticipantAsyncAttributeKey(id))
}

func (p *ParticipantAsyncAttributes) Get(id *livekit.DataTrackSchemaId) *livekit.DataTrackSchemaDefinition {
	p.lock.Lock()
	defer p.lock.Unlock()

	if id == nil {
		return nil
	}

	aa, ok := p.attributes[ToParticipantAsyncAttributeKey(id)]
	if !ok {
		return nil
	}

	return aa
}

func (p *ParticipantAsyncAttributes) GetAll() []*livekit.DataTrackSchemaDefinition {
	p.lock.Lock()
	defer p.lock.Unlock()

	all := make([]*livekit.DataTrackSchemaDefinition, 0, len(p.attributes))
	for _, aa := range p.attributes {
		all = append(all, utils.CloneProto(aa))
	}
	return all
}

func (p *ParticipantAsyncAttributes) GetAllIDs() []*livekit.DataTrackSchemaId {
	p.lock.Lock()
	defer p.lock.Unlock()

	ids := make([]*livekit.DataTrackSchemaId, 0, len(p.attributes))
	for _, aa := range p.attributes {
		ids = append(ids, utils.CloneProto(aa.Id))
	}

	return ids
}

// -------------------------------

func ToParticipantAsyncAttributeKey(id *livekit.DataTrackSchemaId) string {
	return fmt.Sprintf("%s/%d", id.Name, id.Encoding)
}

func fromParticipantAsyncAttributeKey(key string) *livekit.DataTrackSchemaId {
	parts := strings.Split(key, "/")
	encoding, _ := strconv.Atoi(parts[1])
	return &livekit.DataTrackSchemaId{
		Name:     parts[0],
		Encoding: livekit.DataTrackSchemaEncoding(encoding),
	}
}
