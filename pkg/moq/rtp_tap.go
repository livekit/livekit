// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package moq

import (
	"sync"
	"time"

	"github.com/livekit/mediatransportutil"
	"github.com/livekit/protocol/livekit"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/livekit-server/pkg/moq/media"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

type moqSample struct {
	frame media.Frame
	time  uint64
}
type moqTap struct {
	id       string
	rend     *moqRendition
	frames   chan moqSample
	done     chan struct{}
	once     sync.Once
	mu       sync.Mutex
	asm      *media.Assembler
	needKey  bool
	started  bool
	baseRTP  uint64
	baseTime int64
}

func newMoQTap(rend *moqRendition, id string) *moqTap {
	t := &moqTap{id: id, rend: rend, frames: make(chan moqSample, 4), done: make(chan struct{})}
	t.Resync()
	return t
}

// Upstream RTP can restart at an earlier sequence number and timestamp while
// the downstream MoQ subscription remains open.
func (t *moqTap) Resync() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.started = false
	t.needKey = t.rend.catalog.Role == media.RoleVideo
	t.asm = media.NewAssembler(t.rend.codec, media.AssemblerOptions{OnLoss: func() {
		t.needKey = true
		t.rend.receiver.SendPLI(0, false)
	}})
	for {
		select {
		case <-t.frames:
		default:
			return
		}
	}
}

func (t *moqTap) WriteRTP(ext *buffer.ExtPacket, layer int32) int32 {
	if layer != 0 || ext == nil || ext.Packet == nil || t.IsClosed() {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var forwarded int32
	for _, f := range t.asm.Push(media.PacketFromExt(ext, t.rend.codec, nil, false), time.Now()) {
		video := t.rend.catalog.Role == media.RoleVideo
		if video && t.needKey && !f.Keyframe {
			continue
		}
		if len(f.Data) > maxMoQFrame {
			t.needKey = video
			continue
		}
		if !t.started {
			t.baseRTP, t.baseTime, t.started = f.ExtTS, time.Now().UnixMicro(), true
		}
		us := t.baseTime + (int64(f.ExtTS)-int64(t.baseRTP))*1_000_000/int64(t.rend.receiver.Codec().ClockRate)
		select {
		case t.frames <- moqSample{f, uint64(us)}:
			forwarded = 1
			t.needKey = false
		default:
			t.needKey = video
			if video {
				t.rend.receiver.SendPLI(0, false)
			}
		}
	}
	return forwarded
}
func (t *moqTap) HandleRTCPSenderReportData(_ webrtc.PayloadType, layer int32, sr *livekit.RTCPSenderReportState) error {
	if layer == 0 && sr != nil && sr.NtpTimestamp != 0 {
		t.mu.Lock()
		t.baseTime, t.baseRTP, t.started = mediatransportutil.NtpTime(sr.NtpTimestamp).Time().UnixMicro(), sr.RtpTimestampExt, true
		t.mu.Unlock()
	}
	return nil
}
func (t *moqTap) ID() string { return t.id }
func (t *moqTap) waitingForKeyframe() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.needKey
}
func (t *moqTap) SubscriberID() livekit.ParticipantID { return livekit.ParticipantID(t.id) }
func (t *moqTap) Close()                              { t.once.Do(func() { close(t.done) }) }
func (t *moqTap) IsClosed() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}
func (t *moqTap) ReceiverRestart(sfu.TrackReceiver)          { t.Resync() }
func (t *moqTap) SetReceiver(sfu.TrackReceiver)              {}
func (t *moqTap) UpTrackLayersChange()                       {}
func (t *moqTap) UpTrackBitrateAvailabilityChange()          {}
func (t *moqTap) UpTrackMaxPublishedLayerChange(int32)       {}
func (t *moqTap) UpTrackMaxTemporalLayerSeenChange(int32)    {}
func (t *moqTap) UpTrackBitrateReport([]int32, sfu.Bitrates) {}
