package sfu

import "github.com/pion/webrtc/v4"

type TrackRemote interface {
	ID() string
	RID() string
	Msid() string
	SSRC() webrtc.SSRC
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
