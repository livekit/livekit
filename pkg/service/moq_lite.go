// Copyright 2026 LiveKit, Inc.
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

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/quic-go/quic-go/quicvarint"
	webtransport "github.com/quic-go/webtransport-go"
)

type moqLiteSubscribe struct {
	ID         uint64
	Broadcast  string
	Track      string
	Priority   uint8
	Ordered    bool
	MaxLatency uint64
	StartGroup uint64
	EndGroup   uint64
	HasStart   bool
	HasEnd     bool
}

type moqLiteResolvedTrack struct {
	moqResolvedTrack
	Timing bool
}

type moqLiteTimingSample struct {
	Sequence        uint64  `json:"sequence"`
	TrackID         string  `json:"trackId"`
	TrackName       string  `json:"trackName"`
	Layer           int32   `json:"layer"`
	RTPTime         uint32  `json:"rtpTime"`
	KeyFrame        bool    `json:"keyFrame"`
	Cached          bool    `json:"cached"`
	SentAtUnixNs    int64   `json:"sentAtUnixNs"`
	UserTimestampUs *int64  `json:"userTimestampUs,omitempty"`
	FrameID         *uint32 `json:"frameId,omitempty"`
}

func (s *MoQService) serveMoQLiteSession(
	sess *webtransport.Session,
	roomName livekit.RoomName,
	claims *auth.ClaimGrants,
	layer int32,
) {
	ctx := sess.Context()
	for {
		stream, err := sess.AcceptStream(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.logger.Debugw("could not accept moq-lite stream", "error", err)
			}
			return
		}

		go func() {
			defer func() {
				_ = stream.Close()
			}()
			if err := s.serveMoQLiteStream(ctx, sess, stream, roomName, claims, layer); err != nil {
				s.logger.Debugw("moq-lite stream failed", "error", err)
			}
		}()
	}
}

func (s *MoQService) serveMoQLiteStream(
	ctx context.Context,
	sess *webtransport.Session,
	stream *webtransport.Stream,
	roomName livekit.RoomName,
	claims *auth.ClaimGrants,
	layer int32,
) error {
	reader := quicvarint.NewReader(stream)
	streamType, err := quicvarint.Read(reader)
	if err != nil {
		return err
	}

	switch streamType {
	case moqLiteStreamSubscribe:
		return s.serveMoQLiteSubscribe(ctx, sess, stream, reader, roomName, claims, layer)
	case moqLiteStreamProbe:
		// Probe is optional telemetry in @moq/net. Closing cleanly tells the client
		// no probe data is available without failing the media subscription.
		return nil
	default:
		return fmt.Errorf("unsupported moq-lite stream type: %d", streamType)
	}
}

func (s *MoQService) serveMoQLiteSubscribe(
	ctx context.Context,
	sess *webtransport.Session,
	stream *webtransport.Stream,
	reader quicvarint.Reader,
	roomName livekit.RoomName,
	claims *auth.ClaimGrants,
	layer int32,
) error {
	subscribe, err := readMoQLiteSubscribe(reader)
	if err != nil {
		return err
	}

	resolved, status, err := s.resolveMoQLiteTrack(ctx, roomName, subscribe, claims)
	if err != nil {
		_ = writeMoQLiteSubscribeDrop(stream, s.config.WriteTimeout)
		return fmt.Errorf("could not resolve moq-lite subscription: status=%d, error=%w", status, err)
	}

	if err := writeMoQLiteSubscribeOK(stream, s.config.WriteTimeout, subscribe); err != nil {
		return err
	}
	go discardMoQLiteSubscribeUpdates(ctx, reader)

	trackSub, cached, unsubscribe := resolved.tap.Subscribe(layer)
	defer unsubscribe()

	if cached != nil {
		if err := s.writeMoQLiteSample(ctx, sess, subscribe.ID, cached, resolved.Timing); err != nil {
			return err
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-trackSub.done:
			return nil
		case sample := <-trackSub.queue:
			if sample == nil {
				continue
			}
			if err := s.writeMoQLiteSample(ctx, sess, subscribe.ID, sample, resolved.Timing); err != nil {
				return err
			}
		}
	}
}

