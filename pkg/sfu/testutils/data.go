package testutils

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"

	"github.com/pion/rtp"
)

//-----------------------------------------------------------

type TestExtPacketParams struct {
	SetMarker      bool
	SetPadding     bool
	IsHead         bool
	IsKeyFrame     bool
	PayloadType    uint8
	SequenceNumber uint16
	Timestamp      uint32
	SSRC           uint32
	PayloadSize    int
	PaddingSize    int
	ArrivalTime    int64
}

//-----------------------------------------------------------

func GetTestExtPacket(params *TestExtPacketParams) (*buffer.ExtPacket, error) {
	packet := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Padding:        params.SetPadding,
			Marker:         params.SetMarker,
			PayloadType:    params.PayloadType,
			SequenceNumber: params.SequenceNumber,
			Timestamp:      params.Timestamp,
			SSRC:           params.SSRC,
		},
		Payload: make([]byte, params.PayloadSize),
		// LK-TODO need a newer version of pion/rtp PaddingSize: params.PaddingSize,
	}

	raw, err := packet.Marshal()
	if err != nil {
		return nil, err
	}

	ep := &buffer.ExtPacket{
		Head:      params.IsHead,
		Arrival:   params.ArrivalTime,
		Packet:    packet,
		KeyFrame:  params.IsKeyFrame,
		RawPacket: raw,
	}

	return ep, nil
}

//--------------------------------------
