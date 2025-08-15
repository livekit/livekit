// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"

	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/protocol/logger"
)

// Writes a file to an RTP track.
// makes it easier to debug and create RTP streams
type TrackWriter struct {
	ctx      context.Context
	cancel   context.CancelFunc
	track    *webrtc.TrackLocalStaticSample
	filePath string
	mime     mime.MimeType

	ogg       *oggreader.OggReader
	ivfheader *ivfreader.IVFFileHeader
	ivf       *ivfreader.IVFReader
	h264      *h264reader.H264Reader
}

func NewTrackWriter(ctx context.Context, track *webrtc.TrackLocalStaticSample, filePath string) *TrackWriter {
	ctx, cancel := context.WithCancel(ctx)
	return &TrackWriter{
		ctx:      ctx,
		cancel:   cancel,
		track:    track,
		filePath: filePath,
		mime:     mime.NormalizeMimeType(track.Codec().MimeType),
	}
}

func (w *TrackWriter) Start() error {
	if w.filePath == "" {
		go w.writeNull()
		return nil
	}

	file, err := os.Open(w.filePath)
	if err != nil {
		return err
	}

	logger.Debugw(
		"starting track writer",
		"trackID", w.track.ID(),
		"mime", w.mime,
	)
	switch w.mime {
	case mime.MimeTypeOpus:
		w.ogg, _, err = oggreader.NewWith(file)
		if err != nil {
			return err
		}
		go w.writeOgg()
	case mime.MimeTypeVP8:
		w.ivf, w.ivfheader, err = ivfreader.NewWith(file)
		if err != nil {
			return err
		}
		go w.writeVP8()
	case mime.MimeTypeH264:
		w.h264, err = h264reader.NewReader(file)
		if err != nil {
			return err
		}
		go w.writeH264()
	}
	return nil
}

func (w *TrackWriter) Stop() {
	w.cancel()
}

func (w *TrackWriter) writeNull() {
	defer w.onWriteComplete()
	sample := media.Sample{Data: []byte{0x0, 0xff, 0xff, 0xff, 0xff}, Duration: 30 * time.Millisecond}
	h264Sample := media.Sample{Data: []byte{0x00, 0x00, 0x00, 0x01, 0x7, 0xff, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x01, 0x8, 0xff, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x01, 0x5, 0xff, 0xff, 0xff, 0xff}, Duration: 30 * time.Millisecond}
	for {
		select {
		case <-time.After(20 * time.Millisecond):
			if w.mime == mime.MimeTypeH264 {
				w.track.WriteSample(h264Sample)
			} else {
				w.track.WriteSample(sample)
			}
		case <-w.ctx.Done():
			return
		}
	}
}

func (w *TrackWriter) writeOgg() {
	// Keep track of last granule, the difference is the amount of samples in the buffer
	var lastGranule uint64
	for {
		if w.ctx.Err() != nil {
			return
		}
		pageData, pageHeader, err := w.ogg.ParseNextPage()
		if err == io.EOF {
			logger.Debugw("all audio samples parsed and sent")
			w.onWriteComplete()
			return
		}

		if err != nil {
			logger.Errorw("could not parse ogg page", err)
			return
		}

		// The amount of samples is the difference between the last and current timestamp
		sampleCount := float64(pageHeader.GranulePosition - lastGranule)
		lastGranule = pageHeader.GranulePosition
		sampleDuration := time.Duration((sampleCount/48000)*1000) * time.Millisecond

		if err = w.track.WriteSample(media.Sample{Data: pageData, Duration: sampleDuration}); err != nil {
			logger.Errorw("could not write sample", err)
			return
		}

		time.Sleep(sampleDuration)
	}
}

func (w *TrackWriter) writeVP8() {
	// Send our video file frame at a time. Pace our sending such that we send it at the same speed it should be played back as.
	// This isn't required since the video is timestamped, but we will such much higher loss if we send all at once.
	sleepTime := time.Millisecond * time.Duration((float32(w.ivfheader.TimebaseNumerator)/float32(w.ivfheader.TimebaseDenominator))*1000)
	for {
		if w.ctx.Err() != nil {
			return
		}
		frame, _, err := w.ivf.ParseNextFrame()
		if err == io.EOF {
			logger.Debugw("all video frames parsed and sent")
			w.onWriteComplete()
			return
		}

		if err != nil {
			logger.Errorw("could not parse VP8 frame", err)
			return
		}

		time.Sleep(sleepTime)
		if err = w.track.WriteSample(media.Sample{Data: frame, Duration: time.Second}); err != nil {
			logger.Errorw("could not write sample", err)
			return
		}
	}
}

func (w *TrackWriter) writeH264() {
	// TODO: this is harder
}

func (w *TrackWriter) onWriteComplete() {

}