func (s *MoQService) resolveMoQLiteTrack(
	ctx context.Context,
	defaultRoomName livekit.RoomName,
	subscribe moqLiteSubscribe,
	claims *auth.ClaimGrants,
) (moqLiteResolvedTrack, int, error) {
	var resolved moqLiteResolvedTrack
	roomName := defaultRoomName
	trackName, timingTrack := splitMoQLiteTrackName(subscribe.Track)
	if subscribe.Broadcast != "" {
		broadcastRoom := livekit.RoomName(subscribe.Broadcast)
		if defaultRoomName != "" && broadcastRoom != defaultRoomName {
			return resolved, http.StatusForbidden, fmt.Errorf("broadcast %q does not match token room %q", subscribe.Broadcast, defaultRoomName)
		}
		roomName = broadcastRoom
	}
	if roomName == "" {
		return resolved, http.StatusBadRequest, ErrNoRoomName
	}

	room := s.roomManager.GetRoom(ctx, roomName)
	if room == nil {
		return resolved, http.StatusNotFound, ErrRoomNotFound
	}

	var candidates []moqLiteResolvedTrack
	for _, participant := range room.GetParticipants() {
		for _, track := range participant.GetPublishedTracks() {
			if !isMoQSupportedTrack(track) {
				continue
			}
			if !participant.HasPermission(track.ID(), livekit.ParticipantIdentity(claims.Identity)) {
				continue
			}
			tap := s.tracks.AttachTrack(room, participant, track)
			if tap == nil {
				continue
			}
			if trackName == "" || string(track.ID()) == trackName || track.Name() == trackName {
				candidates = append(candidates, moqLiteResolvedTrack{
					moqResolvedTrack: moqResolvedTrack{room: room, participant: participant, track: track, tap: tap},
					Timing:           timingTrack,
				})
			}
		}
	}

	if subscribe.Track != "" && len(candidates) == 0 {
		return resolved, http.StatusNotFound, fmt.Errorf("track not found or not available for moq-lite: %s", subscribe.Track)
	}
	if subscribe.Track == "" && len(candidates) != 1 {
		return resolved, http.StatusBadRequest, fmt.Errorf("track name is required when %d moq-compatible tracks are available", len(candidates))
	}
	if len(candidates) > 1 {
		return resolved, http.StatusBadRequest, fmt.Errorf("track name %q matched %d moq-compatible tracks", subscribe.Track, len(candidates))
	}

	return candidates[0], http.StatusOK, nil
}

func splitMoQLiteTrackName(track string) (string, bool) {
	for _, suffix := range []string{".timing", ".metadata"} {
		if strings.HasSuffix(track, suffix) {
			return strings.TrimSuffix(track, suffix), true
		}
	}
	return track, false
}

func (s *MoQService) writeMoQLiteSample(
	ctx context.Context,
	sess *webtransport.Session,
	subscribeID uint64,
	sample *moqSample,
	timing bool,
) error {
	stream, err := sess.OpenUniStreamSync(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = stream.Close()
	}()
	payload := sample.Payload
	if timing {
		var err error
		payload, err = json.Marshal(moqLiteTimingSample{
			Sequence:        sample.Sequence,
			TrackID:         string(sample.TrackID),
			TrackName:       sample.TrackName,
			Layer:           sample.Layer,
			RTPTime:         sample.RTPTime,
			KeyFrame:        sample.KeyFrame,
			Cached:          sample.Cached,
			SentAtUnixNs:    sample.SentAtUnixNs,
			UserTimestampUs: sample.UserTimestampUs,
			FrameID:         sample.FrameID,
		})
		if err != nil {
			return err
		}
	}
	return writeMoQLiteGroup(stream, s.config.WriteTimeout, subscribeID, sample.Sequence, payload)
}

func readMoQLiteSubscribe(r quicvarint.Reader) (moqLiteSubscribe, error) {
	var subscribe moqLiteSubscribe
	data, err := readMoQLiteMessage(r)
	if err != nil {
		return subscribe, err
	}

	br := bytes.NewReader(data)
	reader := quicvarint.NewReader(br)
	subscribe.ID, err = quicvarint.Read(reader)
	if err != nil {
		return subscribe, err
	}
	subscribe.Broadcast, err = readMoQLiteString(reader)
	if err != nil {
		return subscribe, err
	}
	subscribe.Track, err = readMoQLiteString(reader)
	if err != nil {
		return subscribe, err
	}
	subscribe.Priority, err = readMoQLiteU8(reader)
	if err != nil {
		return subscribe, err
	}
	subscribe.Ordered, err = readMoQLiteBool(reader)
	if err != nil {
		return subscribe, err
	}
	subscribe.MaxLatency, err = quicvarint.Read(reader)
	if err != nil {
		return subscribe, err
	}
	startGroup, err := quicvarint.Read(reader)
	if err != nil {
		return subscribe, err
	}
	if startGroup > 0 {
		subscribe.StartGroup = startGroup - 1
		subscribe.HasStart = true
	}
	endGroup, err := quicvarint.Read(reader)
	if err != nil {
		return subscribe, err
	}
	if endGroup > 0 {
		subscribe.EndGroup = endGroup - 1
		subscribe.HasEnd = true
	}
	if br.Len() != 0 {
		return subscribe, fmt.Errorf("moq-lite subscribe had %d trailing bytes", br.Len())
	}
	return subscribe, nil
}

