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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/auth/authfakes"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/webhook"

	"github.com/livekit/livekit-server/version"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/audio"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/livekit-server/pkg/telemetry/telemetryfakes"
	"github.com/livekit/livekit-server/pkg/testutils"
)

func init() {
	prometheus.Init("test", livekit.NodeType_SERVER)
}

const (
	numParticipants     = 3
	defaultDelay        = 10 * time.Millisecond
	audioUpdateInterval = 25
)

func init() {
	config.InitLoggerFromConfig(&config.DefaultConfig.Logging)
	roomUpdateInterval = defaultDelay
}

var iceServersForRoom = []*livekit.ICEServer{{Urls: []string{"stun:stun.l.google.com:19302"}}}

func TestJoinedState(t *testing.T) {
	t.Run("new room should return joinedAt 0", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 0})
		require.Equal(t, int64(0), rm.FirstJoinedAt())
		require.Equal(t, int64(0), rm.LastLeftAt())
	})

	t.Run("should be current time when a participant joins", func(t *testing.T) {
		s := time.Now().Unix()
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
		require.LessOrEqual(t, s, rm.FirstJoinedAt())
		require.Equal(t, int64(0), rm.LastLeftAt())
	})

	t.Run("should be set when a participant leaves", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
		p0 := rm.GetParticipants()[0]
		s := time.Now().Unix()
		rm.RemoveParticipant(p0.Identity(), p0.ID(), types.ParticipantCloseReasonClientRequestLeave)
		require.LessOrEqual(t, s, rm.LastLeftAt())
	})

	t.Run("LastLeftAt should be set when there are still participants in the room", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2})
		p0 := rm.GetParticipants()[0]
		rm.RemoveParticipant(p0.Identity(), p0.ID(), types.ParticipantCloseReasonClientRequestLeave)
		require.Greater(t, rm.LastLeftAt(), int64(0))
	})
}

func TestRoomJoin(t *testing.T) {
	t.Run("joining returns existing participant data", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: numParticipants})
		pNew := NewMockParticipant("new", types.CurrentProtocol, false, false)

		_ = rm.Join(pNew, nil, nil, iceServersForRoom)

		// expect new participant to get a JoinReply
		res := pNew.SendJoinResponseArgsForCall(0)
		require.Equal(t, livekit.RoomID(res.Room.Sid), rm.ID())
		require.Len(t, res.OtherParticipants, numParticipants)
		require.Len(t, rm.GetParticipants(), numParticipants+1)
		require.NotEmpty(t, res.IceServers)
	})

	t.Run("subscribe to existing channels upon join", func(t *testing.T) {
		numExisting := 3
		rm := newRoomWithParticipants(t, testRoomOpts{num: numExisting})
		p := NewMockParticipant("new", types.CurrentProtocol, false, false)

		err := rm.Join(p, nil, &ParticipantOptions{AutoSubscribe: true}, iceServersForRoom)
		require.NoError(t, err)

		stateChangeCB := p.OnStateChangeArgsForCall(0)
		require.NotNil(t, stateChangeCB)
		p.StateReturns(livekit.ParticipantInfo_ACTIVE)
		stateChangeCB(p)

		// it should become a subscriber when connectivity changes
		numTracks := 0
		for _, op := range rm.GetParticipants() {
			if p == op {
				continue
			}

			numTracks += len(op.GetPublishedTracks())
		}
		require.Equal(t, numTracks, p.SubscribeToTrackCallCount())
	})

	t.Run("participant state change is broadcasted to others", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: numParticipants})
		var changedParticipant types.Participant
		rm.OnParticipantChanged(func(participant types.LocalParticipant) {
			changedParticipant = participant
		})
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeLocalParticipant)
		disconnectedParticipant := participants[1].(*typesfakes.FakeLocalParticipant)
		disconnectedParticipant.StateReturns(livekit.ParticipantInfo_DISCONNECTED)

		rm.RemoveParticipant(p.Identity(), p.ID(), types.ParticipantCloseReasonClientRequestLeave)
		time.Sleep(defaultDelay)

		require.Equal(t, p, changedParticipant)

		numUpdates := 0
		for _, op := range participants {
			if op == p || op == disconnectedParticipant {
				require.Zero(t, p.SendParticipantUpdateCallCount())
				continue
			}
			fakeP := op.(*typesfakes.FakeLocalParticipant)
			require.Equal(t, 1, fakeP.SendParticipantUpdateCallCount())
			numUpdates += 1
		}
		require.Equal(t, numParticipants-2, numUpdates)
	})

	t.Run("cannot exceed max participants", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
		rm.lock.Lock()
		rm.protoRoom.MaxParticipants = 1
		rm.lock.Unlock()
		p := NewMockParticipant("second", types.ProtocolVersion(0), false, false)

		err := rm.Join(p, nil, nil, iceServersForRoom)
		require.Equal(t, ErrMaxParticipantsExceeded, err)
	})
}

