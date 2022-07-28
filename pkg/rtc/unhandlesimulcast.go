package rtc

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
)

const (
	simulcastProbeCount = 10
)

type UnhandleSimulcastOption func(r *UnhandleSimulcastInterceptor) error

func UnhandleSimulcastTracks(tracks map[uint32]SimulcastTrackInfo) UnhandleSimulcastOption {
	return func(r *UnhandleSimulcastInterceptor) error {
		r.simTracks = tracks
		return nil
	}
}

type UnhandleSimulcastInterceptorFactory struct {
	opts []UnhandleSimulcastOption
}

func (f *UnhandleSimulcastInterceptorFactory) NewInterceptor(id string) (interceptor.Interceptor, error) {
	i := &UnhandleSimulcastInterceptor{simTracks: map[uint32]SimulcastTrackInfo{}}
	for _, o := range f.opts {
		if err := o(i); err != nil {
			return nil, err
		}
	}
	return i, nil
}

func NewUnhandleSimulcastInterceptorFactory(opts ...UnhandleSimulcastOption) (*UnhandleSimulcastInterceptorFactory, error) {
	return &UnhandleSimulcastInterceptorFactory{opts: opts}, nil
}

type unhandleSimulcastRTPReader struct {
	SimulcastTrackInfo
	tryTimes            int
	reader              interceptor.RTPReader
	midExtensionID      uint8
	streamIDExtensionID uint8
}

func (r *unhandleSimulcastRTPReader) Read(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
	n, a, err := r.reader.Read(b, a)
	if r.tryTimes < 0 || err != nil {
		return n, a, err
	}

	header := rtp.Header{}
	hsize, err := header.Unmarshal(b[:n])
	if err != nil {
		return n, a, nil
	}
	var mid, rid string
	if payload := header.GetExtension(r.midExtensionID); payload != nil {
		mid = string(payload)
	}

	if payload := header.GetExtension(r.streamIDExtensionID); payload != nil {
		rid = string(payload)
	}

	if mid != "" && rid != "" {
		r.tryTimes = -1
		return n, a, nil
	}

	r.tryTimes--

	if mid == "" {
		header.SetExtension(r.midExtensionID, []byte(r.Mid))
	}
	if rid == "" {
		header.SetExtension(r.streamIDExtensionID, []byte(r.Rid))
	}

	hsize2 := header.MarshalSize()

	if hsize2-hsize+n > len(b) { // no enough buf to set extension
		return n, a, nil
	}
	copy(b[hsize2:], b[hsize:n])
	header.MarshalTo(b)
	return hsize2 - hsize + n, a, nil
}

type UnhandleSimulcastInterceptor struct {
	interceptor.NoOp
	simTracks map[uint32]SimulcastTrackInfo
}

func getHeaderExtensionID(extensions []interceptor.RTPHeaderExtension, extension webrtc.RTPHeaderExtensionCapability) int {
	for _, h := range extensions {
		if extension.URI == h.URI {
			return h.ID
		}
	}
	return 0
}

func (u *UnhandleSimulcastInterceptor) BindRemoteStream(info *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	if t, ok := u.simTracks[info.SSRC]; ok {
		// if we support fec for simulcast streams at future, should get rsid extensions
		midExtensionID := getHeaderExtensionID(info.RTPHeaderExtensions, webrtc.RTPHeaderExtensionCapability{URI: sdp.SDESMidURI})
		streamIDExtensionID := getHeaderExtensionID(info.RTPHeaderExtensions, webrtc.RTPHeaderExtensionCapability{URI: sdp.SDESRTPStreamIDURI})
		if midExtensionID == 0 || streamIDExtensionID == 0 {
			return reader
		}

		return &unhandleSimulcastRTPReader{
			SimulcastTrackInfo:  t,
			reader:              reader,
			tryTimes:            simulcastProbeCount,
			midExtensionID:      uint8(midExtensionID),
			streamIDExtensionID: uint8(streamIDExtensionID),
		}
	}
	return reader
}
