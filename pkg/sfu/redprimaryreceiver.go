package sfu

import (
	"encoding/binary"
	"errors"

	"go.uber.org/atomic"

	"github.com/pion/rtp"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	MimeTypeAudioRed = "audio/red"
)

var (
	ErrIncompleteRedHeader = errors.New("incomplete red block header")
	ErrIncompleteRedBlock  = errors.New("incomplete red block payload")
)

type RedPrimaryReceiver struct {
	TrackReceiver
	downTrackSpreader *DownTrackSpreader
	logger            logger.Logger
	closed            atomic.Bool
}

func NewRedPrimaryReceiver(receiver TrackReceiver, dsp DownTrackSpreaderParams) *RedPrimaryReceiver {
	return &RedPrimaryReceiver{
		TrackReceiver:     receiver,
		downTrackSpreader: NewDownTrackSpreader(dsp),
		logger:            dsp.Logger,
	}
}

func (r *RedPrimaryReceiver) ForwardRTP(pkt *buffer.ExtPacket, spatialLayer int32) {
	// extract primary payload from RED and forward to downtracks
	if r.downTrackSpreader.DownTrackCount() == 0 {
		return
	}
	payload, err := ExtractPrimaryEncodingForRED(pkt.Packet.Payload)
	if err != nil {
		r.logger.Errorw("get primary encoding for red failed", err, "payloadtype", pkt.Packet.PayloadType)
		return
	}

	pPkt := *pkt
	primaryRtpPacket := *pkt.Packet
	primaryRtpPacket.Payload = payload
	pPkt.Packet = &primaryRtpPacket

	// not modify the ExtPacket.RawPacket here for performance since it is not used by the DownTrack,
	// otherwise it should be set to the correct value (marshal the primary rtp packet)

	r.downTrackSpreader.Broadcast(func(dt TrackSender) {
		_ = dt.WriteRTP(&pPkt, spatialLayer)
	})

	// TODO : detect rtp packet lost, recover it from the redundant payload then send to downstreams.
}

func (r *RedPrimaryReceiver) AddDownTrack(track TrackSender) error {
	if r.closed.Load() {
		return ErrReceiverClosed
	}

	if r.downTrackSpreader.HasDownTrack(track.SubscriberID()) {
		r.logger.Infow("subscriberID already exists, replace the downtrack", "subscriberID", track.SubscriberID())
	}

	r.downTrackSpreader.Store(track)
	return nil
}

func (r *RedPrimaryReceiver) DeleteDownTrack(subscriberID livekit.ParticipantID) {
	if r.closed.Load() {
		return
	}

	r.downTrackSpreader.Free(subscriberID)
}

func (r *RedPrimaryReceiver) CanClose() bool {
	return r.closed.Load() || r.downTrackSpreader.DownTrackCount() == 0
}

func (r *RedPrimaryReceiver) Close() {
	r.closed.Store(true)
	for _, dt := range r.downTrackSpreader.ResetAndGetDownTracks() {
		dt.Close()
	}
}

func (r *RedPrimaryReceiver) ReadRTP(buf []byte, layer uint8, sn uint16) (int, error) {
	n, err := r.TrackReceiver.ReadRTP(buf, layer, sn)
	if err != nil {
		return n, err
	}

	var pkt rtp.Packet
	pkt.Unmarshal(buf[:n])
	payload, err := ExtractPrimaryEncodingForRED(pkt.Payload)
	if err != nil {
		return 0, err
	}
	pkt.Payload = payload

	return pkt.MarshalTo(buf)
}

func ExtractPrimaryEncodingForRED(payload []byte) ([]byte, error) {

	/* RED payload https://datatracker.ietf.org/doc/html/rfc2198#section-3
		0                   1                    2                   3
	    0 1 2 3 4 5 6 7 8 9 0 1 2 3  4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |F|   block PT  |  timestamp offset         |   block length    |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   F: 1 bit First bit in header indicates whether another header block
	       follows.  If 1 further header blocks follow, if 0 this is the
	       last header block.
	*/

	var blockLength int
	for {
		if payload[0]&0x80 == 0 {
			// last block is primary encoding data
			payload = payload[1:]
			break
		} else {
			if len(payload) < 4 {
				// illegal data
				return nil, ErrIncompleteRedHeader
			}
			blockLength += int(binary.BigEndian.Uint16(payload[2:]) & 0x03FF)
			payload = payload[4:]
		}
	}

	if len(payload) < blockLength {
		return nil, ErrIncompleteRedBlock
	}

	return payload[blockLength:], nil
}
