package client

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/h264reader"
	"github.com/pion/webrtc/v3/pkg/media/ivfreader"
	"github.com/pion/webrtc/v3/pkg/media/oggreader"

	"github.com/livekit/livekit-server/pkg/logger"
)

type TrackWriter struct {
	ctx      context.Context
	track    *webrtc.Track
	filePath string
	format   string

	ogg       *oggreader.OggReader
	ivfheader *ivfreader.IVFFileHeader
	ivf       *ivfreader.IVFReader
	h264      *h264reader.H264Reader
}

func NewTrackWriter(ctx context.Context, track *webrtc.Track, filePath string, format string) *TrackWriter {
	return &TrackWriter{
		ctx:      ctx,
		track:    track,
		filePath: filePath,
		format:   format,
	}
}

func (w *TrackWriter) Start() error {
	file, err := os.Open(w.filePath)
	if err != nil {
		return err
	}

	logger.GetLogger().Infow("starting track writer",
		"track", w.track.ID(),
		"format", w.format)
	switch w.format {
	case webrtc.Opus:
		w.ogg, _, err = oggreader.NewWith(file)
		if err != nil {
			return err
		}
		go w.writeOgg()
	case webrtc.VP8:
		w.ivf, w.ivfheader, err = ivfreader.NewWith(file)
		if err != nil {
			return err
		}
		go w.writeVP8()
	case webrtc.H264:
		w.h264, err = h264reader.NewReader(file)
		if err != nil {
			return err
		}
		go w.writeH264()
	}
	return nil
}

func (w *TrackWriter) writeOgg() {
	// Keep track of last granule, the difference is the amount of samples in the buffer
	var lastGranule uint64
	for {
		pageData, pageHeader, err := w.ogg.ParseNextPage()
		if err == io.EOF {
			logger.GetLogger().Infow("all audio samples parsed and sent")
			w.onWriteComplete()
			return
		}

		if err != nil {
			logger.GetLogger().Errorw("could not parse ogg page", "err", err)
			return
		}

		// The amount of samples is the difference between the last and current timestamp
		sampleCount := float64(pageHeader.GranulePosition - lastGranule)
		lastGranule = pageHeader.GranulePosition

		if err = w.track.WriteSample(media.Sample{Data: pageData, Samples: uint32(sampleCount)}); err != nil {
			logger.GetLogger().Errorw("could not write sample", "err", err)
			return
		}

		// Convert seconds to Milliseconds, Sleep doesn't accept floats
		time.Sleep(time.Duration((sampleCount/48000)*1000) * time.Millisecond)
	}
}

func (w *TrackWriter) writeVP8() {
	// Send our video file frame at a time. Pace our sending so we send it at the same speed it should be played back as.
	// This isn't required since the video is timestamped, but we will such much higher loss if we send all at once.
	sleepTime := time.Millisecond * time.Duration((float32(w.ivfheader.TimebaseNumerator)/float32(w.ivfheader.TimebaseDenominator))*1000)
	for {
		frame, _, err := w.ivf.ParseNextFrame()
		if err == io.EOF {
			logger.GetLogger().Infow("all video frames parsed and sent")
			w.onWriteComplete()
			return
		}

		if err != nil {
			logger.GetLogger().Errorw("could not parse VP8 frame", "err", err)
			return
		}

		time.Sleep(sleepTime)
		if err = w.track.WriteSample(media.Sample{Data: frame, Samples: 90000}); err != nil {
			logger.GetLogger().Errorw("could not write sample", "err", err)
			return
		}
	}
}

func (w *TrackWriter) writeH264() {
	// TODO: this is harder
}

func (w *TrackWriter) onWriteComplete() {

}