// various state changes to participant and that others are receiving update
func TestParticipantUpdate(t *testing.T) {
	tests := []struct {
		name         string
		sendToSender bool // should sender receive it
		action       func(p types.LocalParticipant)
	}{
		{
			"track mutes are sent to everyone",
			true,
			func(p types.LocalParticipant) {
				p.SetTrackMuted(&livekit.MuteTrackRequest{Muted: true}, false)
			},
		},
		{
			"track metadata updates are sent to everyone",
			true,
			func(p types.LocalParticipant) {
				p.SetMetadata("")
			},
		},
		{
			"track publishes are sent to existing participants",
			true,
			func(p types.LocalParticipant) {
				p.AddTrack(&livekit.AddTrackRequest{
					Type: livekit.TrackType_VIDEO,
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rm := newRoomWithParticipants(t, testRoomOpts{num: 3})
			// remember how many times send has been called for each
			callCounts := make(map[livekit.ParticipantID]int)
			for _, p := range rm.GetParticipants() {
				fp := p.(*typesfakes.FakeLocalParticipant)
				callCounts[p.ID()] = fp.SendParticipantUpdateCallCount()
			}

			sender := rm.GetParticipants()[0]
			test.action(sender)

			// go through the other participants, make sure they've received update
			for _, p := range rm.GetParticipants() {
				expected := callCounts[p.ID()]
				if p != sender || test.sendToSender {
					expected += 1
				}
				fp := p.(*typesfakes.FakeLocalParticipant)
				require.Equal(t, expected, fp.SendParticipantUpdateCallCount())
			}
		})
	}
}

func TestPushAndDequeueUpdates(t *testing.T) {
	identity := "test_user"
	publisher1v1 := &livekit.ParticipantInfo{
		Identity:    identity,
		Sid:         "1",
		IsPublisher: true,
		Version:     1,
		JoinedAt:    0,
	}
	publisher1v2 := &livekit.ParticipantInfo{
		Identity:    identity,
		Sid:         "1",
		IsPublisher: true,
		Version:     2,
		JoinedAt:    1,
	}
	publisher2 := &livekit.ParticipantInfo{
		Identity:    identity,
		Sid:         "2",
		IsPublisher: true,
		Version:     1,
		JoinedAt:    2,
	}
	subscriber1v1 := &livekit.ParticipantInfo{
		Identity: identity,
		Sid:      "1",
		Version:  1,
		JoinedAt: 0,
	}
	subscriber1v2 := &livekit.ParticipantInfo{
		Identity: identity,
		Sid:      "1",
		Version:  2,
		JoinedAt: 1,
	}

	requirePIEquals := func(t *testing.T, a, b *livekit.ParticipantInfo) {
		require.Equal(t, a.Sid, b.Sid)
		require.Equal(t, a.Identity, b.Identity)
		require.Equal(t, a.Version, b.Version)
	}
	testCases := []struct {
		name        string
		pi          *livekit.ParticipantInfo
		closeReason types.ParticipantCloseReason
		immediate   bool
		existing    *ParticipantUpdate
		expected    []*ParticipantUpdate
		validate    func(t *testing.T, rm *Room, updates []*ParticipantUpdate)
	}{
		{
			name:     "publisher updates are immediate",
			pi:       publisher1v1,
			expected: []*ParticipantUpdate{{ParticipantInfo: publisher1v1}},
		},
		{
			name: "subscriber updates are queued",
			pi:   subscriber1v1,
		},
		{
			name:     "last version is enqueued",
			pi:       subscriber1v2,
			existing: &ParticipantUpdate{ParticipantInfo: utils.CloneProto(subscriber1v1)}, // clone the existing value since it can be modified when setting to disconnected
			validate: func(t *testing.T, rm *Room, _ []*ParticipantUpdate) {
				queued := rm.batchedUpdates[livekit.ParticipantIdentity(identity)]
				require.NotNil(t, queued)
				requirePIEquals(t, subscriber1v2, queued.ParticipantInfo)
			},
		},
		{
			name:      "latest version when immediate",
			pi:        subscriber1v2,
			existing:  &ParticipantUpdate{ParticipantInfo: utils.CloneProto(subscriber1v1)},
			immediate: true,
			expected:  []*ParticipantUpdate{{ParticipantInfo: subscriber1v2}},
			validate: func(t *testing.T, rm *Room, _ []*ParticipantUpdate) {
				queued := rm.batchedUpdates[livekit.ParticipantIdentity(identity)]
				require.Nil(t, queued)
			},
		},
		{
			name:     "out of order updates are rejected",
			pi:       subscriber1v1,
			existing: &ParticipantUpdate{ParticipantInfo: utils.CloneProto(subscriber1v2)},
			validate: func(t *testing.T, rm *Room, updates []*ParticipantUpdate) {
				queued := rm.batchedUpdates[livekit.ParticipantIdentity(identity)]
				requirePIEquals(t, subscriber1v2, queued.ParticipantInfo)
			},
		},
		{
			name:        "sid change is broadcasted immediately with synthsized disconnect",
			pi:          publisher2,
			closeReason: types.ParticipantCloseReasonServiceRequestRemoveParticipant, // just to test if update contain the close reason
			existing:    &ParticipantUpdate{ParticipantInfo: utils.CloneProto(subscriber1v2), CloseReason: types.ParticipantCloseReasonStale},
			expected: []*ParticipantUpdate{
				{
					ParticipantInfo: &livekit.ParticipantInfo{
						Identity: identity,
						Sid:      "1",
						Version:  2,
						State:    livekit.ParticipantInfo_DISCONNECTED,
					},
					IsSynthesizedDisconnect: true,
					CloseReason:             types.ParticipantCloseReasonStale,
				},
				{ParticipantInfo: publisher2, CloseReason: types.ParticipantCloseReasonServiceRequestRemoveParticipant},
			},
		},
		{
			name:     "when switching to publisher, queue is cleared",
			pi:       publisher1v2,
			existing: &ParticipantUpdate{ParticipantInfo: utils.CloneProto(subscriber1v1)},
			expected: []*ParticipantUpdate{{ParticipantInfo: publisher1v2}},
			validate: func(t *testing.T, rm *Room, updates []*ParticipantUpdate) {
				require.Empty(t, rm.batchedUpdates)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
			if tc.existing != nil {
				rm.batchedUpdates[livekit.ParticipantIdentity(tc.existing.ParticipantInfo.Identity)] = tc.existing
			}
			rm.batchedUpdatesMu.Lock()
			updates := PushAndDequeueUpdates(
				tc.pi,
				tc.closeReason,
				tc.immediate,
				rm.GetParticipant(livekit.ParticipantIdentity(tc.pi.Identity)),
				rm.batchedUpdates,
			)
			rm.batchedUpdatesMu.Unlock()
			require.Equal(t, len(tc.expected), len(updates))
			for i, item := range tc.expected {
				requirePIEquals(t, item.ParticipantInfo, updates[i].ParticipantInfo)
				require.Equal(t, item.IsSynthesizedDisconnect, updates[i].IsSynthesizedDisconnect)
				require.Equal(t, item.CloseReason, updates[i].CloseReason)
			}

			if tc.validate != nil {
				tc.validate(t, rm, updates)
			}
		})
	}
}

func TestRoomClosure(t *testing.T) {
	t.Run("room closes after participant leaves", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
		isClosed := false
		rm.OnClose(func() {
			isClosed = true
		})
		p := rm.GetParticipants()[0]
		rm.lock.Lock()
		// allows immediate close after
		rm.protoRoom.EmptyTimeout = 0
		rm.lock.Unlock()
		rm.RemoveParticipant(p.Identity(), p.ID(), types.ParticipantCloseReasonClientRequestLeave)

		time.Sleep(time.Duration(rm.ToProto().DepartureTimeout)*time.Second + defaultDelay)

		rm.CloseIfEmpty()
		require.Len(t, rm.GetParticipants(), 0)
		require.True(t, isClosed)

		require.Equal(t, ErrRoomClosed, rm.Join(p, nil, nil, iceServersForRoom))
	})

	t.Run("room does not close before empty timeout", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 0})
		isClosed := false
		rm.OnClose(func() {
			isClosed = true
		})
		require.NotZero(t, rm.protoRoom.EmptyTimeout)
		rm.CloseIfEmpty()
		require.False(t, isClosed)
	})

	t.Run("room closes after empty timeout", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 0})
		isClosed := false
		rm.OnClose(func() {
			isClosed = true
		})
		rm.lock.Lock()
		rm.protoRoom.EmptyTimeout = 1
		rm.lock.Unlock()

		time.Sleep(1010 * time.Millisecond)
		rm.CloseIfEmpty()
		require.True(t, isClosed)
	})
}

