package rtc

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/livekit"
	lksdp "github.com/livekit/protocol/sdp"
)

func (p *ParticipantImpl) setCodecPreferencesForPublisher(offer webrtc.SessionDescription) webrtc.SessionDescription {
	offer = p.setCodecPreferencesOpusRedForPublisher(offer)
	offer = p.setCodecPreferencesVideoForPublisher(offer)
	return offer
}

func (p *ParticipantImpl) setCodecPreferencesOpusRedForPublisher(offer webrtc.SessionDescription) webrtc.SessionDescription {
	parsed, lastAudio, err := p.TransportManager.GetLastUnmatchedMediaForOffer(offer, "audio")
	if err != nil || lastAudio == nil {
		return offer
	}

	streamID, ok := lksdp.ExtractStreamID(lastAudio)
	if !ok {
		return offer
	}

	p.pendingTracksLock.RLock()
	_, info := p.getPendingTrack(streamID, livekit.TrackType_AUDIO)
	// if RED is disabled for this track, don't prefer RED codec in offer
	if info != nil && info.DisableRed {
		p.pendingTracksLock.RUnlock()
		return offer
	}
	p.pendingTracksLock.RUnlock()

	codecs, err := codecsFromMediaDescription(lastAudio)
	if err != nil {
		return offer
	}

	var opusPayload uint8
	for _, codec := range codecs {
		if strings.EqualFold(codec.Name, "opus") {
			opusPayload = codec.PayloadType
			break
		}
	}
	if opusPayload == 0 {
		return offer
	}

	var preferredCodecs, leftCodecs []string
	for _, codec := range codecs {
		// codec contain opus/red
		if strings.EqualFold(codec.Name, "red") && strings.Contains(codec.Fmtp, strconv.FormatInt(int64(opusPayload), 10)) {
			preferredCodecs = append(preferredCodecs, strconv.FormatInt(int64(codec.PayloadType), 10))
		} else {
			leftCodecs = append(leftCodecs, strconv.FormatInt(int64(codec.PayloadType), 10))
		}
	}

	// no opus/red found
	if len(preferredCodecs) == 0 {
		return offer
	}

	lastAudio.MediaName.Formats = append(lastAudio.MediaName.Formats[:0], preferredCodecs...)
	lastAudio.MediaName.Formats = append(lastAudio.MediaName.Formats, leftCodecs...)

	bytes, err := parsed.Marshal()
	if err != nil {
		p.params.Logger.Errorw("failed to marshal offer", err)
		return offer
	}

	return webrtc.SessionDescription{
		Type: offer.Type,
		SDP:  string(bytes),
	}
}

func (p *ParticipantImpl) setCodecPreferencesVideoForPublisher(offer webrtc.SessionDescription) webrtc.SessionDescription {
	parsed, lastVideo, err := p.TransportManager.GetLastUnmatchedMediaForOffer(offer, "video")
	if err != nil || lastVideo == nil {
		return offer
	}
	// last video is pending for publish, set codec preference
	streamID, ok := lksdp.ExtractStreamID(lastVideo)
	if !ok {
		return offer
	}

	p.pendingTracksLock.RLock()
	_, info := p.getPendingTrack(streamID, livekit.TrackType_VIDEO)
	if info == nil {
		p.pendingTracksLock.RUnlock()
		return offer
	}
	var mime string
	for _, c := range info.Codecs {
		if c.Cid == streamID {
			mime = c.MimeType
			break
		}
	}
	if mime == "" && len(info.Codecs) > 0 {
		mime = info.Codecs[0].MimeType
	}
	p.pendingTracksLock.RUnlock()

	if mime == "" {
		return offer
	}

	codecs, err := codecsFromMediaDescription(lastVideo)
	if err != nil {
		return offer
	}

	mime = strings.ToUpper(mime)
	var preferredCodecs, leftCodecs []string
	for _, c := range codecs {
		if strings.HasSuffix(mime, strings.ToUpper(c.Name)) {
			preferredCodecs = append(preferredCodecs, strconv.FormatInt(int64(c.PayloadType), 10))
		} else {
			leftCodecs = append(leftCodecs, strconv.FormatInt(int64(c.PayloadType), 10))
		}
	}

	lastVideo.MediaName.Formats = append(lastVideo.MediaName.Formats[:0], preferredCodecs...)
	lastVideo.MediaName.Formats = append(lastVideo.MediaName.Formats, leftCodecs...)

	bytes, err := parsed.Marshal()
	if err != nil {
		p.params.Logger.Errorw("failed to marshal offer", err)
		return offer
	}

	return webrtc.SessionDescription{
		Type: offer.Type,
		SDP:  string(bytes),
	}
}

// configure publisher answer for audio track's dtx and stereo settings
func (p *ParticipantImpl) configurePublisherAnswer(answer webrtc.SessionDescription) webrtc.SessionDescription {
	offer := p.TransportManager.LastPublisherOffer()
	parsedOffer, err := offer.Unmarshal()
	if err != nil {
		return answer
	}

	parsed, err := answer.Unmarshal()
	if err != nil {
		return answer
	}

	for _, m := range parsed.MediaDescriptions {
		switch m.MediaName.Media {
		case "audio":
			mid, ok := m.Attribute(sdp.AttrKeyMID)
			if !ok {
				continue
			}
			// find track info from offer's stream id
			var ti *livekit.TrackInfo
			for _, om := range parsedOffer.MediaDescriptions {
				omid, ok := om.Attribute(sdp.AttrKeyMID)
				if ok && omid == mid {
					streamID, ok := lksdp.ExtractStreamID(om)
					if !ok {
						continue
					}
					track, _ := p.getPublishedTrackBySdpCid(streamID).(*MediaTrack)
					if track == nil {
						p.pendingTracksLock.RLock()
						_, ti = p.getPendingTrack(streamID, livekit.TrackType_AUDIO)
						p.pendingTracksLock.RUnlock()
					} else {
						ti = track.TrackInfo(false)
					}
					break
				}
			}

			if ti == nil || (ti.DisableDtx && !ti.Stereo) {
				// no need to configure
				continue
			}

			opusPT, err := parsed.GetPayloadTypeForCodec(sdp.Codec{Name: "opus"})
			if err != nil {
				p.params.Logger.Infow("failed to get opus payload type", "error", err, "trakcID", ti.Sid)
				continue
			}

			for i, attr := range m.Attributes {
				if strings.HasPrefix(attr.String(), fmt.Sprintf("fmtp:%d", opusPT)) {
					if !ti.DisableDtx {
						attr.Value += ";usedtx=1"
					}
					if ti.Stereo {
						attr.Value += ";stereo=1"
					}
					m.Attributes[i] = attr
				}
			}

		default:
			continue
		}
	}

	bytes, err := parsed.Marshal()
	if err != nil {
		p.params.Logger.Infow("failed to marshal answer", "error", err)
		return answer
	}
	answer.SDP = string(bytes)
	return answer
}
