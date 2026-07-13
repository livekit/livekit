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
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

type ParticipantDataBlobParams struct {
	Logger logger.Logger
}

type ParticipantDataBlob struct {
	params ParticipantDataBlobParams
	lock   sync.Mutex
	blobs  map[string]*livekit.DataBlob
}

func NewParticipantDataBlob(params ParticipantDataBlobParams) *ParticipantDataBlob {
	return &ParticipantDataBlob{
		params: params,
		blobs:  make(map[string]*livekit.DataBlob),
	}
}

func (p *ParticipantDataBlob) Add(db *livekit.DataBlob) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if db.Key == nil {
		return
	}

	p.blobs[db.Key.String()] = db
}

func (p *ParticipantDataBlob) Delete(dbKey *livekit.DataBlobKey) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if dbKey == nil {
		return
	}

	delete(p.blobs, dbKey.String())
}

func (p *ParticipantDataBlob) Get(dbKey *livekit.DataBlobKey) *livekit.DataBlob {
	p.lock.Lock()
	defer p.lock.Unlock()

	if dbKey == nil {
		return nil
	}

	db, ok := p.blobs[dbKey.String()]
	if !ok {
		return nil
	}

	return db
}

func (p *ParticipantDataBlob) GetAll() []*livekit.DataBlob {
	p.lock.Lock()
	defer p.lock.Unlock()

	all := make([]*livekit.DataBlob, 0, len(p.blobs))
	for _, db := range p.blobs {
		all = append(all, db)
	}
	return all
}

// -------------------------------
