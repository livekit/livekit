package sfu

import (
	"bytes"
	"encoding/binary"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/pion/ion-log"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

// DownTrack  implements TrackLocal, is the track used to write packets
// to SFU Subscriber, the track handle the packets for simple, simulcast
// and SVC Publisher.
type DownTrack struct {
	id       string
	bound    atomicBool
	mime     string
	nList    *nackList
	ssrc     uint32
	payload  uint8
	streamID string

	//enabled  atomicBool
	reSync   atomicBool
	snOffset uint16
	tsOffset uint32
	lastSSRC uint32
	lastSN   uint16
	lastTS   uint32

	codec          webrtc.RTPCodecCapability
	transceiver    *webrtc.RTPTransceiver
	writeStream    webrtc.TrackLocalWriter
	onCloseHandler func()
	onBind         func()
	closeOnce      sync.Once

	// Report helpers
	octetCount   uint32
	packetCount  uint32
	maxPacketTs  uint32
	lastPacketMs int64
}

// NewDownTrack returns a DownTrack.
func NewDownTrack(c webrtc.RTPCodecCapability, id, streamID string) (*DownTrack, error) {
	return &DownTrack{
		id:       id,
		nList:    newNACKList(),
		codec:    c,
		streamID: streamID,
	}, nil
}

// Bind is called by the PeerConnection after negotiation is complete
// This asserts that the code requested is supported by the remote peer.
// If so it setups all the state (SSRC and PayloadType) to have a call
func (d *DownTrack) Bind(t webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	parameters := webrtc.RTPCodecParameters{RTPCodecCapability: d.codec}
	if codec, err := codecParametersFuzzySearch(parameters, t.CodecParameters()); err == nil {
		d.ssrc = uint32(t.SSRC())
		d.payload = uint8(codec.PayloadType)
		d.writeStream = t.WriteStream()
		d.mime = strings.ToLower(codec.MimeType)
		d.bound.set(true)
		d.reSync.set(true)
		if d.onBind != nil {
			d.onBind()
		}
		return codec, nil
	}
	return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
}

// Unbind implements the teardown logic when the track is no longer needed. This happens
// because a track has been stopped.
func (d *DownTrack) Unbind(_ webrtc.TrackLocalContext) error {
	d.bound.set(false)
	return nil
}

func (d *DownTrack) IsBound() bool {
	return d.bound.get()
}

// ID is the unique identifier for this Track. This should be unique for the
// stream, but doesn't have to globally unique. A common example would be 'audio' or 'video'
// and StreamID would be 'desktop' or 'webcam'
func (d *DownTrack) ID() string { return d.id }

// Codec returns current track codec capability
func (d *DownTrack) Codec() webrtc.RTPCodecCapability { return d.codec }

// StreamID is the group this track belongs too. This must be unique
func (d *DownTrack) StreamID() string { return d.streamID }

// Kind controls if this TrackLocal is audio or video
func (d *DownTrack) Kind() webrtc.RTPCodecType {
	switch {
	case strings.HasPrefix(d.codec.MimeType, "audio/"):
		return webrtc.RTPCodecTypeAudio
	case strings.HasPrefix(d.codec.MimeType, "video/"):
		return webrtc.RTPCodecTypeVideo
	default:
		return webrtc.RTPCodecType(0)
	}
}

// WriteRTP writes a RTP Packet to the DownTrack
func (d *DownTrack) WriteRTP(p *rtp.Packet) error {
	if !d.IsBound() {
		return nil
	}
	return d.writeSimpleRTP(*p)
}

func (d *DownTrack) CreateSourceDescriptionChunks() []rtcp.SourceDescriptionChunk {
	if !d.IsBound() {
		return nil
	}
	return []rtcp.SourceDescriptionChunk{
		{
			Source: d.ssrc,
			Items: []rtcp.SourceDescriptionItem{{
				Type: rtcp.SDESCNAME,
				Text: d.streamID,
			}},
		}, {
			Source: d.ssrc,
			Items: []rtcp.SourceDescriptionItem{{
				Type: rtcp.SDESType(15),
				Text: d.transceiver.Mid(),
			}},
		},
	}
}

func (d *DownTrack) CreateSenderReport() *rtcp.SenderReport {
	now := time.Now().UnixNano()
	nowNTP := timeToNtp(now)
	lastPktMs := atomic.LoadInt64(&d.lastPacketMs)
	maxPktTs := atomic.LoadUint32(&d.lastTS)
	diffTs := uint32((now/1e6)-lastPktMs) * d.codec.ClockRate / 1000
	octets, packets := d.getSRStats()
	return &rtcp.SenderReport{
		SSRC:        d.ssrc,
		NTPTime:     nowNTP,
		RTPTime:     maxPktTs + diffTs,
		PacketCount: packets,
		OctetCount:  octets,
	}
}

// Close track
func (d *DownTrack) Close() {
	d.closeOnce.Do(func() {
		if d.onCloseHandler != nil {
			d.onCloseHandler()
		}
	})
}

// OnCloseHandler method to be called on remote tracked removed
func (d *DownTrack) OnCloseHandler(fn func()) {
	d.onCloseHandler = fn
}

func (d *DownTrack) OnBind(fn func()) {
	d.onBind = fn
}

// -- start -- accessors to avoid accessing private vars

func (d *DownTrack) SetTransceiver(transceiver *webrtc.RTPTransceiver) {
	d.transceiver = transceiver
}

func (d *DownTrack) RTPSender() *webrtc.RTPSender {
	return d.transceiver.Sender()
}

func (d *DownTrack) SSRC() uint32 {
	return d.ssrc
}

func (d *DownTrack) LastSSRC() uint32 {
	return d.lastSSRC
}

func (d *DownTrack) SnOffset() uint16 {
	return d.snOffset
}

func (d *DownTrack) TsOffset() uint32 {
	return d.tsOffset
}

func (d *DownTrack) GetNACKSeqNo(seqNo []uint16) []uint16 {
	return d.nList.getNACKSeqNo(seqNo)
}

// -- end -- accessors to avoid accessing private vars

func (d *DownTrack) writeSimpleRTP(pkt rtp.Packet) error {
	if d.reSync.get() {
		if d.Kind() == webrtc.RTPCodecTypeVideo {
			relay := false
			// Wait for a keyframe to sync new source
			switch d.mime {
			case "video/vp8":
				vp8Packet := VP8Helper{}
				if err := vp8Packet.Unmarshal(pkt.Payload); err == nil {
					relay = vp8Packet.IsKeyFrame
				}
			case "video/h264":
				var word uint32
				payload := bytes.NewReader(pkt.Payload)
				err := binary.Read(payload, binary.BigEndian, &word)
				if err != nil || (word&0x1F000000)>>24 != 24 {
					relay = false
				} else {
					relay = word&0x1F == 7
				}
			}
			if !relay {
				// when we are writing to a new client and there isn't a keyframe, it makes it impossible
				// for clients to render a frame. we'll send an error for the track writer.
				return ErrRequiresKeyFrame
			}
		}
		d.snOffset = pkt.SequenceNumber - d.lastSN - 1
		d.tsOffset = pkt.Timestamp - d.lastTS - 1
		d.lastSSRC = pkt.SSRC
		d.reSync.set(false)
	}

	atomic.AddUint32(&d.octetCount, uint32(len(pkt.Payload)))
	atomic.AddUint32(&d.packetCount, 1)

	d.lastSSRC = pkt.SSRC
	newSN := pkt.SequenceNumber - d.snOffset
	newTS := pkt.Timestamp - d.tsOffset
	if (newSN-d.lastSN)&0x8000 == 0 || d.lastSN == 0 {
		d.lastSN = newSN
		atomic.StoreInt64(&d.lastPacketMs, time.Now().UnixNano()/1e6)
		atomic.StoreUint32(&d.lastTS, newTS)
	}
	pkt.PayloadType = d.payload
	pkt.Timestamp = d.lastTS
	pkt.SequenceNumber = d.lastSN
	pkt.SSRC = d.ssrc

	_, err := d.writeStream.WriteRTP(&pkt.Header, pkt.Payload)
	if err != nil {
		log.Errorf("Write packet err %v", err)
	}

	return err
}

func (d *DownTrack) getSRStats() (octets, packets uint32) {
	octets = atomic.LoadUint32(&d.octetCount)
	packets = atomic.LoadUint32(&d.packetCount)
	return
}
