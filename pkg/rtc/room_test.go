package rtc_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	livekit "github.com/livekit/livekit-server/proto"
)

const (
	numParticipants     = 3
	defaultDelay        = 10 * time.Millisecond
	audioUpdateInterval = 25
)

func init() {
	logger.InitDevelopment("")
}

func TestJoinedState(t *testing.T) {
	t.Run("new room should return joinedAt 0", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 0})
		require.Equal(t, int64(0), rm.FirstJoinedAt())
		require.Equal(t, int64(0), rm.LastLeftAt())
	})

	t.Run("should be current time when a participant joins", func(t *testing.T) {
		s := time.Now().Unix()
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
		require.Equal(t, s, rm.FirstJoinedAt())
		require.Equal(t, int64(0), rm.LastLeftAt())
	})

	t.Run("should be set when a participant leaves", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
		p0 := rm.GetParticipants()[0]
		s := time.Now().Unix()
		rm.RemoveParticipant(p0.Identity())
		require.Equal(t, s, rm.LastLeftAt())
	})

	t.Run("LastLeftAt should not be set when there are still participants in the room", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2})
		p0 := rm.GetParticipants()[0]
		rm.RemoveParticipant(p0.Identity())
		require.EqualValues(t, 0, rm.LastLeftAt())
	})
}

func TestRoomJoin(t *testing.T) {
	t.Run("joining returns existing participant data", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: numParticipants})
		pNew := newMockParticipant("new", types.DefaultProtocol)

		rm.Join(pNew, nil)

		// expect new participant to get a JoinReply
		info, participants, iceServers := pNew.SendJoinResponseArgsForCall(0)
		require.Equal(t, info.Sid, rm.Room.Sid)
		require.Len(t, participants, numParticipants)
		require.Len(t, rm.GetParticipants(), numParticipants+1)
		require.NotEmpty(t, iceServers)
	})

	t.Run("subscribe to existing channels upon join", func(t *testing.T) {
		numExisting := 3
		rm := newRoomWithParticipants(t, testRoomOpts{num: numExisting})
		p := newMockParticipant("new", types.DefaultProtocol)

		err := rm.Join(p, &rtc.ParticipantOptions{AutoSubscribe: true})
		require.NoError(t, err)

		stateChangeCB := p.OnStateChangeArgsForCall(0)
		require.NotNil(t, stateChangeCB)
		p.StateReturns(livekit.ParticipantInfo_ACTIVE)
		stateChangeCB(p, livekit.ParticipantInfo_JOINED)

		// it should become a subscriber when connectivity changes
		for _, op := range rm.GetParticipants() {
			if p == op {
				continue
			}
			mockP := op.(*typesfakes.FakeParticipant)
			require.NotZero(t, mockP.AddSubscriberCallCount())
			// last call should be to add the newest participant
			require.Equal(t, p, mockP.AddSubscriberArgsForCall(mockP.AddSubscriberCallCount()-1))
		}
	})

	t.Run("participant state change is broadcasted to others", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: numParticipants})
		var changedParticipant types.Participant
		rm.OnParticipantChanged(func(participant types.Participant) {
			changedParticipant = participant
		})
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeParticipant)
		disconnectedParticipant := participants[1].(*typesfakes.FakeParticipant)
		disconnectedParticipant.StateReturns(livekit.ParticipantInfo_DISCONNECTED)

		rm.RemoveParticipant(p.Identity())
		time.Sleep(defaultDelay)

		require.Equal(t, p, changedParticipant)

		numUpdates := 0
		for _, op := range participants {
			if op == p || op == disconnectedParticipant {
				require.Zero(t, p.SendParticipantUpdateCallCount())
				continue
			}
			fakeP := op.(*typesfakes.FakeParticipant)
			require.Equal(t, 1, fakeP.SendParticipantUpdateCallCount())
			numUpdates += 1
		}
		require.Equal(t, numParticipants-2, numUpdates)
	})

	t.Run("cannot exceed max participants", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
		rm.Room.MaxParticipants = 1
		p := newMockParticipant("second", types.ProtocolVersion(0))

		err := rm.Join(p, nil)
		require.Equal(t, rtc.ErrMaxParticipantsExceeded, err)
	})
}

