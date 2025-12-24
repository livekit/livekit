// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sfu

import "github.com/pion/webrtc/v4"

type TrackRemote interface {
	ID() string
	RID() string
	Msid() string
	SSRC() webrtc.SSRC
	RtxSSRC() webrtc.SSRC
	StreamID() string
	Kind() webrtc.RTPCodecType
	Codec() webrtc.RTPCodecParameters
	RTCTrack() *webrtc.TrackRemote
}

// TrackRemoteFromSdp represents a remote track that could be created by the sdp.
// It is a wrapper around the webrtc.TrackRemote and return the Codec from sdp
// before the first RTP packet is received.
type TrackRemoteFromSdp struct {
	*webrtc.TrackRemote
	sdpCodec webrtc.RTPCodecParameters
}

func NewTrackRemoteFromSdp(track *webrtc.TrackRemote, codec webrtc.RTPCodecParameters) *TrackRemoteFromSdp {
	return &TrackRemoteFromSdp{
		TrackRemote: track,
		sdpCodec:    codec,
	}
}

func (t *TrackRemoteFromSdp) Codec() webrtc.RTPCodecParameters {
	return t.sdpCodec
}

func (t *TrackRemoteFromSdp) RTCTrack() *webrtc.TrackRemote {
	return t.TrackRemote
}
