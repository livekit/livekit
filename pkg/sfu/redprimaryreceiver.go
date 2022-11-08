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

	firstPktReceived bool
	lastSeq          uint16

	// bitset for upstream packet receive history [lastSeq-8, lastSeq-1], bit 1 represents packet received
	pktHistory byte
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

	pkts, err := r.getSendPktsFromRed(pkt.Packet)
	if err != nil {
		r.logger.Errorw("get encoding for red failed", err, "payloadtype", pkt.Packet.PayloadType)
		return
	}

	for _, sendPkt := range pkts {
		pPkt := *pkt
		pPkt.Packet = sendPkt

		// not modify the ExtPacket.RawPacket here for performance since it is not used by the DownTrack,
		// otherwise it should be set to the correct value (marshal the primary rtp packet)
		r.downTrackSpreader.Broadcast(func(dt TrackSender) {
			_ = dt.WriteRTP(&pPkt, spatialLayer)
		})
	}
}

func (r *RedPrimaryReceiver) AddDownTrack(track TrackSender) error {
	if r.closed.Load() {
		return ErrReceiverClosed
	}

	if r.downTrackSpreader.HasDownTrack(track.SubscriberID()) {
		r.logger.Infow("subscriberID already exists, replacing downtrack", "subscriberID", track.SubscriberID())
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
	payload, err := extractPrimaryEncodingForRED(pkt.Payload)
	if err != nil {
		return 0, err
	}
	pkt.Payload = payload

	return pkt.MarshalTo(buf)
}

func (r *RedPrimaryReceiver) getSendPktsFromRed(rtp *rtp.Packet) ([]*rtp.Packet, error) {
	var needRecover bool
	if !r.firstPktReceived {
		r.lastSeq = rtp.SequenceNumber
		r.pktHistory = 0
		r.firstPktReceived = true
	} else {
		diff := rtp.SequenceNumber - r.lastSeq
		switch {
		case diff == 0: // duplicate
			break
		case diff > 0x8000: // unorder
			// in history
			if 65535-diff < 8 {
				r.pktHistory |= 1 << (65535 - diff)
				needRecover = true
			}

		case diff > 8: // long jump
			r.lastSeq = rtp.SequenceNumber
			r.pktHistory = 0
			needRecover = true

		default:
			r.lastSeq = rtp.SequenceNumber
			r.pktHistory = (r.pktHistory << byte(diff)) | 1<<(diff-1)
			needRecover = true
		}
	}

	var recoverBits byte
	if needRecover {
		bitIndex := r.lastSeq - rtp.SequenceNumber
		for i := 0; i < maxRedCount; i++ {
			if bitIndex > 7 {
				break
			}
			if r.pktHistory&byte(1<<bitIndex) == 0 {
				recoverBits |= byte(1 << i)
			}
			bitIndex++
		}
	}

	return extractPktsFromRed(rtp, recoverBits)
}

type block struct {
	tsOffset uint32
	length   int
	pt       uint8
	primary  bool
}

func extractPktsFromRed(redPkt *rtp.Packet, recoverBits byte) ([]*rtp.Packet, error) {
	payload := redPkt.Payload
	var blocks []block
	var blockLength int
	for {
		if payload[0]&0x80 == 0 {
			// last block is primary encoding data
			payload = payload[1:]
			blocks = append(blocks, block{primary: true})
			break
		} else {
			if len(payload) < 4 {
				// illegal data
				return nil, ErrIncompleteRedHeader
			}
			blockHead := binary.BigEndian.Uint32(payload[0:])
			length := int(blockHead & 0x03FF)
			blockHead >>= 10
			tsOffset := blockHead & 0x3FFF
			blockHead >>= 14
			pt := uint8(blockHead & 0x7F)
			payload = payload[4:]
			blockLength += length
			blocks = append(blocks, block{pt: pt, length: length, tsOffset: tsOffset})
		}
	}

	if len(payload) < blockLength {
		return nil, ErrIncompleteRedBlock
	}

	pkts := make([]*rtp.Packet, 0, len(blocks))
	for i, b := range blocks {
		if b.primary {
			pkts = append(pkts, &rtp.Packet{Header: redPkt.Header, Payload: payload})
			break
		}

		// last block is primary encoding
		recoverIndex := len(blocks) - i - 1
		if recoverIndex < 1 || recoverBits&(1<<(recoverIndex-1)) == 0 {
			payload = payload[b.length:]
			continue
		}

		header := redPkt.Header
		header.SequenceNumber -= uint16(recoverIndex)
		header.Timestamp -= b.tsOffset
		header.PayloadType = b.pt
		pkts = append(pkts, &rtp.Packet{Header: header, Payload: payload[:b.length]})
		payload = payload[b.length:]
	}

	return pkts, nil
}

func extractPrimaryEncodingForRED(payload []byte) ([]byte, error) {

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
