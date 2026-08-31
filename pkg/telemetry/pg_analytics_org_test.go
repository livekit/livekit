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

package telemetry

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
)

func participantEvent(
	eventType livekit.AnalyticsEventType,
	participantID string,
	attributes map[string]string,
) *livekit.AnalyticsEvent {
	return participantEventKind(eventType, participantID, attributes, livekit.ParticipantInfo_STANDARD)
}

func participantEventKind(
	eventType livekit.AnalyticsEventType,
	participantID string,
	attributes map[string]string,
	kind livekit.ParticipantInfo_Kind,
) *livekit.AnalyticsEvent {
	return &livekit.AnalyticsEvent{
		Type: eventType,
		Participant: &livekit.ParticipantInfo{
			Sid:        participantID,
			Attributes: attributes,
			Kind:       kind,
		},
	}
}

func orgAttribute(orgID string) map[string]string {
	return map[string]string{config.DefaultAnalyticsOrgAttributeKey: orgID}
}

func newTestOrgResolver() *orgResolver {
	return newOrgResolver(config.DefaultAnalyticsOrgAttributeKey)
}

func TestOrgResolverReadsTheTokenAttribute(t *testing.T) {
	now := time.Now()
	r := newTestOrgResolver()

	r.observe(participantEvent(
		livekit.AnalyticsEventType_PARTICIPANT_JOINED, "PA_1", orgAttribute("org_01HQZX"),
	), now)

	resolved := r.resolve("PA_1", now)
	require.Equal(t, "org_01HQZX", resolved.OrgID)
	require.True(t, resolved.Attributed)
}

func TestOrgResolverUsesTheConfiguredAttributeKey(t *testing.T) {
	now := time.Now()
	r := newOrgResolver("tenant")

	r.observe(participantEvent(
		livekit.AnalyticsEventType_PARTICIPANT_JOINED, "PA_1",
		map[string]string{"tenant": "org_01HQZX", "orgId": "wrong"},
	), now)

	resolved := r.resolve("PA_1", now)
	require.Equal(t, "org_01HQZX", resolved.OrgID)
}

// A guest's token carries the attribute with an empty value. That is a deliberate
// "no organization", and must stay distinguishable from a participant whose token
// never carried the attribute - only the latter is a misconfiguration.
func TestOrgResolverSeparatesGuestFromMissingAttribute(t *testing.T) {
	now := time.Now()
	r := newTestOrgResolver()

	r.observe(participantEvent(
		livekit.AnalyticsEventType_PARTICIPANT_JOINED, "PA_guest", orgAttribute(""),
	), now)
	r.observe(participantEvent(
		livekit.AnalyticsEventType_PARTICIPANT_JOINED, "PA_no_attr", map[string]string{"other": "x"},
	), now)

	guest := r.resolve("PA_guest", now)
	require.Empty(t, guest.OrgID)
	require.True(t, guest.Attributed, "a guest's token did carry the attribute")

	noAttr := r.resolve("PA_no_attr", now)
	require.Empty(t, noAttr.OrgID)
	require.False(t, noAttr.Attributed)
}

func TestOrgResolverIgnoresUnknownParticipants(t *testing.T) {
	r := newTestOrgResolver()

	resolved := r.resolve("PA_never_seen", time.Now())
	require.Empty(t, resolved.OrgID)
	require.False(t, resolved.Attributed)
	require.False(t, resolved.KindKnown)

	resolved = r.resolve("", time.Now())
	require.Empty(t, resolved.OrgID)
	require.False(t, resolved.Attributed)
}

func TestOrgResolverIgnoresEventsWithoutAParticipant(t *testing.T) {
	now := time.Now()
	r := newTestOrgResolver()

	r.observe(&livekit.AnalyticsEvent{Type: livekit.AnalyticsEventType_ROOM_CREATED}, now)
	r.observe(participantEvent(livekit.AnalyticsEventType_PARTICIPANT_JOINED, "", orgAttribute("org_1")), now)

	require.Zero(t, r.size())
}

// Several code paths hand telemetry a ParticipantInfo trimmed down to sid, identity
// and kind. Those events must not erase an organization an earlier event supplied,
// or the participant's bytes silently stop being billable.
func TestOrgResolverKeepsOrgWhenALaterEventOmitsAttributes(t *testing.T) {
	now := time.Now()
	r := newTestOrgResolver()

	r.observe(participantEvent(
		livekit.AnalyticsEventType_PARTICIPANT_JOINED, "PA_1", orgAttribute("org_01HQZX"),
	), now)
	r.observe(participantEvent(
		livekit.AnalyticsEventType_PARTICIPANT_ACTIVE, "PA_1", nil,
	), now)

	resolved := r.resolve("PA_1", now)
	require.Equal(t, "org_01HQZX", resolved.OrgID)
	require.True(t, resolved.Attributed)
}

func TestOrgResolverTakesTheLatestAttributedValue(t *testing.T) {
	now := time.Now()
	r := newTestOrgResolver()

	r.observe(participantEvent(
		livekit.AnalyticsEventType_PARTICIPANT_JOINED, "PA_1", orgAttribute("org_old"),
	), now)
	r.observe(participantEvent(
		livekit.AnalyticsEventType_PARTICIPANT_ACTIVE, "PA_1", orgAttribute("org_new"),
	), now)

	resolved := r.resolve("PA_1", now)
	require.Equal(t, "org_new", resolved.OrgID)
}