func TestNewTrack(t *testing.T) {
	t.Run("new track should be added to ready participants", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 3})
		participants := rm.GetParticipants()
		p0 := participants[0].(*typesfakes.FakeLocalParticipant)
		p0.StateReturns(livekit.ParticipantInfo_JOINED)
		p1 := participants[1].(*typesfakes.FakeLocalParticipant)
		p1.StateReturns(livekit.ParticipantInfo_ACTIVE)

		pub := participants[2].(*typesfakes.FakeLocalParticipant)

		// pub adds track
		track := NewMockTrack(livekit.TrackType_VIDEO, "webcam")
		trackCB := pub.OnTrackPublishedArgsForCall(0)
		require.NotNil(t, trackCB)
		trackCB(pub, track)
		// only p1 should've been subscribed to
		require.Equal(t, 0, p0.SubscribeToTrackCallCount())
		require.Equal(t, 1, p1.SubscribeToTrackCallCount())
	})
}

func TestActiveSpeakers(t *testing.T) {
	t.Parallel()
	getActiveSpeakerUpdates := func(p *typesfakes.FakeLocalParticipant) [][]*livekit.SpeakerInfo {
		var updates [][]*livekit.SpeakerInfo
		numCalls := p.SendSpeakerUpdateCallCount()
		for i := 0; i < numCalls; i++ {
			infos, _ := p.SendSpeakerUpdateArgsForCall(i)
			updates = append(updates, infos)
		}
		return updates
	}

	audioUpdateDuration := (audioUpdateInterval + 10) * time.Millisecond
	t.Run("participant should not be getting audio updates (protocol 2)", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1, protocol: 2})
		defer rm.Close(types.ParticipantCloseReasonNone)
		p := rm.GetParticipants()[0].(*typesfakes.FakeLocalParticipant)
		require.Empty(t, rm.GetActiveSpeakers())

		time.Sleep(audioUpdateDuration)

		updates := getActiveSpeakerUpdates(p)
		require.Empty(t, updates)
	})

	t.Run("speakers should be sorted by loudness", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2})
		defer rm.Close(types.ParticipantCloseReasonNone)
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeLocalParticipant)
		p2 := participants[1].(*typesfakes.FakeLocalParticipant)
		p.GetAudioLevelReturns(20, true)
		p2.GetAudioLevelReturns(10, true)

		speakers := rm.GetActiveSpeakers()
		require.Len(t, speakers, 2)
		require.Equal(t, string(p.ID()), speakers[0].Sid)
		require.Equal(t, string(p2.ID()), speakers[1].Sid)
	})

	t.Run("participants are getting audio updates (protocol 3+)", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2, protocol: 3})
		defer rm.Close(types.ParticipantCloseReasonNone)
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeLocalParticipant)
		time.Sleep(time.Millisecond) // let the first update cycle run
		p.GetAudioLevelReturns(30, true)

		speakers := rm.GetActiveSpeakers()
		require.NotEmpty(t, speakers)
		require.Equal(t, string(p.ID()), speakers[0].Sid)

		testutils.WithTimeout(t, func() string {
			for _, op := range participants {
				op := op.(*typesfakes.FakeLocalParticipant)
				updates := getActiveSpeakerUpdates(op)
				if len(updates) == 0 {
					return fmt.Sprintf("%s did not get any audio updates", op.Identity())
				}
			}
			return ""
		})

		// no longer speaking, send update with empty items
		p.GetAudioLevelReturns(127, false)

		testutils.WithTimeout(t, func() string {
			updates := getActiveSpeakerUpdates(p)
			lastUpdate := updates[len(updates)-1]
			if len(lastUpdate) == 0 {
				return "did not get updates of speaker going quiet"
			}
			if lastUpdate[0].Active {
				return "speaker should not have been active"
			}
			return ""
		})
	})

	t.Run("audio level is smoothed", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2, protocol: 3, audioSmoothIntervals: 3})
		defer rm.Close(types.ParticipantCloseReasonNone)
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeLocalParticipant)
		op := participants[1].(*typesfakes.FakeLocalParticipant)
		p.GetAudioLevelReturns(30, true)
		convertedLevel := float32(audio.ConvertAudioLevel(30))

		testutils.WithTimeout(t, func() string {
			updates := getActiveSpeakerUpdates(op)
			if len(updates) == 0 {
				return "no speaker updates received"
			}
			lastSpeakers := updates[len(updates)-1]
			if len(lastSpeakers) == 0 {
				return "no speakers in the update"
			}
			if lastSpeakers[0].Level > convertedLevel {
				return ""
			}
			return "level mismatch"
		})

		testutils.WithTimeout(t, func() string {
			updates := getActiveSpeakerUpdates(op)
			if len(updates) == 0 {
				return "no updates received"
			}
			lastSpeakers := updates[len(updates)-1]
			if len(lastSpeakers) == 0 {
				return "no speakers found"
			}
			if lastSpeakers[0].Level > convertedLevel {
				return ""
			}
			return "did not match expected levels"
		})

		p.GetAudioLevelReturns(127, false)
		testutils.WithTimeout(t, func() string {
			updates := getActiveSpeakerUpdates(op)
			if len(updates) == 0 {
				return "no speaker updates received"
			}
			lastSpeakers := updates[len(updates)-1]
			if len(lastSpeakers) == 1 && !lastSpeakers[0].Active {
				return ""
			}
			return "speakers didn't go back to zero"
		})
	})
}

