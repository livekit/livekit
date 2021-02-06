package routing

import (
	"context"

	"github.com/go-redis/redis/v8"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	// hash of node_id => Node proto
	NodesKey = "nodes"

	// hash of room_name => node_id
	NodeRoomKey = "room_node_map"
)

var redisCtx = context.Background()

// location of the participant's RTC connection, hash
func participantRTCKey(participantKey string) string {
	return "participant_rtc:" + participantKey
}

// location of the participant's Signal connection, hash
func participantSignalKey(connectionId string) string {
	return "participant_signal:" + connectionId
}

func rtcNodeChannel(nodeId string) string {
	return "rtc_channel:" + nodeId
}

func signalNodeChannel(nodeId string) string {
	return "signal_channel:" + nodeId
}

func publishRTCMessage(rc *redis.Client, nodeId string, participantKey string, msg proto.Message) error {
	rm := &livekit.RTCNodeMessage{
		ParticipantKey: participantKey,
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
	default:
		return errInvalidRouterMessage
	}
	data, err := proto.Marshal(rm)
	if err != nil {
		return err
	}

	//logger.Debugw("publishing to", "rtcChannel", rtcNodeChannel(nodeId),
	//	"message", rm.Message)
	return rc.Publish(redisCtx, rtcNodeChannel(nodeId), data).Err()
}

func publishSignalMessage(rc *redis.Client, nodeId string, connectionId string, msg proto.Message) error {
	rm := &livekit.SignalNodeMessage{
		ConnectionId: connectionId,
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
		return errInvalidRouterMessage
	}
	data, err := proto.Marshal(rm)
	if err != nil {
		return err
	}
	return rc.Publish(redisCtx, signalNodeChannel(nodeId), data).Err()
}

type RTCNodeSink struct {
	rc             *redis.Client
	nodeId         string
	participantKey string
	isClosed       utils.AtomicFlag
	onClose        func()
}

func NewRTCNodeSink(rc *redis.Client, nodeId, participantKey string) *RTCNodeSink {
	return &RTCNodeSink{
		rc:             rc,
		nodeId:         nodeId,
		participantKey: participantKey,
	}
}

func (s *RTCNodeSink) WriteMessage(msg proto.Message) error {
	if s.isClosed.Get() {
		return ErrChannelClosed
	}
	return publishRTCMessage(s.rc, s.nodeId, s.participantKey, msg)
}

func (s *RTCNodeSink) Close() {
	if !s.isClosed.TrySet(true) {
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
	rc           *redis.Client
	nodeId       string
	connectionId string
	isClosed     utils.AtomicFlag
	onClose      func()
}

func NewSignalNodeSink(rc *redis.Client, nodeId, connectionId string) *SignalNodeSink {
	return &SignalNodeSink{
		rc:           rc,
		nodeId:       nodeId,
		connectionId: connectionId,
	}
}

func (s *SignalNodeSink) WriteMessage(msg proto.Message) error {
	if s.isClosed.Get() {
		return ErrChannelClosed
	}
	return publishSignalMessage(s.rc, s.nodeId, s.connectionId, msg)
}

func (s *SignalNodeSink) Close() {
	if !s.isClosed.TrySet(true) {
		return
	}
	publishSignalMessage(s.rc, s.nodeId, s.connectionId, &livekit.EndSession{})
	if s.onClose != nil {
		s.onClose()
	}
}

func (s *SignalNodeSink) OnClose(f func()) {
	s.onClose = f
}
