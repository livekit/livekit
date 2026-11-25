// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package moq

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/trickstercache/trickster/v2/pkg/moq/message"
	"github.com/trickstercache/trickster/v2/pkg/moq/session"

	"github.com/livekit/livekit-server/pkg/moq/media"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const maxMoQFrame = 4 << 20

// Every external publication is authorized by the AddTrack request that
// assigned its SID. Its request stream owns the RTP receiver and publication.
func (p *moqPeer) acceptMedia(r *session.Request, name string) bool {
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] != "up" {
		return false
	}
	m, ok := media.MimeFromCodecString(parts[2])
	if !ok || parts[2] != media.CodecShortName(m) || !supportedMoQCodec(m) {
		return false
	}
	rx, track, err := p.participant.StartMoQPublication(p.generation, livekit.TrackID(parts[1]), m)
	if err != nil {
		return false
	}
	defer rx.Close()
	defer track.Close(false)
	h := &moqMediaUpload{peer: p, track: track, receiver: rx}
	r.SetHandlers(nil, h)
	if err := r.Send(&message.RequestOK{}); err == nil {
		<-r.Context().Done()
	}
	return true
}

type moqMediaUpload struct {
	peer      *moqPeer
	track     types.MediaTrack
	receiver  *media.IngestReceiver
	mu        sync.Mutex
	group     uint64
	haveGroup bool
	object    uint64
}

func (h *moqMediaUpload) HandleSubgroup(r *session.Request, in *session.IncomingSubgroup) {
	if in.Header().Subgroup != 0 {
		in.Cancel(message.ResetCancelled)
		return
	}
	for {
		obj, err := in.Next()
		if err == io.EOF {
			return
		}
		if err != nil || obj.PayloadLen > maxMoQFrame || obj.PayloadLen == 0 || !h.canPublish() {
			in.Cancel(message.ResetCancelled)
			return
		}
		if !h.peer.inflight.TryAcquire(int64(obj.PayloadLen)) {
			in.Cancel(message.ResetDeliveryTimeout)
			return
		}
		err = h.readFrame(in, in.Header().Group, obj)
		h.peer.inflight.Release(int64(obj.PayloadLen))
		if err != nil {
			in.Cancel(message.ResetCancelled)
			return
		}
	}
}

func (h *moqMediaUpload) canPublish() bool {
	return h.track.IsOpen() && h.peer.participant.CanPublishSource(h.track.Source()) &&
		h.peer.participant.IsMoQAttachmentCurrent(h.peer.generation)
}

