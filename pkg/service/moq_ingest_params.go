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
	"fmt"
	"net/http"
	"strconv"

	"github.com/livekit/protocol/livekit"
)

type moqIngestPublishParams struct {
	TrackName    string
	Width        uint32
	Height       uint32
	FPS          uint32
	ResumeToken  string
	TrackID      livekit.TrackID
	NextSequence uint64
}

func parseMoQIngestPublishParams(r *http.Request) (moqIngestPublishParams, error) {
	q := r.URL.Query()
	params := moqIngestPublishParams{
		TrackName: q.Get("track"),
		Width:     640,
		Height:    480,
		FPS:       30,
	}
	if params.TrackName == "" {
		params.TrackName = "camera"
	}
	if width := q.Get("width"); width != "" {
		parsed, err := strconv.ParseUint(width, 10, 32)
		if err != nil || parsed == 0 {
			return params, fmt.Errorf("invalid width: %q", width)
		}
		params.Width = uint32(parsed)
	}
	if height := q.Get("height"); height != "" {
		parsed, err := strconv.ParseUint(height, 10, 32)
		if err != nil || parsed == 0 {
			return params, fmt.Errorf("invalid height: %q", height)
		}
		params.Height = uint32(parsed)
	}
	if fps := q.Get("fps"); fps != "" {
		parsed, err := strconv.ParseUint(fps, 10, 32)
		if err != nil || parsed == 0 {
			return params, fmt.Errorf("invalid fps: %q", fps)
		}
		params.FPS = uint32(parsed)
	}
	params.ResumeToken = q.Get("resume_token")
	params.TrackID = livekit.TrackID(q.Get("track_sid"))
	if next := q.Get("next_sequence"); next != "" {
		parsed, err := strconv.ParseUint(next, 10, 64)
		if err != nil {
			return params, fmt.Errorf("invalid next_sequence: %q", next)
		}
		params.NextSequence = parsed
	}
	return params, nil
}