// various state changes to participant and that others are receiving update
func TestParticipantUpdate(t *testing.T) {
	tests := []struct {
		name         string
		sendToSender bool // should sender receive it
		action       func(p types.Participant)
	}{
		{
			"track mutes are sent to everyone",
			true,
			func(p types.Participant) {
				p.SetTrackMuted("", true)
			},
		},
		{
			"track metadata updates are sent to everyone",
			true,
			func(p types.Participant) {
				p.SetMetadata("")
			},
		},
		{
			"track publishes are sent to existing participants",
			true,
			func(p types.Participant) {
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
			callCounts := make(map[string]int)
			for _, p := range rm.GetParticipants() {
				fp := p.(*typesfakes.FakeParticipant)
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
				fp := p.(*typesfakes.FakeParticipant)
				require.Equal(t, expected, fp.SendParticipantUpdateCallCount())
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
		// allows immediate close after
		rm.Room.EmptyTimeout = 0
		rm.RemoveParticipant(p.Identity())

		time.Sleep(defaultDelay)

		rm.CloseIfEmpty()
		require.Len(t, rm.GetParticipants(), 0)
		require.True(t, isClosed)

		require.Equal(t, rtc.ErrRoomClosed, rm.Join(p, nil))
	})

	t.Run("room does not close before empty timeout", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 0})
		isClosed := false
		rm.OnClose(func() {
			isClosed = true
		})
		require.NotZero(t, rm.Room.EmptyTimeout)
		rm.CloseIfEmpty()
		require.False(t, isClosed)
	})

	t.Run("room closes after empty timeout", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 0})
		isClosed := false
		rm.OnClose(func() {
			isClosed = true
		})
		rm.Room.EmptyTimeout = 1

		time.Sleep(1010 * time.Millisecond)
		rm.CloseIfEmpty()
		require.True(t, isClosed)
	})
}

func TestNewTrack(t *testing.T) {
	t.Run("new track should be added to ready participants", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 3})
		participants := rm.GetParticipants()
		p0 := participants[0].(*typesfakes.FakeParticipant)
		p0.StateReturns(livekit.ParticipantInfo_JOINED)
		p1 := participants[1].(*typesfakes.FakeParticipant)
		p1.StateReturns(livekit.ParticipantInfo_ACTIVE)

		pub := participants[2].(*typesfakes.FakeParticipant)

		// p3 adds track
		track := newMockTrack(livekit.TrackType_VIDEO, "webcam")
		trackCB := pub.OnTrackPublishedArgsForCall(0)
		require.NotNil(t, trackCB)
		trackCB(pub, track)
		// only p2 should've been called
		require.Equal(t, 1, track.AddSubscriberCallCount())
		require.Equal(t, p1, track.AddSubscriberArgsForCall(0))
	})
}

