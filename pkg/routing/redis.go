package routing

import (
	"context"

	"github.com/go-redis/redis/v8"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
)

const (
	// hash of node_id => Node proto
	NodesKey = "nodes"

	// hash of room_name => node_id
	NodeRoomKey = "room_node_map"
)

var redisCtx = context.Background()

// location of the participant's RTC connection, hash
func participantRTCKey(participantKey livekit.ParticipantKey) string {
	return "participant_rtc:" + string(participantKey)
}

// location of the participant's Signal connection, hash
func participantSignalKey(connectionID livekit.ConnectionID) string {
	return "participant_signal:" + string(connectionID)
}

func rtcNodeChannel(nodeID livekit.NodeID) string {
	return "rtc_channel:" + string(nodeID)
}

func signalNodeChannel(nodeID livekit.NodeID) string {
	return "signal_channel:" + string(nodeID)
}

func publishRTCMessage(rc redis.UniversalClient, nodeID livekit.NodeID, participantKey livekit.ParticipantKey, msg proto.Message) error {
	rm := &livekit.RTCNodeMessage{
		ParticipantKey: string(participantKey),
	}
	switch o := msg.(type) {
	case *livekit.StartSession:
		rm.Message = &livekit.RTCNodeMessage_StartSession{
			StartSession: o,
		}
	case *livekit.SignalRequest:
		rm.Message = &livekit.RTCNodeMessage_Request{
			Request: o,
		}
	case *livekit.RTCNodeMessage:
		rm = o
		rm.ParticipantKey = string(participantKey)
	default:
		return ErrInvalidRouterMessage
	}
	data, err := proto.Marshal(rm)
	if err != nil {
		return err
	}

	// logger.Debugw("publishing to rtc", "rtcChannel", rtcNodeChannel(nodeID),
	//	"message", rm.Message)
	return rc.Publish(redisCtx, rtcNodeChannel(nodeID), data).Err()
}

func publishSignalMessage(rc redis.UniversalClient, nodeID livekit.NodeID, connectionID livekit.ConnectionID, msg proto.Message) error {
	rm := &livekit.SignalNodeMessage{
		ConnectionId: string(connectionID),
	}
	switch o := msg.(type) {
	case *livekit.SignalResponse:
		rm.Message = &livekit.SignalNodeMessage_Response{
			Response: o,
		}
	case *livekit.EndSession:
		rm.Message = &livekit.SignalNodeMessage_EndSession{
			EndSession: o,
		}
	default:
		return ErrInvalidRouterMessage
	}
	data, err := proto.Marshal(rm)
	if err != nil {
		return err
	}

	// logger.Debugw("publishing to signal", "signalChannel", signalNodeChannel(nodeID),
	//	"message", rm.Message)
	return rc.Publish(redisCtx, signalNodeChannel(nodeID), data).Err()
}

type RTCNodeSink struct {
	rc             redis.UniversalClient
	nodeID         livekit.NodeID
	participantKey livekit.ParticipantKey
	isClosed       atomic.Bool
	onClose        func()
}

func NewRTCNodeSink(rc redis.UniversalClient, nodeID livekit.NodeID, participantKey livekit.ParticipantKey) *RTCNodeSink {
	return &RTCNodeSink{
		rc:             rc,
		nodeID:         nodeID,
		participantKey: participantKey,
	}
}

func (s *RTCNodeSink) WriteMessage(msg proto.Message) error {
	if s.isClosed.Load() {
		return ErrChannelClosed
	}
	return publishRTCMessage(s.rc, s.nodeID, s.participantKey, msg)
}

func (s *RTCNodeSink) Close() {
	if s.isClosed.Swap(true) {
		return
	}
	if s.onClose != nil {
		s.onClose()
	}
}

func (s *RTCNodeSink) OnClose(f func()) {
	s.onClose = f
}

type SignalNodeSink struct {
	rc           redis.UniversalClient
	nodeID       livekit.NodeID
	connectionID livekit.ConnectionID
	isClosed     atomic.Bool
	onClose      func()
}

func NewSignalNodeSink(rc redis.UniversalClient, nodeID livekit.NodeID, connectionID livekit.ConnectionID) *SignalNodeSink {
	return &SignalNodeSink{
		rc:           rc,
		nodeID:       nodeID,
		connectionID: connectionID,
	}
}

func (s *SignalNodeSink) WriteMessage(msg proto.Message) error {
	if s.isClosed.Load() {
		return ErrChannelClosed
	}
	return publishSignalMessage(s.rc, s.nodeID, s.connectionID, msg)
}

func (s *SignalNodeSink) Close() {
	if s.isClosed.Swap(true) {
		return
	}
	_ = publishSignalMessage(s.rc, s.nodeID, s.connectionID, &livekit.EndSession{})
	if s.onClose != nil {
		s.onClose()
	}
}

func (s *SignalNodeSink) OnClose(f func()) {
	s.onClose = f
}