func TestDataChannel(t *testing.T) {
	t.Parallel()

	const (
		curAPI = iota
		legacySID
		legacyIdentity
	)
	modes := []int{
		curAPI, legacySID, legacyIdentity,
	}
	modeNames := []string{
		"cur", "legacy sid", "legacy identity",
	}

	setSource := func(mode int, dp *livekit.DataPacket, p types.LocalParticipant) {
		switch mode {
		case curAPI:
			dp.ParticipantIdentity = string(p.Identity())
		case legacySID:
			dp.GetUser().ParticipantSid = string(p.ID())
		case legacyIdentity:
			dp.GetUser().ParticipantIdentity = string(p.Identity())
		}
	}
	setDest := func(mode int, dp *livekit.DataPacket, p types.LocalParticipant) {
		switch mode {
		case curAPI:
			dp.DestinationIdentities = []string{string(p.Identity())}
		case legacySID:
			dp.GetUser().DestinationSids = []string{string(p.ID())}
		case legacyIdentity:
			dp.GetUser().DestinationIdentities = []string{string(p.Identity())}
		}
	}

	t.Run("participants should receive data", func(t *testing.T) {
		for _, mode := range modes {
			mode := mode
			t.Run(modeNames[mode], func(t *testing.T) {
				rm := newRoomWithParticipants(t, testRoomOpts{num: 3})
				defer rm.Close(types.ParticipantCloseReasonNone)
				participants := rm.GetParticipants()
				p := participants[0].(*typesfakes.FakeLocalParticipant)

				packet := &livekit.DataPacket{
					Kind: livekit.DataPacket_RELIABLE,
					Value: &livekit.DataPacket_User{
						User: &livekit.UserPacket{
							Payload: []byte("message.."),
						},
					},
				}
				setSource(mode, packet, p)

				packetExp := utils.CloneProto(packet)
				if mode != legacySID {
					packetExp.ParticipantIdentity = string(p.Identity())
					packetExp.GetUser().ParticipantIdentity = string(p.Identity())
				}

				encoded, _ := proto.Marshal(packetExp)
				p.OnDataPacketArgsForCall(0)(p, packet.Kind, packet)

				// ensure everyone has received the packet
				for _, op := range participants {
					fp := op.(*typesfakes.FakeLocalParticipant)
					if fp == p {
						require.Zero(t, fp.SendDataMessageCallCount())
						continue
					}
					require.Equal(t, 1, fp.SendDataMessageCallCount())
					_, got, _, _ := fp.SendDataMessageArgsForCall(0)
					require.Equal(t, encoded, got)
				}
			})
		}
	})

	t.Run("only one participant should receive the data", func(t *testing.T) {
		for _, mode := range modes {
			mode := mode
			t.Run(modeNames[mode], func(t *testing.T) {
				rm := newRoomWithParticipants(t, testRoomOpts{num: 4})
				defer rm.Close(types.ParticipantCloseReasonNone)
				participants := rm.GetParticipants()
				p := participants[0].(*typesfakes.FakeLocalParticipant)
				p1 := participants[1].(*typesfakes.FakeLocalParticipant)

				packet := &livekit.DataPacket{
					Kind: livekit.DataPacket_RELIABLE,
					Value: &livekit.DataPacket_User{
						User: &livekit.UserPacket{
							Payload: []byte("message to p1.."),
						},
					},
				}
				setSource(mode, packet, p)
				setDest(mode, packet, p1)

				packetExp := utils.CloneProto(packet)
				if mode != legacySID {
					packetExp.ParticipantIdentity = string(p.Identity())
					packetExp.GetUser().ParticipantIdentity = string(p.Identity())
					packetExp.DestinationIdentities = []string{string(p1.Identity())}
					packetExp.GetUser().DestinationIdentities = []string{string(p1.Identity())}
				}

				encoded, _ := proto.Marshal(packetExp)
				p.OnDataPacketArgsForCall(0)(p, packet.Kind, packet)

				// only p1 should receive the data
				for _, op := range participants {
					fp := op.(*typesfakes.FakeLocalParticipant)
					if fp != p1 {
						require.Zero(t, fp.SendDataMessageCallCount())
					}
				}
				require.Equal(t, 1, p1.SendDataMessageCallCount())
				_, got, _, _ := p1.SendDataMessageArgsForCall(0)
				require.Equal(t, encoded, got)
			})
		}
	})

	t.Run("publishing disallowed", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2})
		defer rm.Close(types.ParticipantCloseReasonNone)
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeLocalParticipant)
		p.CanPublishDataReturns(false)

		packet := livekit.DataPacket{
			Kind: livekit.DataPacket_RELIABLE,
			Value: &livekit.DataPacket_User{
				User: &livekit.UserPacket{
					Payload: []byte{},
				},
			},
		}
		if p.CanPublishData() {
			p.OnDataPacketArgsForCall(0)(p, packet.Kind, &packet)
		}

		// no one should've been sent packet
		for _, op := range participants {
			fp := op.(*typesfakes.FakeLocalParticipant)
			require.Zero(t, fp.SendDataMessageCallCount())
		}
	})
}

