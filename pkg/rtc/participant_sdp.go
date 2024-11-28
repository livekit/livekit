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

package rtc

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/protocol/livekit"
	lksdp "github.com/livekit/protocol/sdp"
)

func (p *ParticipantImpl) setCodecPreferencesForPublisher(offer webrtc.SessionDescription) webrtc.SessionDescription {
	offer = p.setCodecPreferencesOpusRedForPublisher(offer)
	offer = p.setCodecPreferencesVideoForPublisher(offer)
	return offer
}

func (p *ParticipantImpl) setCodecPreferencesOpusRedForPublisher(offer webrtc.SessionDescription) webrtc.SessionDescription {
	parsed, unmatchAudios, err := p.TransportManager.GetUnmatchMediaForOffer(offer, "audio")
	if err != nil || len(unmatchAudios) == 0 {
		return offer
	}

	for _, unmatchAudio := range unmatchAudios {
		streamID, ok := lksdp.ExtractStreamID(unmatchAudio)
		if !ok {
			continue
		}

		p.pendingTracksLock.RLock()
		_, info, _, _ := p.getPendingTrack(streamID, livekit.TrackType_AUDIO, false)
		// if RED is disabled for this track, don't prefer RED codec in offer
		disableRed := info != nil && info.DisableRed
		p.pendingTracksLock.RUnlock()

		codecs, err := codecsFromMediaDescription(unmatchAudio)
		if err != nil {
			p.pubLogger.Errorw("extract codecs from media section failed", err, "media", unmatchAudio)
			continue
		}

		var opusPayload uint8
		for _, codec := range codecs {
			if strings.EqualFold(codec.Name, "opus") {
				opusPayload = codec.PayloadType
				break
			}
		}
		if opusPayload == 0 {
			continue
		}

		var preferredCodecs, leftCodecs []string
		for _, codec := range codecs {
			// codec contain opus/red
			if !disableRed && strings.EqualFold(codec.Name, "red") && strings.Contains(codec.Fmtp, strconv.FormatInt(int64(opusPayload), 10)) {
				preferredCodecs = append(preferredCodecs, strconv.FormatInt(int64(codec.PayloadType), 10))
			} else {
				leftCodecs = append(leftCodecs, strconv.FormatInt(int64(codec.PayloadType), 10))
			}
		}

		// ensure nack enabled for audio in publisher offer
		var nackFound bool
		for _, attr := range unmatchAudio.Attributes {
			if attr.Key == "rtcp-fb" && strings.Contains(attr.Value, fmt.Sprintf("%d nack", opusPayload)) {
				nackFound = true
				break
			}
		}
		if !nackFound {
			unmatchAudio.Attributes = append(unmatchAudio.Attributes, sdp.Attribute{
				Key:   "rtcp-fb",
				Value: fmt.Sprintf("%d nack", opusPayload),
			})
		}

		// no opus/red found
		if len(preferredCodecs) == 0 {
			continue
		}

		unmatchAudio.MediaName.Formats = append(unmatchAudio.MediaName.Formats[:0], preferredCodecs...)
		unmatchAudio.MediaName.Formats = append(unmatchAudio.MediaName.Formats, leftCodecs...)
	}

	bytes, err := parsed.Marshal()
	if err != nil {
		p.pubLogger.Errorw("failed to marshal offer", err)
		return offer
	}

	return webrtc.SessionDescription{
		Type: offer.Type,
		SDP:  string(bytes),
	}
}

func (p *ParticipantImpl) setCodecPreferencesVideoForPublisher(offer webrtc.SessionDescription) webrtc.SessionDescription {
	parsed, unmatchVideos, err := p.TransportManager.GetUnmatchMediaForOffer(offer, "video")
	if err != nil || len(unmatchVideos) == 0 {
		return offer
	}
	// unmatched video is pending for publish, set codec preference
	for _, unmatchVideo := range unmatchVideos {
		streamID, ok := lksdp.ExtractStreamID(unmatchVideo)
		if !ok {
			continue
		}

		var info *livekit.TrackInfo
		p.pendingTracksLock.RLock()
		mt := p.getPublishedTrackBySdpCid(streamID)
		if mt != nil {
			info = mt.ToProto()
		} else {
			_, info, _, _ = p.getPendingTrack(streamID, livekit.TrackType_VIDEO, false)
		}

		if info == nil {
			p.pendingTracksLock.RUnlock()
			continue
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

		mime = strings.ToUpper(mime)
		if mime != "" {
			codecs, err := codecsFromMediaDescription(unmatchVideo)
			if err != nil {
				p.pubLogger.Errorw("extract codecs from media section failed", err, "media", unmatchVideo)
				continue
			}

			var preferredCodecs, leftCodecs []string
			for _, c := range codecs {
				if strings.HasSuffix(mime, strings.ToUpper(c.Name)) {
					preferredCodecs = append(preferredCodecs, strconv.FormatInt(int64(c.PayloadType), 10))
				} else {
					leftCodecs = append(leftCodecs, strconv.FormatInt(int64(c.PayloadType), 10))
				}
			}

			unmatchVideo.MediaName.Formats = append(unmatchVideo.MediaName.Formats[:0], preferredCodecs...)
			// if the client don't comply with codec order in SDP answer, only keep preferred codecs to force client to use it
			if p.params.ClientInfo.ComplyWithCodecOrderInSDPAnswer() {
				unmatchVideo.MediaName.Formats = append(unmatchVideo.MediaName.Formats, leftCodecs...)
			}
		}
	}

	bytes, err := parsed.Marshal()
	if err != nil {
		p.pubLogger.Errorw("failed to marshal offer", err)
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
			_, ok := m.Attribute(sdp.AttrKeyInactive)
			if ok {
				continue
			}
			mid, ok := m.Attribute(sdp.AttrKeyMID)
			if !ok {
				continue
			}
			// find track info from offer's stream id
			var ti *livekit.TrackInfo
			for _, om := range parsedOffer.MediaDescriptions {
				_, ok := om.Attribute(sdp.AttrKeyInactive)
				if ok {
					continue
				}
				omid, ok := om.Attribute(sdp.AttrKeyMID)
				if ok && omid == mid {
					streamID, ok := lksdp.ExtractStreamID(om)
					if !ok {
						continue
					}
					track, _ := p.getPublishedTrackBySdpCid(streamID).(*MediaTrack)
					if track == nil {
						p.pendingTracksLock.RLock()
						_, ti, _, _ = p.getPendingTrack(streamID, livekit.TrackType_AUDIO, false)
						p.pendingTracksLock.RUnlock()
					} else {
						ti = track.ToProto()
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
				p.pubLogger.Infow("failed to get opus payload type", "error", err, "trackID", ti.Sid)
				continue
			}

			for i, attr := range m.Attributes {
				if strings.HasPrefix(attr.String(), fmt.Sprintf("fmtp:%d", opusPT)) {
					if !ti.DisableDtx {
						attr.Value += ";usedtx=1"
					}
					if ti.Stereo {
						attr.Value += ";stereo=1;maxaveragebitrate=510000"
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
		p.pubLogger.Infow("failed to marshal answer", "error", err)
		return answer
	}
	answer.SDP = string(bytes)
	return answer
}
