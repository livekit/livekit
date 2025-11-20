package client

import (
	"github.com/frostbyte73/core"
	"github.com/livekit/livekit-server/pkg/rtc/datatrack"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

type DataTrackRemote struct {
	publisherIdentity  livekit.ParticipantIdentity
	publisherID        livekit.ParticipantID
	handle             uint16
	trackID            livekit.TrackID
	numReceivedPackets int

	closed core.Fuse
}

func NewDataTrackRemote(
	publisherIdentity livekit.ParticipantIdentity,
	publisherID livekit.ParticipantID,
	handle uint16,
	trackID livekit.TrackID,
) *DataTrackRemote {
	logger.Infow(
		"creating data track remote",
		"publisherIdentity", publisherIdentity,
		"publisherID", publisherID,
		"handle", handle,
		"trackID", trackID,
	)
	return &DataTrackRemote{
		publisherIdentity: publisherIdentity,
		publisherID:       publisherID,
		handle:            handle,
		trackID:           trackID,
	}
}

func (d *DataTrackRemote) Close() {
	logger.Infow(
		"closing data track remote",
		"publisherIdentity", d.publisherIdentity,
		"publisherID", d.publisherID,
		"handle", d.handle,
		"trackID", d.trackID,
	)
	d.closed.Break()
}

func (d *DataTrackRemote) Handle() uint16 {
	return d.handle
}

func (d *DataTrackRemote) ID() livekit.TrackID {
	return d.trackID
}

func (d *DataTrackRemote) PacketReceived(packet *datatrack.Packet) {
	if d.closed.IsBroken() {
		return
	}

	valid := true
	if len(packet.Payload) == 0 {
		valid = false
	}
	for i := range len(packet.Payload) {
		if packet.Payload[i] != byte(255-i) {
			valid = false
			break
		}
	}
	if valid {
		d.numReceivedPackets++
	}
}

func (d *DataTrackRemote) NumReceivedPackets() int {
	return d.numReceivedPackets
}
