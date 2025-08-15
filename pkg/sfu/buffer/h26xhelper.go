package buffer

import (
	"errors"
	"fmt"
)

// SPSInfo holds parsed SPS parameters
type SPSInfo struct {
	ChromaFormatIDC             uint
	PicWidthInLumaSamples       uint
	PicHeightInLumaSamples      uint
	ConformanceWindowFlag       bool
	ConfWinLeftOffset           uint
	ConfWinRightOffset          uint
	ConfWinTopOffset            uint
	ConfWinBottomOffset         uint
	CodedWidth, CodedHeight     uint // Raw coded resolution
	DisplayWidth, DisplayHeight uint // Resolution after conformance window cropping
}

// -------- BitReader --------
type BitReader struct {
	data []byte
	pos  int // bit position
}

func NewBitReader(data []byte) *BitReader {
	return &BitReader{data: data}
}

func (br *BitReader) left() int {
	return len(br.data)*8 - br.pos
}

func (br *BitReader) ReadBits(n int) (uint, error) {
	if n < 0 || br.left() < n {
		return 0, errors.New("not enough bits")
	}
	var v uint
	for i := 0; i < n; i++ {
		bytePos := br.pos / 8
		bitPos := 7 - (br.pos % 8)
		bit := (br.data[bytePos] >> bitPos) & 1
		v = (v << 1) | uint(bit)
		br.pos++
	}
	return v, nil
}

func (br *BitReader) ReadFlag() (bool, error) {
	b, err := br.ReadBits(1)
	return b == 1, err
}

func (br *BitReader) ReadUE() (uint, error) {
	// Unsigned Exp-Golomb
	zeros := 0
	for {
		bit, err := br.ReadBits(1)
		if err != nil {
			return 0, err
		}
		if bit == 0 {
			zeros++
			continue
		}
		break // hit the stop bit '1'
	}
	if zeros == 0 {
		return 0, nil
	}
	info, err := br.ReadBits(zeros)
	if err != nil {
		return 0, err
	}
	return (1<<zeros - 1) + info, nil
}

func (br *BitReader) ReadSE() (int, error) {
	ueVal, err := br.ReadUE()
	if err != nil {
		return 0, err
	}
	k := int(ueVal)
	var val int
	if k%2 == 0 {
		val = -int(k / 2)
	} else {
		val = (k + 1) / 2
	}
	return val, nil
}

// ------------------------- H265 -------------------------

// stripStartCode removes 00 00 01 or 00 00 00 01 if present.
func stripStartCode(b []byte) []byte {
	if len(b) >= 4 && b[0] == 0x00 && b[1] == 0x00 && b[2] == 0x00 && b[3] == 0x01 {
		return b[4:]
	}
	if len(b) >= 3 && b[0] == 0x00 && b[1] == 0x00 && b[2] == 0x01 {
		return b[3:]
	}
	return b
}

// removeEmulationPreventionBytes removes 0x03 after 0x0000
func removeEmulationPreventionBytes(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if i > 1 && data[i] == 0x03 && data[i-1] == 0x00 && data[i-2] == 0x00 {
			continue
		}
		out = append(out, data[i])
	}
	return out
}