func TestHiddenParticipants(t *testing.T) {
	t.Run("other participants don't receive hidden updates", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2, numHidden: 1})
		defer rm.Close(types.ParticipantCloseReasonNone)

		pNew := NewMockParticipant("new", types.CurrentProtocol, false, false)
		rm.Join(pNew, nil, nil, iceServersForRoom)

		// expect new participant to get a JoinReply
		res := pNew.SendJoinResponseArgsForCall(0)
		require.Equal(t, livekit.RoomID(res.Room.Sid), rm.ID())
		require.Len(t, res.OtherParticipants, 2)
		require.Len(t, rm.GetParticipants(), 4)
		require.NotEmpty(t, res.IceServers)
		require.Equal(t, "testregion", res.ServerInfo.Region)
	})

	t.Run("hidden participant subscribes to tracks", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2})
		hidden := NewMockParticipant("hidden", types.CurrentProtocol, true, false)

		err := rm.Join(hidden, nil, &ParticipantOptions{AutoSubscribe: true}, iceServersForRoom)
		require.NoError(t, err)

		stateChangeCB := hidden.OnStateChangeArgsForCall(0)
		require.NotNil(t, stateChangeCB)
		hidden.StateReturns(livekit.ParticipantInfo_ACTIVE)
		stateChangeCB(hidden)

		require.Eventually(t, func() bool { return hidden.SubscribeToTrackCallCount() == 2 }, 5*time.Second, 10*time.Millisecond)
	})
}