func (h *moqMediaUpload) readFrame(in io.Reader, group uint64, obj message.SubgroupObject) error {
	payload := make([]byte, int(obj.PayloadLen))
	if _, err := io.ReadFull(in, payload); err != nil {
		return err
	}
	frame, err := readFrameProperties(obj.Properties, payload)
	if err != nil {
		return err
	}
	if h.track.Kind() == livekit.TrackType_VIDEO {
		if h.receiver.Mime() == mime.MimeTypeVP8 {
			frame.Keyframe = len(payload) >= 10 && payload[0]&1 == 0 && bytes.Equal(payload[3:6], []byte{0x9d, 0x01, 0x2a})
		}
		if obj.ID == 0 && !frame.Keyframe {
			return errors.New("video group requires an initial keyframe")
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// A blocked read can outlive the participant's grant or QUIC attachment.
	if !h.canPublish() {
		return errors.New("media publication is no longer active")
	}
	current := (!h.haveGroup || group > h.group) && obj.ID == 0 || h.haveGroup && group == h.group && obj.ID == h.object+1
	if !current || h.track.IsMuted() {
		return nil
	}
	h.haveGroup, h.group, h.object = true, group, obj.ID
	return h.receiver.WriteFrame(0, frame)
}

func (h *moqMediaUpload) HandleFetch(_ *session.Request, in *session.IncomingFetch) {
	in.Cancel(message.ResetCancelled)
}
func (h *moqMediaUpload) HandleDatagram(_ *session.Request, _ *message.Datagram) {}

type moqRendition struct {
	track    types.MediaTrack
	receiver sfu.TrackReceiver
	codec    mime.MimeType
	catalog  media.CatalogTrack
}

func supportedMoQCodec(m mime.MimeType) bool {
	switch m {
	case mime.MimeTypeOpus, mime.MimeTypeVP8, mime.MimeTypeVP9, mime.MimeTypeH264, mime.MimeTypeH265, mime.MimeTypeAV1:
		return true
	}
	return false
}

func (p *moqPeer) rendition(pub types.LocalParticipant, track types.MediaTrack) *moqRendition {
	if !p.allowedMedia(pub, track) {
		return nil
	}
	mt, ok := track.(interface{ PrimaryReceiver() sfu.TrackReceiver })
	if !ok {
		return nil
	}
	rx := mt.PrimaryReceiver()
	if rx == nil {
		return nil
	}
	m := rx.Mime()
	if m == mime.MimeTypeRED {
		// RedPrimaryReceiver unwraps RFC 2198 but reports the parent receiver's MIME.
		m = mime.MimeTypeOpus
		rx = rx.GetPrimaryReceiverForRed()
	}
	if rx == nil {
		return nil
	}
	if !supportedMoQCodec(m) {
		return nil
	}
	ti := track.ToProto()
	if ti.GetEncryption() != livekit.Encryption_NONE {
		return nil
	}
	c := media.CatalogTrack{
		Name:              string(track.ID()) + "/" + media.CodecShortName(m),
		Packaging:         media.PackagingLOC,
		PackagingMetadata: "streamMapping=object",
		IsLive:            true,
		Codec:             media.WebCodecsString(m, rx.Codec().SDPFmtpLine),
		LiveKit:           &media.TrackExt{Sid: string(track.ID()), PublisherSID: string(pub.ID())},
	}
	if c.Codec == "" {
		return nil
	}
	if m == mime.MimeTypeOpus {
		c.Role, c.Samplerate, c.ChannelConfig = media.RoleAudio, 48000, "1"
		if ti.Stereo {
			c.ChannelConfig = "2"
		}
	} else {
		c.Role = media.RoleVideo
		setVideoCatalogMetadata(&c, ti, m, rx.GetTemporalLayerFpsForSpatial(0))
	}
	return &moqRendition{track, rx, m, c}
}

// The RTP tap forwards spatial layer zero. Track-level dimensions describe
// that rendition only for single-layer publications.
func setVideoCatalogMetadata(c *media.CatalogTrack, ti *livekit.TrackInfo, m mime.MimeType, rates []float32) {
	layers := buffer.GetVideoLayersForMimeType(m, ti)
	if len(layers) <= 1 {
		c.Width, c.Height = int(ti.Width), int(ti.Height)
	}
	for _, layer := range layers {
		if layer == nil || layer.SpatialLayer != 0 {
			continue
		}
		if layer.Width != 0 && layer.Height != 0 {
			c.Width, c.Height = int(layer.Width), int(layer.Height)
		}
		c.Bitrate = int(layer.Bitrate)
		break
	}
	if len(rates) > 0 {
		if fps := slices.Max(rates); fps > 0 {
			c.Framerate = float64(fps)
		}
	}
}

func (p *moqPeer) allowedMedia(pub types.LocalParticipant, track types.MediaTrack) bool {
	return pub != nil && track != nil && pub.ID() != p.participant.ID() && !pub.Hidden() && track.IsOpen() && p.participant.CanSubscribe() &&
		p.participant.IsMoQAttachmentCurrent(p.generation) && p.participant.WantsMoQTrack(track.ID()) &&
		(rtc.IsParticipantExemptFromTrackPermissionsRestrictions(p.participant) || pub.HasPermission(track.ID(), p.participant.Identity()))
}

func (p *moqPeer) serveCatalog(r *session.Request) {
	p.mu.Lock()
	if p.catalog != nil && p.catalog.Context().Err() == nil {
		p.mu.Unlock()
		_ = r.Reject(message.RequestNotSupported, "catalog subscription already active", 0)
		return
	}
	p.catalog = r
	p.mu.Unlock()
	sub, err := acceptMoQSubscription(r)
	if err != nil {
		return
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var previous []byte
	for {
		if !p.participant.IsMoQAttachmentCurrent(p.generation) || !p.participant.CanSubscribe() {
			r.Cancel(message.ResetCancelled)
			return
		}
		c := media.Catalog{}
		for _, id := range p.participant.MoQSubscribedTrackIDs() {
			pub, track := p.room.LookupPublishedTrack(id)
			if rend := p.rendition(pub, track); rend != nil {
				c.Tracks = append(c.Tracks, rend.catalog)
			}
		}
		slices.SortFunc(c.Tracks, func(a, b media.CatalogTrack) int { return strings.Compare(a.Name, b.Name) })
		data, err := c.Marshal()
		if err != nil {
			r.Cancel(message.ResetInternalError)
			return
		}
		if !bytes.Equal(data, previous) {
			if err := writeMoQObject(r, p.catalogGroup.Add(1)-1, data); err != nil {
				r.Cancel(message.ResetDeliveryTimeout)
				return
			}
			previous = data
		}
		select {
		case <-r.Context().Done():
			return
		case <-sub.update:
			endMoQSubscriptionOnUpdate(r)
			return
		case <-ticker.C:
		}
	}
}

func (p *moqPeer) serveMedia(r *session.Request, name string) bool {
	id, _, ok := strings.Cut(name, "/")
	if !ok || !p.participant.WantsMoQTrack(livekit.TrackID(id)) {
		return false
	}
	pub, track := p.room.LookupPublishedTrack(livekit.TrackID(id))
	rend := p.rendition(pub, track)
	if rend == nil || rend.catalog.Name != name {
		return false
	}
	if !p.reserveMedia(track) {
		_ = r.Reject(message.RequestNotSupported, "duplicate subscription or room subscription limit reached", 0)
		return true
	}
	defer func() {
		p.mu.Lock()
		delete(p.mediaSubscriptions, track.ID())
		p.mu.Unlock()
	}()
	t := newMoQTap(rend, fmt.Sprintf("%s/%d/%d", p.participant.ID(), p.generation, r.RequestID()))
	if err := rend.receiver.AddDownTrack(t); err != nil {
		return false
	}
	defer rend.receiver.DeleteDownTrack(t.SubscriberID())
	defer t.Close()
	if source, ok := track.(interface {
		SetRenditionSubscribed(livekit.ParticipantID, mime.MimeType, bool)
	}); ok {
		source.SetRenditionSubscribed(t.SubscriberID(), rend.receiver.Mime(), true)
		defer source.SetRenditionSubscribed(t.SubscriberID(), rend.receiver.Mime(), false)
	}
	sub, err := acceptMoQSubscription(r)
	if err != nil {
		return true
	}
	if !p.allowedMedia(pub, track) {
		r.Cancel(message.ResetCancelled)
		return true
	}
	release := p.participant.BindMoQSubscription(p.generation, pub.ID(), track)
	defer release()
	video := rend.catalog.Role == media.RoleVideo
	if video {
		rend.receiver.SendPLI(0, true)
	}
	var group, object uint64
	haveGroup := false
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !p.allowedMedia(pub, rend.track) {
				r.Cancel(message.ResetCancelled)
				return true
			}
			if video && !rend.track.IsMuted() && t.waitingForKeyframe() {
				rend.receiver.SendPLI(0, false)
			}
		case <-r.Context().Done():
			return true
		case <-sub.update:
			endMoQSubscriptionOnUpdate(r)
			return true
		case <-t.done:
			r.Cancel(message.ResetCancelled)
			return true
		case sample := <-t.frames:
			if rend.track.IsMuted() {
				haveGroup = false
				continue
			}
			if !p.allowedMedia(pub, rend.track) {
				r.Cancel(message.ResetCancelled)
				return true
			}
			if !video || sample.frame.Keyframe {
				group, object, haveGroup = max(group+1, sample.frame.ExtTS), 0, true
			}
			if !haveGroup {
				continue
			}
			props, err := frameProperties(sample.time, sample.frame, video)
			if err != nil {
				r.Cancel(message.ResetInternalError)
				return true
			}
			header := message.SubgroupHeader{Group: group, Mode: message.SubgroupIDFirstObject, Subgroup: object, EndOfGroup: !video, HasProperties: true, Priority: 80}
			obj := message.SubgroupObject{ID: object, Properties: props, PayloadLen: uint64(len(sample.frame.Data))}
			if err := writeMoQSample(r, header, obj, sample.frame.Data); err != nil {
				r.Cancel(message.ResetDeliveryTimeout)
				return true
			}
			object++
		}
	}
}

func (p *moqPeer) reserveMedia(track types.MediaTrack) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.mediaSubscriptions[track.ID()]; exists {
		return false
	}
	kind := track.Kind()
	if limit := p.participant.MoQSubscriptionLimit(kind); limit > 0 {
		var count int32
		for _, k := range p.mediaSubscriptions {
			if k == kind {
				count++
			}
		}
		if count >= limit {
			return false
		}
	}
	if p.mediaSubscriptions == nil {
		p.mediaSubscriptions = make(map[livekit.TrackID]livekit.TrackType)
	}
	p.mediaSubscriptions[track.ID()] = kind
	return true
}
