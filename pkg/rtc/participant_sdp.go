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
	"slices"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksdp "github.com/livekit/protocol/sdp"
	"github.com/livekit/protocol/utils"
)

func (p *ParticipantImpl) populateSdpCid(parsedOffer *sdp.SessionDescription) ([]*sdp.MediaDescription, []*sdp.MediaDescription) {
	processUnmatch := func(unmatches []*sdp.MediaDescription, trackType livekit.TrackType) {
		for _, unmatch := range unmatches {
			streamID, ok := lksdp.ExtractStreamID(unmatch)
			if !ok {
				continue
			}

			sdpCodecs, err := lksdp.CodecsFromMediaDescription(unmatch)
			if err != nil || len(sdpCodecs) == 0 {
				p.pubLogger.Errorw(
					"extract codecs from media section failed", err,
					"media", unmatch,
					"parsedOffer", parsedOffer,
				)
				continue
			}

			p.pendingTracksLock.Lock()
			signalCid, info, _, migrated, _ := p.getPendingTrack(streamID, trackType, false)
			if migrated {
				p.pendingTracksLock.Unlock()
				continue
			}

			if info == nil {
				p.pendingTracksLock.Unlock()

				// could be already published track and the unmatch could be a back up codec publish
				numUnmatchedTracks := 0
				var unmatchedTrack types.MediaTrack
				var unmatchedSdpMimeType mime.MimeType

				found := false
				for _, sdpCodec := range sdpCodecs {
					sdpMimeType := mime.NormalizeMimeTypeCodec(sdpCodec.Name).ToMimeType()
					for _, publishedTrack := range p.GetPublishedTracks() {
						if sigCid, sdpCid := publishedTrack.(*MediaTrack).GetCidsForMimeType(sdpMimeType); sigCid != "" && sdpCid == "" {
							// a back up codec has a SDP cid match
							if sigCid == streamID {
								found = true
								break
							} else {
								numUnmatchedTracks++
								unmatchedTrack = publishedTrack
								unmatchedSdpMimeType = sdpMimeType
							}
						}
					}
					if found {
						break
					}
				}
				if !found && unmatchedTrack != nil {
					if numUnmatchedTracks != 1 {
						p.pubLogger.Warnw(
							"too many unmatched tracks", nil,
							"media", unmatch,
							"parsedOffer", parsedOffer,
						)
					}
					unmatchedTrack.(*MediaTrack).UpdateCodecSdpCid(unmatchedSdpMimeType, streamID)
					p.pubLogger.Debugw(
						"published track SDP cid updated",
						"trackID", unmatchedTrack.ID(),
						"track", logger.Proto(unmatchedTrack.ToProto()),
					)
				}
				continue
			}

			if len(info.Codecs) == 0 {
				p.pendingTracksLock.Unlock()
				p.pubLogger.Warnw(
					"track without codecs", nil,
					"trackID", info.Sid,
					"pendingTrack", p.pendingTracks[signalCid],
					"media", unmatch,
					"parsedOffer", parsedOffer,
				)
				continue
			}

			found := false
			updated := false
			for _, sdpCodec := range sdpCodecs {
				if mime.NormalizeMimeTypeCodec(sdpCodec.Name) == mime.GetMimeTypeCodec(info.Codecs[0].MimeType) {
					// set SdpCid only if different from SignalCid
					if streamID != info.Codecs[0].Cid {
						info.Codecs[0].SdpCid = streamID
						updated = true
					}
					found = true
					break
				}
				if found {
					break
				}
			}

			if !found {
				// not using SimulcastCodec, i. e. mime type not available till track publish
				if len(info.Codecs) == 1 {
					// set SdpCid only if different from SignalCid
					if streamID != info.Codecs[0].Cid {
						info.Codecs[0].SdpCid = streamID
						updated = true
					}
				}
			}

			if updated {
				p.pendingTracks[signalCid].trackInfos[0] = utils.CloneProto(info)
				p.pubLogger.Debugw(
					"pending track SDP cid updated",
					"signalCid", signalCid,
					"trackID", info.Sid,
					"pendingTrack", p.pendingTracks[signalCid],
				)
			}
			p.pendingTracksLock.Unlock()
		}
	}

	unmatchAudios, err := p.TransportManager.GetUnmatchMediaForOffer(parsedOffer, "audio")
	if err != nil {
		p.pubLogger.Warnw("could not get unmatch audios", err)
		return nil, nil
	}

	unmatchVideos, err := p.TransportManager.GetUnmatchMediaForOffer(parsedOffer, "video")
	if err != nil {
		p.pubLogger.Warnw("could not get unmatch audios", err)
		return nil, nil
	}

	processUnmatch(unmatchAudios, livekit.TrackType_AUDIO)
	processUnmatch(unmatchVideos, livekit.TrackType_VIDEO)
	return unmatchAudios, unmatchVideos
}