func TestRoomUpdate(t *testing.T) {
	t.Run("updates are sent when participant joined", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
		defer rm.Close(types.ParticipantCloseReasonNone)

		p1 := rm.GetParticipants()[0].(*typesfakes.FakeLocalParticipant)
		require.Equal(t, 0, p1.SendRoomUpdateCallCount())

		p2 := NewMockParticipant("p2", types.CurrentProtocol, false, false)
		require.NoError(t, rm.Join(p2, nil, nil, iceServersForRoom))

		// p1 should have received an update
		time.Sleep(2 * defaultDelay)
		require.LessOrEqual(t, 1, p1.SendRoomUpdateCallCount())
		require.EqualValues(t, 2, p1.SendRoomUpdateArgsForCall(p1.SendRoomUpdateCallCount()-1).NumParticipants)
	})

	t.Run("participants should receive metadata update", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2})
		defer rm.Close(types.ParticipantCloseReasonNone)

		rm.SetMetadata("test metadata...")

		// callbacks are updated from goroutine
		time.Sleep(2 * defaultDelay)

		for _, op := range rm.GetParticipants() {
			fp := op.(*typesfakes.FakeLocalParticipant)
			// room updates are now sent for both participant joining and room metadata
			require.GreaterOrEqual(t, fp.SendRoomUpdateCallCount(), 1)
		}
	})
}

