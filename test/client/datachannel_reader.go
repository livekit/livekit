package client

import (
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/datachannel"
)

type DataChannelReader struct {
	bitrate *datachannel.BitrateCalculator
	target  int
}

func NewDataChannelReader(bitrate int) *DataChannelReader {
	return &DataChannelReader{
		target:  bitrate,
		bitrate: datachannel.NewBitrateCalculator(datachannel.BitrateDuration*5, datachannel.BitrateWindow),
	}
}

func (d *DataChannelReader) Read(p []byte, sid string) {
	for {
		if bitrate, ok := d.bitrate.ForceBitrate(time.Now()); ok && bitrate > 0 && bitrate > d.target {
			time.Sleep(10 * time.Millisecond)
			d.bitrate.AddBytes(0, 0, time.Now())
			continue
		}
		break
	}
	d.bitrate.AddBytes(len(p), 0, time.Now())
}
