package sfu

import (
	"encoding/binary"
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

type dummyDowntrack struct {
	TrackSender
	pkt *rtp.Packet
}

func (dt *dummyDowntrack) WriteRTP(p *buffer.ExtPacket, _ int32) error {
	dt.pkt = p.Packet
	return nil
}

func TestRedReceiver(t *testing.T) {

	dt := &dummyDowntrack{TrackSender: &DownTrack{}}
	tsStep := uint32(48000 / 1000 * 10)

	t.Run("normal", func(t *testing.T) {
		w := &WebRTCReceiver{isRED: true}
		require.Equal(t, w.GetRedReceiver(), w)
		w.isRED = false
		red := w.GetRedReceiver().(*RedReceiver)
		require.NotNil(t, red)
		require.NoError(t, red.AddDownTrack(dt))

		header := rtp.Header{SequenceNumber: 65534, Timestamp: (uint32(1) << 31) - 2*tsStep, PayloadType: 111}
		expectPkt := make([]*rtp.Packet, 0, maxRedCount+1)
		for i := 0; i < 10; i++ {
			hbuf, _ := header.Marshal()
			pkt1 := &rtp.Packet{
				Header:  header,
				Payload: hbuf,
			}
			expectPkt = append(expectPkt, pkt1)
			if len(expectPkt) > maxRedCount+1 {
				expectPkt = expectPkt[1:]
			}
			red.ForwardRTP(&buffer.ExtPacket{
				Packet: pkt1,
			}, 0)
			verifyRedEncodings(t, dt.pkt, expectPkt)
			header.SequenceNumber++
			header.Timestamp += tsStep
		}
	})

	t.Run("packet lost and jump", func(t *testing.T) {
		w := &WebRTCReceiver{}
		red := w.GetRedReceiver().(*RedReceiver)
		require.NoError(t, red.AddDownTrack(dt))

		header := rtp.Header{SequenceNumber: 65534, Timestamp: (uint32(1) << 31) - 2*tsStep, PayloadType: 111}
		expectPkt := make([]*rtp.Packet, 0, maxRedCount+1)
		for i := 0; i < 10; i++ {
			if i%2 == 0 {
				header.SequenceNumber++
				header.Timestamp += tsStep
				expectPkt = append(expectPkt, nil)
				continue
			}
			hbuf, _ := header.Marshal()
			pkt1 := &rtp.Packet{
				Header:  header,
				Payload: hbuf,
			}
			expectPkt = append(expectPkt, pkt1)
			if len(expectPkt) > maxRedCount+1 {
				expectPkt = expectPkt[len(expectPkt)-maxRedCount-1:]
			}
			red.ForwardRTP(&buffer.ExtPacket{
				Packet: pkt1,
			}, 0)
			verifyRedEncodings(t, dt.pkt, expectPkt)
			header.SequenceNumber++
			header.Timestamp += tsStep
		}

		// jump
		header.SequenceNumber += 10
		header.Timestamp += 10 * tsStep

		expectPkt = expectPkt[:0]
		for i := 0; i < 3; i++ {
			hbuf, _ := header.Marshal()
			pkt1 := &rtp.Packet{
				Header:  header,
				Payload: hbuf,
			}
			expectPkt = append(expectPkt, pkt1)
			if len(expectPkt) > maxRedCount+1 {
				expectPkt = expectPkt[len(expectPkt)-maxRedCount-1:]
			}
			red.ForwardRTP(&buffer.ExtPacket{
				Packet: pkt1,
			}, 0)
			verifyRedEncodings(t, dt.pkt, expectPkt)
			header.SequenceNumber++
			header.Timestamp += tsStep
		}

	})
	t.Run("unorder and repeat", func(t *testing.T) {
		w := &WebRTCReceiver{}
		red := w.GetRedReceiver().(*RedReceiver)
		require.NoError(t, red.AddDownTrack(dt))

		header := rtp.Header{SequenceNumber: 65534, Timestamp: (uint32(1) << 31) - 2*tsStep, PayloadType: 111}
		var prevPkts []*rtp.Packet
		for i := 0; i < 10; i++ {
			hbuf, _ := header.Marshal()
			pkt1 := &rtp.Packet{
				Header:  header,
				Payload: hbuf,
			}
			red.ForwardRTP(&buffer.ExtPacket{
				Packet: pkt1,
			}, 0)
			header.SequenceNumber++
			header.Timestamp += tsStep
			prevPkts = append(prevPkts, pkt1)
		}

		// old unorder data don't have red records
		expectPkt := prevPkts[len(prevPkts)-3 : len(prevPkts)-2]
		red.ForwardRTP(&buffer.ExtPacket{
			Packet: expectPkt[0],
		}, 0)
		verifyRedEncodings(t, dt.pkt, expectPkt)

		// repeat packet only have 1 red records
		expectPkt = prevPkts[len(prevPkts)-2:]
		red.ForwardRTP(&buffer.ExtPacket{
			Packet: expectPkt[1],
		}, 0)
		verifyRedEncodings(t, dt.pkt, expectPkt)
	})
}

func verifyRedEncodings(t *testing.T, red *rtp.Packet, redPkts []*rtp.Packet) {
	solidPkts := make([]*rtp.Packet, 0, len(redPkts))
	for _, pkt := range redPkts {
		if pkt != nil {
			solidPkts = append(solidPkts, pkt)
		}
	}
	pktsFromRed, err := extractPktsFromRed(red)
	require.NoError(t, err)
	require.Len(t, pktsFromRed, len(solidPkts))
	for i, pkt := range pktsFromRed {
		verifyEncodingEqual(t, pkt, solidPkts[i])
	}
}

func verifyEncodingEqual(t *testing.T, p1, p2 *rtp.Packet) {
	require.Equal(t, p1.Header.Timestamp, p2.Header.Timestamp)
	require.Equal(t, p1.PayloadType, p2.PayloadType)
	require.EqualValues(t, p1.Payload, p2.Payload)
}

type block struct {
	tsOffset uint32
	length   int
	pt       uint8
}

func extractPktsFromRed(redPkt *rtp.Packet) ([]*rtp.Packet, error) {
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

	payload := redPkt.Payload
	var blocks []block
	var blockLength int
	for {
		if payload[0]&0x80 == 0 {
			// last block is primary encoding data
			payload = payload[1:]
			blocks = append(blocks, block{})
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
	for _, b := range blocks {
		if b.tsOffset == 0 {
			pkts = append(pkts, &rtp.Packet{Header: redPkt.Header, Payload: payload})
			break
		}
		header := redPkt.Header
		header.Timestamp -= b.tsOffset
		header.PayloadType = b.pt
		pkts = append(pkts, &rtp.Packet{Header: header, Payload: payload[:b.length]})
		payload = payload[b.length:]
	}

	return pkts, nil
}