type testRoomOpts struct {
	num                  int
	numHidden            int
	protocol             types.ProtocolVersion
	audioSmoothIntervals uint32
}

func newRoomWithParticipants(t *testing.T, opts testRoomOpts) *Room {
	kp := &authfakes.FakeKeyProvider{}
	kp.GetSecretReturns("testkey")

	n, err := webhook.NewDefaultNotifier(webhook.DefaultWebHookConfig, kp)
	require.NoError(t, err)

	rm := NewRoom(
		&livekit.Room{Name: "room"},
		nil,
		WebRTCConfig{},
		config.RoomConfig{
			EmptyTimeout:     5 * 60,
			DepartureTimeout: 1,
		},
		&sfu.AudioConfig{
			AudioLevelConfig: audio.AudioLevelConfig{
				UpdateInterval:  audioUpdateInterval,
				SmoothIntervals: opts.audioSmoothIntervals,
			},
		},
		&livekit.ServerInfo{
			Edition:  livekit.ServerInfo_Standard,
			Version:  version.Version,
			Protocol: types.CurrentProtocol,
			NodeId:   "testnode",
			Region:   "testregion",
		},
		telemetry.NewTelemetryService(n, &telemetryfakes.FakeAnalyticsService{}),
		nil, nil, nil,
	)
	for i := 0; i < opts.num+opts.numHidden; i++ {
		identity := livekit.ParticipantIdentity(fmt.Sprintf("p%d", i))
		participant := NewMockParticipant(identity, opts.protocol, i >= opts.num, true)
		err := rm.Join(participant, nil, &ParticipantOptions{AutoSubscribe: true}, iceServersForRoom)
		require.NoError(t, err)
		participant.StateReturns(livekit.ParticipantInfo_ACTIVE)
		participant.IsReadyReturns(true)
		// each participant has a track
		participant.GetPublishedTracksReturns([]types.MediaTrack{
			&typesfakes.FakeMediaTrack{},
		})
	}
	return rm
}