func (p *ParticipantImpl) setCodecPreferencesForPublisher(
	parsedOffer *sdp.SessionDescription,
	unmatchAudios []*sdp.MediaDescription,
	unmatchVideos []*sdp.MediaDescription,
) *sdp.SessionDescription {
	parsedOffer, unprocessedUnmatchAudios := p.setCodecPreferencesForPublisherMedia(
		parsedOffer,
		unmatchAudios,
		livekit.TrackType_AUDIO,
	)
	parsedOffer = p.setCodecPreferencesOpusRedForPublisher(parsedOffer, unprocessedUnmatchAudios)
	parsedOffer, _ = p.setCodecPreferencesForPublisherMedia(
		parsedOffer,
		unmatchVideos,
		livekit.TrackType_VIDEO,
	)
	return parsedOffer
}

func (p *ParticipantImpl) setCodecPreferencesOpusRedForPublisher(
	parsedOffer *sdp.SessionDescription,
	unmatchAudios []*sdp.MediaDescription,
) *sdp.SessionDescription {
	for _, unmatchAudio := range unmatchAudios {
		streamID, ok := lksdp.ExtractStreamID(unmatchAudio)
		if !ok {
			continue
		}

		p.pendingTracksLock.RLock()
		_, ti, _, _, _ := p.getPendingTrack(streamID, livekit.TrackType_AUDIO, false)
		p.pendingTracksLock.RUnlock()
		if ti == nil {
			continue
		}

		codecs, err := lksdp.CodecsFromMediaDescription(unmatchAudio)
		if err != nil {
			p.pubLogger.Errorw(
				"extract codecs from media section failed", err,
				"media", unmatchAudio,
				"parsedOffer", parsedOffer,
			)
			continue
		}

		var opusPayload uint8
		for _, codec := range codecs {
			if mime.IsMimeTypeCodecStringOpus(codec.Name) {
				opusPayload = codec.PayloadType
				break
			}
		}
		if opusPayload == 0 {
			continue
		}

		// if RED is disabled for this track, don't prefer RED codec in offer
		var preferredCodecs, leftCodecs []string
		for _, codec := range codecs {
			// codec contain opus/red
			if !ti.DisableRed && mime.IsMimeTypeCodecStringRED(codec.Name) && strings.Contains(codec.Fmtp, strconv.FormatInt(int64(opusPayload), 10)) {
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

	return parsedOffer
}

func (p *ParticipantImpl) setCodecPreferencesForPublisherMedia(
	parsedOffer *sdp.SessionDescription,
	unmatches []*sdp.MediaDescription,
	trackType livekit.TrackType,
) (*sdp.SessionDescription, []*sdp.MediaDescription) {
	unprocessed := make([]*sdp.MediaDescription, 0, len(unmatches))
	// unmatched media is pending for publish, set codec preference
	for _, unmatch := range unmatches {
		var ti *livekit.TrackInfo
		var mimeType string

		streamID, ok := lksdp.ExtractStreamID(unmatch)
		if !ok {
			unprocessed = append(unprocessed, unmatch)
			continue
		}

		p.pendingTracksLock.RLock()
		mt := p.getPublishedTrackBySdpCid(streamID)
		if mt != nil {
			ti = mt.ToProto()
		} else {
			_, ti, _, _, _ = p.getPendingTrack(streamID, trackType, false)
		}
		p.pendingTracksLock.RUnlock()

		if ti == nil {
			unprocessed = append(unprocessed, unmatch)
			continue
		}

		for _, c := range ti.Codecs {
			if c.Cid == streamID || c.SdpCid == streamID {
				mimeType = c.MimeType
				break
			}
		}
		if mimeType == "" && len(ti.Codecs) > 0 {
			mimeType = ti.Codecs[0].MimeType
		}

		if mimeType == "" {
			unprocessed = append(unprocessed, unmatch)
			continue
		}

		codecs, err := lksdp.CodecsFromMediaDescription(unmatch)
		if err != nil {
			p.pubLogger.Errorw(
				"extract codecs from media section failed", err,
				"media", unmatch,
				"parsedOffer", parsedOffer,
			)
			unprocessed = append(unprocessed, unmatch)
			continue
		}

		var codecIdx int
		var preferredCodecs, leftCodecs []string
		for idx, c := range codecs {
			if mime.GetMimeTypeCodec(mimeType) == mime.NormalizeMimeTypeCodec(c.Name) {
				preferredCodecs = append(preferredCodecs, strconv.FormatInt(int64(c.PayloadType), 10))
				codecIdx = idx
			} else {
				leftCodecs = append(leftCodecs, strconv.FormatInt(int64(c.PayloadType), 10))
			}
		}

		// could not find preferred mime in the offer
		if len(preferredCodecs) == 0 {
			unprocessed = append(unprocessed, unmatch)
			continue
		}

		unmatch.MediaName.Formats = append(unmatch.MediaName.Formats[:0], preferredCodecs...)
		if trackType == livekit.TrackType_VIDEO {
			// if the client don't comply with codec order in SDP answer, only keep preferred codecs to force client to use it
			if p.params.ClientInfo.ComplyWithCodecOrderInSDPAnswer() {
				unmatch.MediaName.Formats = append(unmatch.MediaName.Formats, leftCodecs...)
			}
		} else {
			// ensure nack enabled for audio in publisher offer
			var nackFound bool
			for _, attr := range unmatch.Attributes {
				if attr.Key == "rtcp-fb" && strings.Contains(attr.Value, fmt.Sprintf("%d nack", codecs[codecIdx].PayloadType)) {
					nackFound = true
					break
				}
			}
			if !nackFound {
				unmatch.Attributes = append(unmatch.Attributes, sdp.Attribute{
					Key:   "rtcp-fb",
					Value: fmt.Sprintf("%d nack", codecs[codecIdx].PayloadType),
				})
			}

			unmatch.MediaName.Formats = append(unmatch.MediaName.Formats, leftCodecs...)
		}
	}

	return parsedOffer, unprocessed
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
						_, ti, _, _, _ = p.getPendingTrack(streamID, livekit.TrackType_AUDIO, false)
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

			opusPT, err := parsed.GetPayloadTypeForCodec(sdp.Codec{Name: mime.MimeTypeCodecOpus.String()})
			if err != nil {
				p.pubLogger.Infow("failed to get opus payload type", "error", err, "trackID", ti.Sid)
				continue
			}

			for i, attr := range m.Attributes {
				if strings.HasPrefix(attr.String(), fmt.Sprintf("fmtp:%d", opusPT)) {
					if !ti.DisableDtx {
						attr.Value += ";usedtx=1"
					}
					if slices.Contains(ti.AudioFeatures, livekit.AudioTrackFeature_TF_STEREO) {
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