func readMoQLiteMessage(r quicvarint.Reader) ([]byte, error) {
	size, err := quicvarint.Read(r)
	if err != nil {
		return nil, err
	}
	if size > moqLiteMaxMessageSize {
		return nil, fmt.Errorf("moq-lite message too large: %d", size)
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	return data, nil
}

func readMoQLiteString(r quicvarint.Reader) (string, error) {
	data, err := readMoQLiteMessage(r)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func readMoQLiteU8(r quicvarint.Reader) (uint8, error) {
	b, err := r.ReadByte()
	return b, err
}

func readMoQLiteBool(r quicvarint.Reader) (bool, error) {
	value, err := readMoQLiteU8(r)
	if err != nil {
		return false, err
	}
	switch value {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, fmt.Errorf("invalid moq-lite bool: %d", value)
	}
}

func discardMoQLiteSubscribeUpdates(ctx context.Context, r quicvarint.Reader) {
	for {
		if ctx.Err() != nil {
			return
		}
		size, err := quicvarint.Read(r)
		if err != nil {
			return
		}
		if size > moqLiteMaxMessageSize {
			return
		}
		if _, err = io.CopyN(io.Discard, r, int64(size)); err != nil {
			return
		}
	}
}

func writeMoQLiteSubscribeOK(w interface {
	SetWriteDeadline(time.Time) error
	Write([]byte) (int, error)
}, timeout time.Duration, subscribe moqLiteSubscribe) error {
	var payload []byte
	payload = append(payload, subscribe.Priority)
	payload = append(payload, boolByte(subscribe.Ordered))
	payload = quicvarint.Append(payload, subscribe.MaxLatency)
	payload = appendMoQLiteOptionalGroup(payload, subscribe.HasStart, subscribe.StartGroup)
	payload = appendMoQLiteOptionalGroup(payload, subscribe.HasEnd, subscribe.EndGroup)

	var response []byte
	response = quicvarint.Append(response, moqLiteSubscribeOK)
	response = appendMoQLiteMessage(response, payload)
	return writeMoQLiteBytes(w, timeout, response)
}

func writeMoQLiteSubscribeDrop(w interface {
	SetWriteDeadline(time.Time) error
	Write([]byte) (int, error)
}, timeout time.Duration) error {
	var payload []byte
	payload = quicvarint.Append(payload, 0)
	payload = quicvarint.Append(payload, 0)
	payload = quicvarint.Append(payload, 1)

	var response []byte
	response = quicvarint.Append(response, moqLiteSubscribeDrop)
	response = appendMoQLiteMessage(response, payload)
	return writeMoQLiteBytes(w, timeout, response)
}

func writeMoQLiteGroup(w interface {
	SetWriteDeadline(time.Time) error
	Write([]byte) (int, error)
}, timeout time.Duration, subscribeID uint64, sequence uint64, payload []byte) error {
	var group []byte
	group = quicvarint.Append(group, subscribeID)
	group = quicvarint.Append(group, sequence)

	data := make([]byte, 0, 1+len(group)+len(payload)+16)
	data = append(data, moqLiteStreamGroup)
	data = appendMoQLiteMessage(data, group)
	data = quicvarint.Append(data, uint64(len(payload)))
	data = append(data, payload...)
	return writeMoQLiteBytes(w, timeout, data)
}

func appendMoQLiteOptionalGroup(dst []byte, ok bool, group uint64) []byte {
	if !ok {
		return quicvarint.Append(dst, 0)
	}
	return quicvarint.Append(dst, group+1)
}

func appendMoQLiteMessage(dst []byte, payload []byte) []byte {
	dst = quicvarint.Append(dst, uint64(len(payload)))
	return append(dst, payload...)
}

func writeMoQLiteBytes(w interface {
	SetWriteDeadline(time.Time) error
	Write([]byte) (int, error)
}, timeout time.Duration, data []byte) error {
	if err := w.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	_, err := w.Write(data)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}