// The final stats flush for a participant arrives after they have left, so an entry
// has to outlive PARTICIPANT_LEFT or those last rows lose their organization.
func TestOrgResolverKeepsEntryThroughTheLingerWindow(t *testing.T) {
	now := time.Now()
	r := newTestOrgResolver()

	r.observe(participantEvent(
		livekit.AnalyticsEventType_PARTICIPANT_JOINED, "PA_1", orgAttribute("org_01HQZX"),
	), now)
	r.observe(participantEvent(livekit.AnalyticsEventType_PARTICIPANT_LEFT, "PA_1", nil), now)

	stillLingering := now.Add(orgLingerAfterLeave - time.Second)
	require.Zero(t, r.sweep(stillLingering))
	resolved := r.resolve("PA_1", stillLingering)
	require.Equal(t, "org_01HQZX", resolved.OrgID)

	expired := now.Add(orgLingerAfterLeave + time.Second)
	require.Equal(t, 1, r.sweep(expired))
	resolved = r.resolve("PA_1", expired)
	require.Empty(t, resolved.OrgID)
	require.False(t, resolved.Attributed)
}

// A resume reuses the participant id after the signalling collector reported a
// leave, so the entry counting down to eviction has to come back to life.
func TestOrgResolverResumeCancelsEviction(t *testing.T) {
	now := time.Now()
	r := newTestOrgResolver()

	r.observe(participantEvent(
		livekit.AnalyticsEventType_PARTICIPANT_JOINED, "PA_1", orgAttribute("org_01HQZX"),
	), now)
	r.observe(participantEvent(livekit.AnalyticsEventType_PARTICIPANT_LEFT, "PA_1", nil), now)

	resumedAt := now.Add(time.Second)
	r.observe(participantEvent(
		livekit.AnalyticsEventType_PARTICIPANT_RESUMED, "PA_1", orgAttribute("org_01HQZX"),
	), resumedAt)

	afterLinger := now.Add(orgLingerAfterLeave + time.Second)
	require.Zero(t, r.sweep(afterLinger))
	resolved := r.resolve("PA_1", afterLinger)
	require.Equal(t, "org_01HQZX", resolved.OrgID)
}

// A participant lost without a clean teardown never produces PARTICIPANT_LEFT, so
// the index needs a backstop that does not depend on the event arriving.
func TestOrgResolverSweepsEntriesThatWentQuiet(t *testing.T) {
	now := time.Now()
	r := newTestOrgResolver()

	r.observe(participantEvent(
		livekit.AnalyticsEventType_PARTICIPANT_JOINED, "PA_1", orgAttribute("org_01HQZX"),
	), now)

	require.Equal(t, 1, r.sweep(now.Add(orgMaxIdle+time.Minute)))
	require.Zero(t, r.size())
}

// Looking a participant up is what keeps them alive: a long session that never
// re-emits a lifecycle event must not be swept out from under its own samples.
func TestOrgResolverLookupKeepsALongSessionAlive(t *testing.T) {
	now := time.Now()
	r := newTestOrgResolver()

	r.observe(participantEvent(
		livekit.AnalyticsEventType_PARTICIPANT_JOINED, "PA_1", orgAttribute("org_01HQZX"),
	), now)

	// still producing samples well past the idle backstop
	for at := now; at.Before(now.Add(3 * orgMaxIdle)); at = at.Add(orgMaxIdle / 2) {
		resolved := r.resolve("PA_1", at)
		require.Equal(t, "org_01HQZX", resolved.OrgID)
		require.Zero(t, r.sweep(at))
	}
}

// A real ParticipantInfo always carries a kind - STANDARD is participant zero, not
// "absent" - so it should be captured even for a participant whose token never
// carried an organization attribute at all.
func TestOrgResolverCapturesKindEvenWithoutAnOrganization(t *testing.T) {
	now := time.Now()
	r := newTestOrgResolver()

	r.observe(participantEventKind(
		livekit.AnalyticsEventType_PARTICIPANT_JOINED, "PA_1", nil, livekit.ParticipantInfo_EGRESS,
	), now)

	resolved := r.resolve("PA_1", now)
	require.False(t, resolved.Attributed, "no organization attribute was ever supplied")
	require.True(t, resolved.KindKnown)
	require.Equal(t, livekit.ParticipantInfo_EGRESS, resolved.Kind)
}

// Unlike the organization, kind cannot change mid-session, so a later event is free
// to overwrite it - there is no "omitted, so keep the earlier value" case to guard.
func TestOrgResolverKindSurvivesAcrossEvents(t *testing.T) {
	now := time.Now()
	r := newTestOrgResolver()

	r.observe(participantEventKind(
		livekit.AnalyticsEventType_PARTICIPANT_JOINED, "PA_1", orgAttribute("org_01HQZX"), livekit.ParticipantInfo_AGENT,
	), now)
	r.observe(participantEventKind(
		livekit.AnalyticsEventType_PARTICIPANT_ACTIVE, "PA_1", nil, livekit.ParticipantInfo_AGENT,
	), now)

	resolved := r.resolve("PA_1", now)
	require.Equal(t, "org_01HQZX", resolved.OrgID)
	require.True(t, resolved.KindKnown)
	require.Equal(t, livekit.ParticipantInfo_AGENT, resolved.Kind)
}

// observe runs on the telemetry event queue and resolve on the stats flush loop.
// Run with -race; this asserts they are actually safe together.
func TestOrgResolverIsSafeUnderConcurrentUse(t *testing.T) {
	r := newTestOrgResolver()
	now := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				r.observe(participantEvent(
					livekit.AnalyticsEventType_PARTICIPANT_JOINED, "PA_1", orgAttribute("org_01HQZX"),
				), now)
			}
		}()
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				r.resolve("PA_1", now)
			}
		}()
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				r.sweep(now)
				r.size()
			}
		}()
	}
	wg.Wait()
}