// parseH265SPS parses a full H.265 SPS NAL unit
func parseH265SPS(nal []byte) (*SPSInfo, error) {
	// Optional start code
	nal = stripStartCode(nal)

	// Remove emulation prevention bytes across the NAL
	rbsp := removeEmulationPreventionBytes(nal)

	br := NewBitReader(rbsp)

	// ---- NAL header (16 bits): forbidden_zero_bit(1), nal_unit_type(6), nuh_layer_id(6), nuh_temporal_id_plus1(3)
	if _, err := br.ReadBits(1); err != nil { // forbidden_zero_bit
		return nil, err
	}
	nalUnitType, err := br.ReadBits(6)
	if err != nil {
		return nil, err
	}
	if _, err = br.ReadBits(6); err != nil { // nuh_layer_id
		return nil, err
	}
	if _, err = br.ReadBits(3); err != nil { // nuh_temporal_id_plus1
		return nil, err
	}
	// 33 = SPS
	if nalUnitType != 33 {
		return nil, fmt.Errorf("not an HEVC SPS NAL (type=%d)", nalUnitType)
	}

	// ---- sps_video_parameter_set_id u(4), sps_max_sub_layers_minus1 u(3), sps_temporal_id_nesting_flag u(1)
	if _, err = br.ReadBits(4); err != nil {
		return nil, err
	}
	maxSubLayersMinus1, err := br.ReadBits(3)
	if err != nil {
		return nil, err
	}
	if _, err = br.ReadBits(1); err != nil {
		return nil, err
	}

	// ---- profile_tier_level(1, max_sub_layers_minus1)
	// general_profile_space u(2), general_tier_flag u(1), general_profile_idc u(5)
	if _, err = br.ReadBits(2 + 1 + 5); err != nil {
		return nil, err
	}
	// general_profile_compatibility_flags u(32)
	if _, err = br.ReadBits(32); err != nil {
		return nil, err
	}
	// general_constraint_indicator_flags u(48)
	if _, err = br.ReadBits(16); err != nil {
		return nil, err
	}
	if _, err = br.ReadBits(32); err != nil {
		return nil, err
	}
	// general_level_idc u(8)
	if _, err = br.ReadBits(8); err != nil {
		return nil, err
	}

	subLayerProfilePresentFlag := make([]bool, maxSubLayersMinus1)
	subLayerLevelPresentFlag := make([]bool, maxSubLayersMinus1)
	for i := uint(0); i < maxSubLayersMinus1; i++ {
		f1, err := br.ReadFlag()
		if err != nil {
			return nil, err
		}
		f2, err := br.ReadFlag()
		if err != nil {
			return nil, err
		}
		subLayerProfilePresentFlag[i] = f1
		subLayerLevelPresentFlag[i] = f2
	}
	if maxSubLayersMinus1 > 0 {
		// reserved_zero_2bits for i = maxSubLayersMinus1 .. 7
		for i := maxSubLayersMinus1; i < 8; i++ {
			if _, err := br.ReadBits(2); err != nil {
				return nil, err
			}
		}
	}
	for i := uint(0); i < maxSubLayersMinus1; i++ {
		if subLayerProfilePresentFlag[i] {
			if _, err = br.ReadBits(2 + 1 + 5); err != nil {
				return nil, err
			}
			if _, err = br.ReadBits(32); err != nil {
				return nil, err
			}
			if _, err = br.ReadBits(48); err != nil {
				return nil, err
			}
		}
		if subLayerLevelPresentFlag[i] {
			if _, err = br.ReadBits(8); err != nil {
				return nil, err
			}
		}
	}

	// ---- Now the core SPS fields we need
	_, err = br.ReadUE() // sps_seq_parameter_set_id
	if err != nil {
		return nil, err
	}

	chromaFormatIDC, err := br.ReadUE()
	if err != nil {
		return nil, err
	}
	if chromaFormatIDC == 3 {
		// separate_colour_plane_flag u(1)
		if _, err := br.ReadFlag(); err != nil {
			return nil, err
		}
	}

	picW, err := br.ReadUE() // pic_width_in_luma_samples
	if err != nil {
		return nil, err
	}
	picH, err := br.ReadUE() // pic_height_in_luma_samples
	if err != nil {
		return nil, err
	}

	confFlag, err := br.ReadFlag()
	if err != nil {
		return nil, err
	}
	var l, r, t, b uint
	if confFlag {
		if l, err = br.ReadUE(); err != nil {
			return nil, err
		}
		if r, err = br.ReadUE(); err != nil {
			return nil, err
		}
		if t, err = br.ReadUE(); err != nil {
			return nil, err
		}
		if b, err = br.ReadUE(); err != nil {
			return nil, err
		}
	}

	// crop unit size depends on chroma_format_idc
	subWidthC, subHeightC := getSubWidthC(chromaFormatIDC), getSubHeightC(chromaFormatIDC)

	info := &SPSInfo{
		ChromaFormatIDC:        chromaFormatIDC,
		PicWidthInLumaSamples:  picW,
		PicHeightInLumaSamples: picH,
		ConformanceWindowFlag:  confFlag,
		ConfWinLeftOffset:      l,
		ConfWinRightOffset:     r,
		ConfWinTopOffset:       t,
		ConfWinBottomOffset:    b,
		CodedWidth:             picW,
		CodedHeight:            picH,
	}

	if confFlag {
		w := int(picW) - int(l+r)*int(subWidthC)
		h := int(picH) - int(t+b)*int(subHeightC)
		if w < 0 {
			w = 0
		}
		if h < 0 {
			h = 0
		}
		info.DisplayWidth = uint(w)
		info.DisplayHeight = uint(h)
	} else {
		info.DisplayWidth = picW
		info.DisplayHeight = picH
	}

	return info, nil
}

