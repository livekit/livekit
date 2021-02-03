package rtc

import (
	"sync"
	"testing"
	"time"

	"github.com/gammazero/workerpool"
	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/rtcp"
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
		packet := buffer.ExtPacket{}
		receiver.RTPChanReturns(packetGenerator(packet))

		dt := &typesfakes.FakeDownTrack{}
		mt.downtracks["test"] = dt

		mt.Start()
		time.Sleep(testWaitDuration)

		assert.Equal(t, 1, dt.WriteRTPCallCount(), "WriteRTP wasn't called on Forwarder")
		assert.EqualValues(t, packet.Packet, dt.WriteRTPArgsForCall(0))
	})

	t.Run("muted tracks do not forward data", func(t *testing.T) {
		mt := newMediaTrackWithReceiver()
		mt.muted.TrySet(true)

		dt := &typesfakes.FakeDownTrack{}
		mt.downtracks["test"] = dt

		mt.Start()
		time.Sleep(testWaitDuration)
		assert.Zero(t, dt.WriteRTPCallCount())
	})
}

func TestMissingKeyFrames(t *testing.T) {
	t.Run("PLI packet is sent when forwarder misses keyframe", func(t *testing.T) {
		mt := newMediaTrackWithReceiver()

		dt := &typesfakes.FakeDownTrack{}
		mt.downtracks["test"] = dt
		dt.WriteRTPReturns(sfu.ErrRequiresKeyFrame)

		mt.Start()
		time.Sleep(testWaitDuration)

		select {
		case o := <-mt.rtcpCh.ReadChan():
			if o == nil {
				return
			}
			pkts, ok := o.([]rtcp.Packet)
			assert.True(t, ok)
			assert.Len(t, pkts, 1, "a single RTCP packet should be returned")
			assert.IsType(t, &rtcp.PictureLossIndication{}, pkts[0])
		default:
			t.Fatalf("did not receive RTCP packets")
		}
	})
}

// returns a receiver that reads a packet then returns EOF
func newMediaTrackWithReceiver() *MediaTrack {
	packet := buffer.ExtPacket{}
	receiver := &typesfakes.FakeReceiver{
		RTPChanStub: func() <-chan buffer.ExtPacket {
			return packetGenerator(packet)
		},
	}
	return &MediaTrack{
		id:            utils.NewGuid(utils.TrackPrefix),
		participantId: "PAtest",
		kind:          livekit.TrackType_VIDEO,
		codec:         webrtc.RTPCodecParameters{},
		rtcpCh:        utils.NewCalmChannel(5),
		lock:          sync.RWMutex{},
		once:          sync.Once{},
		downtracks:    map[string]types.DownTrack{},
		receiver:      receiver,
		nackWorker:    workerpool.New(1),
	}
}

func packetGenerator(packets ...buffer.ExtPacket) <-chan buffer.ExtPacket {
	pc := make(chan buffer.ExtPacket)
	go func() {
		defer close(pc)
		for _, p := range packets {
			pc <- p
		}
	}()
	return pc
}
