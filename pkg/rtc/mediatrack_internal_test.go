package rtc

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	testWaitDuration = 10 * time.Millisecond
)

func TestForwardRTP(t *testing.T) {
	t.Run("ensure that forwarders are getting packets", func(t *testing.T) {
		mt := newMediaTrackWithReceiver()
		receiver := mt.receiver.(*typesfakes.FakeReceiver)
		packet := &rtp.Packet{}
		receiver.ReadRTPReturnsOnCall(0, packet, nil)

		forwarder := &typesfakes.FakeForwarder{}
		mt.forwarders["test"] = forwarder

		mt.Start()
		time.Sleep(testWaitDuration)

		assert.Equal(t, 2, receiver.ReadRTPCallCount(), "worker didn't call ReadRTP twice")
		assert.Equal(t, 1, forwarder.WriteRTPCallCount(), "WriteRTP wasn't called on Forwarder")
		assert.Equal(t, packet, forwarder.WriteRTPArgsForCall(0))
	})

	t.Run("muted tracks do not forward data", func(t *testing.T) {
		mt := newMediaTrackWithReceiver()
		mt.muted = true

		forwarder := &typesfakes.FakeForwarder{}
		mt.forwarders["test"] = forwarder

		mt.Start()
		time.Sleep(testWaitDuration)
		assert.Zero(t, forwarder.WriteRTPCallCount())
	})
}

func TestMissingKeyFrames(t *testing.T) {
	t.Run("PLI packet is sent when forwarder misses keyframe", func(t *testing.T) {
		mt := newMediaTrackWithReceiver()

		forwarder := &typesfakes.FakeForwarder{}
		mt.forwarders["test"] = forwarder
		forwarder.WriteRTPReturns(sfu.ErrRequiresKeyFrame)

		mt.Start()
		time.Sleep(testWaitDuration)

		select {
		case pkts := <-mt.rtcpCh:
			assert.Len(t, pkts, 1, "a single RTCP packet should be returned")
			assert.IsType(t, &rtcp.PictureLossIndication{}, pkts[0])
		default:
			t.Fatalf("did not receive RTCP packets")
		}
	})
}

// returns a receiver that reads a packet then returns EOF
func newMediaTrackWithReceiver() *MediaTrack {
	packet := &rtp.Packet{}
	receiver := &typesfakes.FakeReceiver{}
	receiver.ReadRTPReturnsOnCall(0, packet, nil)
	receiver.ReadRTPReturnsOnCall(1, nil, io.EOF)
	return &MediaTrack{
		ctx:           context.Background(),
		id:            utils.NewGuid(utils.TrackPrefix),
		participantId: "PAtest",
		muted:         false,
		kind:          livekit.TrackInfo_VIDEO,
		codec:         webrtc.RTPCodecParameters{},
		rtcpCh:        make(chan []rtcp.Packet, 5),
		lock:          sync.RWMutex{},
		once:          sync.Once{},
		forwarders:    map[string]types.Forwarder{},
		receiver:      receiver,
	}
}
