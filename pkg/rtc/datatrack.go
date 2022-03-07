package rtc

import (
	"errors"
	"sync"

	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v3"
	"google.golang.org/protobuf/proto"
)

type DataTrackSender interface {
	sfu.TrackSender
	Write(label string, data []byte)
}

type DataTrack struct {
	trackID       livekit.TrackID
	participantID livekit.ParticipantID
	logger        logger.Logger
	lock          sync.RWMutex
	downTracks    []DataTrackSender
	onDataPacket  func(*livekit.DataPacket)
	onClose       []func()
}

func NewDataTrack(trackID livekit.TrackID, participantID livekit.ParticipantID, logger logger.Logger) *DataTrack {
	t := &DataTrack{
		trackID:       trackID,
		participantID: participantID,
		logger:        logger,
	}
	return t
}

func (t *DataTrack) onData(label string, data []byte) {
	t.lock.RLock()
	f := t.onDataPacket
	dts := t.downTracks
	t.lock.RUnlock()

	for _, dt := range dts {
		dt.Write(label, data)
	}

	if f != nil {
		dp, err := DataPacketFromBytes(label, data)
		if err != nil {
			t.logger.Warnw("invalid data", err, "label", label)
			return
		}
		// only forward on user payloads
		switch payload := dp.Value.(type) {
		case *livekit.DataPacket_User:
			payload.User.ParticipantSid = string(t.participantID)
			f(dp)
		default:
			t.logger.Warnw("received unsupported data packet", nil, "payload", payload)
		}
	}
}

func (t *DataTrack) OnDataPacket(f func(*livekit.DataPacket)) {
	t.lock.Lock()
	t.onDataPacket = f
	t.lock.Unlock()
}

func (t *DataTrack) TrackID() livekit.TrackID {
	return t.trackID
}

func (t *DataTrack) Write(label string, data []byte) {
	t.onData(label, data)
}

func (t *DataTrack) AddDownTrack(dt sfu.TrackSender) error {
	dataDt, ok := dt.(DataTrackSender)
	if !ok {
		return errors.New("invalid DownTrack type, expect DataTrackSender")
	}
	t.lock.Lock()
	defer t.lock.Unlock()
	t.downTracks = append(t.downTracks, dataDt)
	return nil
}

func (t *DataTrack) DeleteDownTrack(peerID livekit.ParticipantID) {
	t.lock.Lock()
	defer t.lock.Unlock()
	for k, v := range t.downTracks {
		if v.PeerID() == peerID {
			t.downTracks[k] = t.downTracks[len(t.downTracks)-1]
			t.downTracks = t.downTracks[:len(t.downTracks)-1]
			break
		}
	}
}

func (t *DataTrack) AddOnClose(f func()) {
	if f == nil {
		return
	}
	t.lock.Lock()
	t.onClose = append(t.onClose, f)
	t.lock.Unlock()
}

func (t *DataTrack) Close() {
	t.lock.Lock()
	fs := t.onClose
	t.lock.Unlock()

	for _, f := range fs {
		f()
	}
}

func (t *DataTrack) Receiver() sfu.TrackReceiver {
	return t
}

func (t *DataTrack) ToProto() *livekit.TrackInfo {
	return &livekit.TrackInfo{
		Sid:  string(t.trackID),
		Type: livekit.TrackType_DATA,
	}
}

func DataPacketFromBytes(label string, data []byte) (*livekit.DataPacket, error) {
	dp := livekit.DataPacket{}
	if err := proto.Unmarshal(data, &dp); err != nil {
		return nil, err
	}

	switch label {
	case reliableDataChannel:
		dp.Kind = livekit.DataPacket_RELIABLE
	case lossyDataChannel:
		dp.Kind = livekit.DataPacket_LOSSY
	default:
		return nil, errors.New("unsupported datachannel added")
	}

	return &dp, nil
}

func (t *DataTrack) Kind() livekit.TrackType {
	return livekit.TrackType_DATA
}

//---------------------------------------------
// no op methods for sfu.TrackReceiver
func (t *DataTrack) StreamID() string {
	return ""
}

func (t *DataTrack) Codec() webrtc.RTPCodecCapability {
	return webrtc.RTPCodecCapability{}
}

func (t *DataTrack) ReadRTP(buf []byte, layer uint8, sn uint16) (int, error) {
	return 0, nil
}

func (t *DataTrack) GetSenderReportTime(layer int32) (rtpTS uint32, ntpTS uint64) {
	return
}

func (t *DataTrack) GetBitrateTemporalCumulative() buffer.Bitrates {
	return buffer.Bitrates{}
}

func (t *DataTrack) SendPLI(layer int32) {
}

func (t *DataTrack) LastPLI() int64 {
	return 0
}

func (t *DataTrack) SetUpTrackPaused(paused bool) {

}

func (t *DataTrack) SetMaxExpectedSpatialLayer(layer int32) {

}

func (t *DataTrack) DebugInfo() map[string]interface{} {
	return map[string]interface{}{}
}