func getSubWidthC(chromaFormatIDC uint) uint {
	if chromaFormatIDC == 1 || chromaFormatIDC == 2 {
		return 2
	}
	return 1
}

func getSubHeightC(chromaFormatIDC uint) uint {
	if chromaFormatIDC == 1 {
		return 2
	}
	return 1
}

func ExtractH265VideoSize(payload []byte) VideoSize {
	if len(payload) < 2 {
		return VideoSize{}
	}
	nalType := (payload[0] >> 1) & 0x3F

	var spsNalu []byte
	switch nalType {
	case 33: // SPS
		spsNalu = payload
	case 48: // Aggregation Packet (AP)
		// skip 2-byte header
		i := 2
		for i+2 <= len(payload) {
			nalSize := int(payload[i])<<8 | int(payload[i+1])
			i += 2
			if i+nalSize > len(payload) {
				break
			}
			nalUnit := payload[i : i+nalSize]
			nt := (nalUnit[0] >> 1) & 0x3F
			if nt == 33 {
				spsNalu = nalUnit
				break
			}
			i += nalSize
		}
	}

	if len(spsNalu) > 0 {
		info, err := parseH265SPS(spsNalu)
		if err != nil {
			return VideoSize{}
		}
		return VideoSize{Width: uint32(info.DisplayWidth), Height: uint32(info.DisplayHeight)}
	}

	return VideoSize{}
}

// ------------------------- H264 -------------------------