func TestActiveSpeakers(t *testing.T) {
	t.Parallel()
	getActiveSpeakerUpdates := func(p *typesfakes.FakeParticipant) []*livekit.ActiveSpeakerUpdate {
		var updates []*livekit.ActiveSpeakerUpdate
		numCalls := p.SendDataPacketCallCount()
		for i := 0; i < numCalls; i++ {
			dp := p.SendDataPacketArgsForCall(i)
			switch val := dp.Value.(type) {
			case *livekit.DataPacket_Speaker:
				updates = append(updates, val.Speaker)
			}
		}
		return updates
	}

	audioUpdateDuration := (audioUpdateInterval + 10) * time.Millisecond
	t.Run("participant should not be getting audio updates (protocol 2)", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1, protocol: types.DefaultProtocol})
		p := rm.GetParticipants()[0].(*typesfakes.FakeParticipant)
		require.Empty(t, rm.GetActiveSpeakers())

		time.Sleep(audioUpdateDuration)

		updates := getActiveSpeakerUpdates(p)
		require.Empty(t, updates)
	})

	t.Run("speakers should be sorted by loudness (protocol 0)", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2})
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeParticipant)
		p2 := participants[1].(*typesfakes.FakeParticipant)
		p.GetAudioLevelReturns(10, true)
		p2.GetAudioLevelReturns(20, true)

		speakers := rm.GetActiveSpeakers()
		require.Len(t, speakers, 2)
		require.Equal(t, p.ID(), speakers[0].Sid)
		require.Equal(t, p2.ID(), speakers[1].Sid)
	})

	t.Run("participants are getting audio updates (protocol 2)", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2, protocol: types.DefaultProtocol})
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeParticipant)
		time.Sleep(time.Millisecond) // let the first update cycle run
		p.GetAudioLevelReturns(30, true)

		speakers := rm.GetActiveSpeakers()
		require.NotEmpty(t, speakers)
		require.Equal(t, p.ID(), speakers[0].Sid)

		time.Sleep(audioUpdateDuration)

		// everyone should've received updates
		for _, op := range participants {
			op := op.(*typesfakes.FakeParticipant)
			updates := getActiveSpeakerUpdates(op)
			require.Len(t, updates, 1)
		}

		// after another cycle, we are not getting any new updates since unchanged
		time.Sleep(audioUpdateDuration)
		for _, op := range participants {
			op := op.(*typesfakes.FakeParticipant)
			updates := getActiveSpeakerUpdates(op)
			require.Len(t, updates, 1)
		}

		// no longer speaking, send update with empty items
		p.GetAudioLevelReturns(127, false)
		time.Sleep(audioUpdateDuration)
		updates := getActiveSpeakerUpdates(p)
		require.Len(t, updates, 2)
		require.Empty(t, updates[1].Speakers)
	})
}

func TestDataChannel(t *testing.T) {
	t.Parallel()

	t.Run("participants should receive data", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 3})
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeParticipant)

		packet := livekit.DataPacket{
			Kind: livekit.DataPacket_RELIABLE,
			Value: &livekit.DataPacket_User{
				User: &livekit.UserPacket{
					ParticipantSid: p.ID(),
					Payload:        []byte("message.."),
				},
			},
		}
		p.OnDataPacketArgsForCall(0)(p, &packet)

		// ensure everyone has received the packet
		for _, op := range participants {
			fp := op.(*typesfakes.FakeParticipant)
			if fp == p {
				require.Zero(t, fp.SendDataPacketCallCount())
				continue
			}
			require.Equal(t, 1, fp.SendDataPacketCallCount())
			require.Equal(t, packet.Value, fp.SendDataPacketArgsForCall(0).Value)
		}
	})

	t.Run("only one participant should receive the data", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 4})
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeParticipant)
		p1 := participants[1].(*typesfakes.FakeParticipant)

		packet := livekit.DataPacket{
			Kind: livekit.DataPacket_RELIABLE,
			Value: &livekit.DataPacket_User{
				User: &livekit.UserPacket{
					ParticipantSid:  p.ID(),
					Payload:         []byte("message to p1.."),
					DestinationSids: []string{p1.ID()},
				},
			},
		}
		p.OnDataPacketArgsForCall(0)(p, &packet)

		// only p1 should receive the data
		for _, op := range participants {
			fp := op.(*typesfakes.FakeParticipant)
			if fp != p1 {
				require.Zero(t, fp.SendDataPacketCallCount())
			}
		}
		require.Equal(t, 1, p1.SendDataPacketCallCount())
		require.Equal(t, packet.Value, p1.SendDataPacketArgsForCall(0).Value)
	})
}

type testRoomOpts struct {
	num      int
	protocol types.ProtocolVersion
}

func newRoomWithParticipants(t *testing.T, opts testRoomOpts) *rtc.Room {
	rm := rtc.NewRoom(
		&livekit.Room{Name: "room"},
		rtc.WebRTCConfig{},
		[]*livekit.ICEServer{
			{
				Urls: []string{
					"stun:stun.l.google.com:19302",
				},
			},
		},
		audioUpdateInterval,
	)
	for i := 0; i < opts.num; i++ {
		identity := fmt.Sprintf("p%d", i)
		participant := newMockParticipant(identity, opts.protocol)
		err := rm.Join(participant, &rtc.ParticipantOptions{AutoSubscribe: true})
		participant.StateReturns(livekit.ParticipantInfo_ACTIVE)
		require.NoError(t, err)
	}
	return rm
}
