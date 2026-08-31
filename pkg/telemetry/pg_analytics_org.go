// Copyright 2026 Hideout
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

// Fork addition: resolves the two things a byte sample needs beyond what
// AnalyticsStat itself carries - the organization it is billed to, and the kind of
// participant that produced it.
//
// Neither is on AnalyticsStat: the SFU only ever sees room and participant ids
// there. The organization reaches the server on the LiveKit access token apps/api
// mints (a participant attribute, signed with the LiveKit API secret, so it is
// server-issued rather than client-supplied); the kind is a property of the
// participant itself, which LiveKit already tracks for every connection
// (STANDARD, EGRESS, INGRESS, AGENT, SIP, ...). Both arrive as ParticipantInfo on
// the participant lifecycle events the sink already receives, so this keeps a
// small in-memory index built from them and stamps every row at flush time,
// without a database lookup and without touching the media path.

package telemetry

import (
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
)

const (
	// orgLingerAfterLeave keeps an entry alive past PARTICIPANT_LEFT. A
	// participant's last stats flush arrives after they leave - telemetry flushes on
	// a 30s ticker and holds closed workers for a further cleanup wait - so evicting
	// on the event itself would leave their final rows unattributed.
	orgLingerAfterLeave = 5 * time.Minute

	// orgMaxIdle bounds the index when PARTICIPANT_LEFT never arrives, which happens
	// if a participant is lost without a clean teardown. Entries are touched on
	// every lookup, so a participant still moving bytes is never evicted by it.
	orgMaxIdle = 24 * time.Hour
)

// orgEntry is one participant's organization and kind, plus the bookkeeping that
// decides when it can be forgotten.
type orgEntry struct {
	orgID string

	// attributed records that a lifecycle event actually carried the organization
	// attribute, whatever its value. It separates "this participant's token said
	// they belong to no organization" - a guest, which is expected - from "no
	// organization ever reached us", which means a token, a deploy or this index is
	// broken. Both leave orgID empty; only the second is worth paging someone about.
	attributed bool

	// kind is the participant's role (standard, egress, ingress, agent, ...). Unlike
	// orgID it cannot change mid-session and is never absent from a real
	// ParticipantInfo, so it is simply overwritten on every observation rather than
	// tracked with remember's "do not clobber with an unset value" rule.
	kind      livekit.ParticipantInfo_Kind
	kindKnown bool

	lastSeen time.Time

	// leftAt is zero while the participant is in a room. Once set, the entry is
	// dropped by the first sweep after the linger window.
	leftAt time.Time
}

// orgResolver indexes participant id -> organization id. It is written from the
// telemetry event queue and read from the stats flush loop, which are different
// goroutines, so every access takes the lock.
type orgResolver struct {
	attributeKey string

	lock    sync.Mutex
	entries map[livekit.ParticipantID]*orgEntry
}

func newOrgResolver(attributeKey string) *orgResolver {
	return &orgResolver{
		attributeKey: attributeKey,
		entries:      make(map[livekit.ParticipantID]*orgEntry),
	}
}

// observe records what an analytics event says about a participant. Only the
// participant lifecycle events carry a ParticipantInfo worth reading; the rest are
// ignored.
func (r *orgResolver) observe(event *livekit.AnalyticsEvent, now time.Time) {
	participant := event.GetParticipant()
	participantID := livekit.ParticipantID(participant.GetSid())
	if participantID == "" {
		return
	}

	switch event.GetType() {
	case livekit.AnalyticsEventType_PARTICIPANT_JOINED,
		livekit.AnalyticsEventType_PARTICIPANT_ACTIVE,
		livekit.AnalyticsEventType_PARTICIPANT_RESUMED:
		orgID, attributed := participant.GetAttributes()[r.attributeKey]
		r.remember(participantID, orgID, attributed, participant.GetKind(), now)

	case livekit.AnalyticsEventType_PARTICIPANT_LEFT:
		r.markLeft(participantID, now)
	}
}