// parseH264SPS parses a full H.264 SPS NAL unit into SPSInfo
func parseH264SPS(nal []byte) (*SPSInfo, error) {
	if len(nal) < 1 {
		return nil, errors.New("empty SPS NAL")
	}
	nal = stripStartCode(nal)
	nalType := nal[0] & 0x1F
	if nalType != 7 {
		return nil, fmt.Errorf("not an SPS NAL (type=%d)", nalType)
	}

	rbsp := removeEmulationPreventionBytes(nal[1:]) // skip NAL header
	br := NewBitReader(rbsp)

	profileIDC, _ := br.ReadBits(8)
	_, _ = br.ReadBits(8) // constraint flags
	_, _ = br.ReadBits(8) // level_idc
	_, _ = br.ReadUE()    // seq_parameter_set_id

	chromaFormatIDC := uint(1)
	if profileIDC == 100 || profileIDC == 110 || profileIDC == 122 || profileIDC == 244 ||
		profileIDC == 44 || profileIDC == 83 || profileIDC == 86 || profileIDC == 118 || profileIDC == 128 {
		chromaFormatIDC, _ = br.ReadUE()
		if chromaFormatIDC == 3 {
			br.ReadFlag() // separate_colour_plane_flag
		}
		br.ReadUE()                   // bit_depth_luma_minus8
		br.ReadUE()                   // bit_depth_chroma_minus8
		br.ReadFlag()                 // qpprime_y_zero_transform_bypass_flag
		if v, _ := br.ReadFlag(); v { // seq_scaling_matrix_present_flag
			for i := 0; i < 8; i++ {
				br.ReadFlag()
			}
		}
	}

	br.ReadUE() // log2_max_frame_num_minus4
	pocType, _ := br.ReadUE()
	if pocType == 0 {
		br.ReadUE()
	} else if pocType == 1 {
		br.ReadFlag()
		br.ReadSE()
		br.ReadSE()
		cnt, _ := br.ReadUE()
		for i := uint(0); i < cnt; i++ {
			br.ReadSE()
		}
	}

	br.ReadUE()   // max_num_ref_frames
	br.ReadFlag() // gaps_in_frame_num_value_allowed_flag

	wMbs, _ := br.ReadUE()
	hMapUnits, _ := br.ReadUE()
	frameMbsOnly, _ := br.ReadFlag()
	if !frameMbsOnly {
		br.ReadFlag() // mb_adaptive_frame_field_flag
	}
	br.ReadFlag() // direct_8x8_inference_flag

	var cropLeft, cropRight, cropTop, cropBottom uint
	if frameCropping, _ := br.ReadFlag(); frameCropping {
		cropLeft, _ = br.ReadUE()
		cropRight, _ = br.ReadUE()
		cropTop, _ = br.ReadUE()
		cropBottom, _ = br.ReadUE()
	}

	width := (wMbs + 1) * 16
	height := (hMapUnits + 1) * 16
	if !frameMbsOnly {
		height *= 2
	}

	subWidthC := getSubWidthC(chromaFormatIDC)
	subHeightC := getSubHeightC(chromaFormatIDC)
	cropUnitX := subWidthC
	cropUnitY := subHeightC
	if chromaFormatIDC == 0 {
		cropUnitX = 1
		if !frameMbsOnly {
			cropUnitY = 2
		} else {
			cropUnitY = 1
		}
	} else if !frameMbsOnly {
		cropUnitY *= 2
	}

	info := &SPSInfo{
		ChromaFormatIDC:        chromaFormatIDC,
		PicWidthInLumaSamples:  width,
		PicHeightInLumaSamples: height,
		ConformanceWindowFlag:  cropLeft+cropRight+cropTop+cropBottom > 0,
		ConfWinLeftOffset:      cropLeft,
		ConfWinRightOffset:     cropRight,
		ConfWinTopOffset:       cropTop,
		ConfWinBottomOffset:    cropBottom,
		CodedWidth:             width,
		CodedHeight:            height,
		DisplayWidth:           width - (cropLeft+cropRight)*cropUnitX,
		DisplayHeight:          height - (cropTop+cropBottom)*cropUnitY,
	}

	return info, nil
}

// ExtractH264VideoSize extracts resolution from H.264 RTP payload
func ExtractH264VideoSize(payload []byte) VideoSize {
	if len(payload) < 1 {
		return VideoSize{}
	}

	parseNAL := func(nal []byte) VideoSize {
		info, err := parseH264SPS(nal)
		if err != nil {
			return VideoSize{}
		}
		return VideoSize{Width: uint32(info.DisplayWidth), Height: uint32(info.DisplayHeight)}
	}

	nalType := payload[0] & 0x1F

	switch nalType {
	case 7: // SPS NAL
		return parseNAL(payload)

	case 28: // FU-A
		if len(payload) < 2 {
			return VideoSize{}
		}
		start := (payload[1] & 0x80) != 0
		if !start {
			return VideoSize{}
		}
		nalHeader := (payload[0] & 0xE0) | (payload[1] & 0x1F)
		sps := append([]byte{nalHeader}, payload[2:]...)
		return parseNAL(sps)

	case 24, 25, 26, 27: // STAP-A/B, MTAP16, MTAP24
		offset := 1
		if nalType == 25 { // STAP-B has 16-bit DON
			offset += 2
		} else if nalType == 26 { // MTAP16
			offset += 3
		} else if nalType == 27 { // MTAP24
			offset += 4
		}

		for offset+2 <= len(payload) {
			naluSize := int(payload[offset])<<8 | int(payload[offset+1])
			offset += 2
			if offset+naluSize > len(payload) {
				break
			}
			nalu := payload[offset : offset+naluSize]
			if nalu[0]&0x1F == 7 { // SPS
				return parseNAL(nalu)
			}
			offset += naluSize
		}
		return VideoSize{}

	default:
		return VideoSize{}
	}
}