// remember indexes a participant, or refreshes one already known. attributed says
// whether this event carried the organization attribute at all, which is what
// separates a guest from a participant nobody told us about.
//
// An event that did not carry the attribute never clears a value an earlier event
// supplied: several code paths hand telemetry a trimmed ParticipantInfo with the
// attributes stripped, and treating that as "this participant has no organization"
// would silently unattribute their bytes. An event that did carry it always wins,
// so an organization changed mid-session is picked up.
func (r *orgResolver) remember(
	participantID livekit.ParticipantID,
	orgID string,
	attributed bool,
	kind livekit.ParticipantInfo_Kind,
	now time.Time,
) {
	r.lock.Lock()
	defer r.lock.Unlock()

	entry, ok := r.entries[participantID]
	if !ok {
		// a real ParticipantInfo always carries a kind (STANDARD is participant
		// zero, not "absent"), so any lifecycle event is enough to start an entry
		// for it, even one whose organization attribute is missing
		entry = &orgEntry{}
		r.entries[participantID] = entry
	}

	if attributed {
		entry.orgID = orgID
		entry.attributed = true
	}
	// kind cannot change mid-session, so it is always safe to overwrite - there is
	// no "later event omitted it" case to guard against the way there is for orgID
	entry.kind = kind
	entry.kindKnown = true
	entry.lastSeen = now
	// a resume after a signalling drop reuses the participant id, so an entry that
	// was counting down to eviction becomes live again
	entry.leftAt = time.Time{}
}

// markLeft starts the linger countdown rather than evicting, so that the stats
// still in flight for this participant can be attributed.
func (r *orgResolver) markLeft(participantID livekit.ParticipantID, now time.Time) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if entry, ok := r.entries[participantID]; ok {
		entry.leftAt = now
	}
}

// resolvedParticipant is what a lookup answers: everything a sample needs about
// the participant that produced it, beyond what AnalyticsStat itself carries.
type resolvedParticipant struct {
	// OrgID and Attributed follow the same rule as remember: an empty OrgID with
	// Attributed true is a guest (the token carried the attribute, empty on
	// purpose); Attributed false means no organization ever reached the sink for
	// this participant at all, which is worth alerting on.
	OrgID      string
	Attributed bool

	// Kind and KindKnown are the participant's role. KindKnown false means the
	// participant was never observed - a zero-value Kind would otherwise be
	// indistinguishable from a genuine STANDARD participant, since that is kind's
	// own zero value.
	Kind      livekit.ParticipantInfo_Kind
	KindKnown bool
}

// resolve returns what is known about a participant. A lookup counts as activity:
// a participant still moving bytes is never swept, even if their PARTICIPANT_LEFT
// was lost.
func (r *orgResolver) resolve(participantID livekit.ParticipantID, now time.Time) resolvedParticipant {
	if participantID == "" {
		return resolvedParticipant{}
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	entry, ok := r.entries[participantID]
	if !ok {
		return resolvedParticipant{}
	}

	entry.lastSeen = now
	return resolvedParticipant{
		OrgID:      entry.orgID,
		Attributed: entry.attributed,
		Kind:       entry.kind,
		KindKnown:  entry.kindKnown,
	}
}

// sweep drops entries for participants that left longer ago than the linger window,
// and any that have been quiet long enough that their teardown was clearly missed.
// It returns how many entries were dropped.
func (r *orgResolver) sweep(now time.Time) int {
	r.lock.Lock()
	defer r.lock.Unlock()

	dropped := 0
	for participantID, entry := range r.entries {
		left := !entry.leftAt.IsZero() && now.Sub(entry.leftAt) > orgLingerAfterLeave
		if left || now.Sub(entry.lastSeen) > orgMaxIdle {
			delete(r.entries, participantID)
			dropped++
		}
	}
	return dropped
}

// size reports how many participants are indexed, for the gauge that makes a leak
// visible.
func (r *orgResolver) size() int {
	r.lock.Lock()
	defer r.lock.Unlock()

	return len(r.entries)
}
