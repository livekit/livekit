/**
 * NALU (Network Abstraction Layer Unit) utilities for H.264 and H.265 video processing
 * Contains functions for parsing and working with NALUs in video frames
 */

/**
 * Mask for extracting NALU type from H.264 header byte
 */
const kH264NaluTypeMask = 0x1f;

/**
 * H.264 NALU types according to RFC 6184
 */
enum H264NALUType {
  /** Coded slice of a non-IDR picture */
  SLICE_NON_IDR = 1,
  /** Coded slice data partition A */
  SLICE_PARTITION_A = 2,
  /** Coded slice data partition B */
  SLICE_PARTITION_B = 3,
  /** Coded slice data partition C */
  SLICE_PARTITION_C = 4,
  /** Coded slice of an IDR picture */
  SLICE_IDR = 5,
  /** Supplemental enhancement information */
  SEI = 6,
  /** Sequence parameter set */
  SPS = 7,
  /** Picture parameter set */
  PPS = 8,
  /** Access unit delimiter */
  AUD = 9,
  /** End of sequence */
  END_SEQ = 10,
  /** End of stream */
  END_STREAM = 11,
  /** Filler data */
  FILLER_DATA = 12,
  /** Sequence parameter set extension */
  SPS_EXT = 13,
  /** Prefix NAL unit */
  PREFIX_NALU = 14,
  /** Subset sequence parameter set */
  SUBSET_SPS = 15,
  /** Depth parameter set */
  DPS = 16,

  // 17, 18 reserved

  /** Coded slice of an auxiliary coded picture without partitioning */
  SLICE_AUX = 19,
  /** Coded slice extension */
  SLICE_EXT = 20,
  /** Coded slice extension for a depth view component or a 3D-AVC texture view component */
  SLICE_LAYER_EXT = 21,

  // 22, 23 reserved
}

/**
 * H.265/HEVC NALU types according to ITU-T H.265
 */
enum H265NALUType {
  /** Coded slice segment of a non-TSA, non-STSA trailing picture */
  TRAIL_N = 0,
  /** Coded slice segment of a non-TSA, non-STSA trailing picture */
  TRAIL_R = 1,
  /** Coded slice segment of a TSA picture */
  TSA_N = 2,
  /** Coded slice segment of a TSA picture */
  TSA_R = 3,
  /** Coded slice segment of an STSA picture */
  STSA_N = 4,
  /** Coded slice segment of an STSA picture */
  STSA_R = 5,
  /** Coded slice segment of a RADL picture */
  RADL_N = 6,
  /** Coded slice segment of a RADL picture */
  RADL_R = 7,
  /** Coded slice segment of a RASL picture */
  RASL_N = 8,
  /** Coded slice segment of a RASL picture */
  RASL_R = 9,

  // 10-15 reserved

  /** Coded slice segment of a BLA picture */
  BLA_W_LP = 16,
  /** Coded slice segment of a BLA picture */
  BLA_W_RADL = 17,
  /** Coded slice segment of a BLA picture */
  BLA_N_LP = 18,
  /** Coded slice segment of an IDR picture */
  IDR_W_RADL = 19,
  /** Coded slice segment of an IDR picture */
  IDR_N_LP = 20,
  /** Coded slice segment of a CRA picture */
  CRA_NUT = 21,

  // 22-31 reserved

  /** Video parameter set */
  VPS_NUT = 32,
  /** Sequence parameter set */
  SPS_NUT = 33,
  /** Picture parameter set */
  PPS_NUT = 34,
  /** Access unit delimiter */
  AUD_NUT = 35,
  /** End of sequence */
  EOS_NUT = 36,
  /** End of bitstream */
  EOB_NUT = 37,
  /** Filler data */
  FD_NUT = 38,
  /** Supplemental enhancement information */
  PREFIX_SEI_NUT = 39,
  /** Supplemental enhancement information */
  SUFFIX_SEI_NUT = 40,

  // 41-47 reserved
  // 48-63 unspecified
}

/**
 * Parse H.264 NALU type from the first byte of a NALU
 * @param startByte First byte of the NALU
 * @returns H.264 NALU type
 */
function parseH264NALUType(startByte: number): H264NALUType {
  return startByte & kH264NaluTypeMask;
}

/**
 * Parse H.265 NALU type from the first byte of a NALU
 * @param firstByte First byte of the NALU
 * @returns H.265 NALU type
 */
function parseH265NALUType(firstByte: number): H265NALUType {
  // In H.265, NALU type is in bits 1-6 (shifted right by 1)
  return (firstByte >> 1) & 0x3f;
}

/**
 * Check if H.264 NALU type is a slice (IDR or non-IDR)
 * @param naluType H.264 NALU type
 * @returns True if the NALU is a slice
 */
function isH264SliceNALU(naluType: H264NALUType): boolean {
  return naluType === H264NALUType.SLICE_IDR || naluType === H264NALUType.SLICE_NON_IDR;
}

/**
 * Check if H.265 NALU type is a slice
 * @param naluType H.265 NALU type
 * @returns True if the NALU is a slice
 */
function isH265SliceNALU(naluType: H265NALUType): boolean {
  return (
    // VCL NALUs (Video Coding Layer) - slice segments
    naluType === H265NALUType.TRAIL_N ||
    naluType === H265NALUType.TRAIL_R ||
    naluType === H265NALUType.TSA_N ||
    naluType === H265NALUType.TSA_R ||
    naluType === H265NALUType.STSA_N ||
    naluType === H265NALUType.STSA_R ||
    naluType === H265NALUType.RADL_N ||
    naluType === H265NALUType.RADL_R ||
    naluType === H265NALUType.RASL_N ||
    naluType === H265NALUType.RASL_R ||
    naluType === H265NALUType.BLA_W_LP ||
    naluType === H265NALUType.BLA_W_RADL ||
    naluType === H265NALUType.BLA_N_LP ||
    naluType === H265NALUType.IDR_W_RADL ||
    naluType === H265NALUType.IDR_N_LP ||
    naluType === H265NALUType.CRA_NUT
  );
}

/**
 * Detected codec type from NALU analysis
 */
export type DetectedCodec = 'h264' | 'h265' | 'unknown';

/**
 * Result of NALU processing for frame encryption
 */
export interface NALUProcessingResult {
  /** Number of unencrypted bytes at the start of the frame */
  unencryptedBytes: number;
  /** Detected codec type */
  detectedCodec: DetectedCodec;
  /** Whether this frame requires NALU processing */
  requiresNALUProcessing: boolean;
}

/**
 * Detect codec type by examining NALU types in the data
 * @param data Frame data
 * @param naluIndices Indices where NALUs start
 * @returns Detected codec type
 */
function detectCodecFromNALUs(data: Uint8Array, naluIndices: number[]): DetectedCodec {
  for (const naluIndex of naluIndices) {
    if (isH264SliceNALU(parseH264NALUType(data[naluIndex]))) return 'h264';
    if (isH265SliceNALU(parseH265NALUType(data[naluIndex]))) return 'h265';
  }
  return 'unknown';
}

/**
 * Find the first slice NALU and return the number of unencrypted bytes
 * @param data Frame data
 * @param naluIndices Indices where NALUs start
 * @param codec Codec type to use for parsing
 * @returns Number of unencrypted bytes (index + 2) or null if no slice found
 */
function findSliceNALUUnencryptedBytes(
  data: Uint8Array,
  naluIndices: number[],
  codec: 'h264' | 'h265',
): number | null {
  for (const index of naluIndices) {
    if (codec === 'h265') {
      const type = parseH265NALUType(data[index]);
      if (isH265SliceNALU(type)) {
        return index + 2;
      }
    } else {
      const type = parseH264NALUType(data[index]);
      if (isH264SliceNALU(type)) {
        return index + 2;
      }
    }
  }
  return null;
}

/**
 * Find all NALU start indices in a byte stream
 * Supports both H.264 and H.265 with 3-byte and 4-byte start codes
 *
 * This function slices the NALUs present in the supplied buffer, assuming it is already byte-aligned.
 * Code adapted from https://github.com/medooze/h264-frame-parser/blob/main/lib/NalUnits.ts to return indices only
 *
 * @param stream Byte stream containing NALUs
 * @returns Array of indices where NALUs start (after the start code)
 */
function findNALUIndices(stream: Uint8Array): number[] {
  const result: number[] = [];
  let start = 0,
    pos = 0,
    searchLength = stream.length - 3; // Changed to -3 to handle 4-byte start codes

  while (pos < searchLength) {
    // skip until end of current NALU - check for both 3-byte and 4-byte start codes
    while (pos < searchLength) {
      // Check for 4-byte start code: 0x00 0x00 0x00 0x01
      if (
        pos < searchLength - 1 &&
        stream[pos] === 0 &&
        stream[pos + 1] === 0 &&
        stream[pos + 2] === 0 &&
        stream[pos + 3] === 1
      ) {
        break;
      }
      // Check for 3-byte start code: 0x00 0x00 0x01
      if (stream[pos] === 0 && stream[pos + 1] === 0 && stream[pos + 2] === 1) {
        break;
      }
      pos++;
    }

    if (pos >= searchLength) pos = stream.length;

    // remove trailing zeros from current NALU
    let end = pos;
    while (end > start && stream[end - 1] === 0) end--;

    // save current NALU
    if (start === 0) {
      if (end !== start) throw TypeError('byte stream contains leading data');
    } else {
      result.push(start);
    }

    // begin new NALU - determine start code length
    let startCodeLength = 3;
    if (
      pos < stream.length - 3 &&
      stream[pos] === 0 &&
      stream[pos + 1] === 0 &&
      stream[pos + 2] === 0 &&
      stream[pos + 3] === 1
    ) {
      startCodeLength = 4;
    }

    start = pos = pos + startCodeLength;
  }
  return result;
}

/**
 * Process NALU data for frame encryption, detecting codec and finding unencrypted bytes
 * @param data Frame data
 * @param knownCodec Known codec from other sources (optional)
 * @returns NALU processing result
 */
export function processNALUsForEncryption(
  data: Uint8Array,
  knownCodec?: 'h264' | 'h265',
): NALUProcessingResult {
  const naluIndices = findNALUIndices(data);
  const detectedCodec = knownCodec ?? detectCodecFromNALUs(data, naluIndices);

  if (detectedCodec === 'unknown') {
    return { unencryptedBytes: 0, detectedCodec, requiresNALUProcessing: false };
  }

  const unencryptedBytes = findSliceNALUUnencryptedBytes(data, naluIndices, detectedCodec);
  if (unencryptedBytes === null) {
    throw new TypeError('Could not find NALU');
  }

  return { unencryptedBytes, detectedCodec, requiresNALUProcessing: true };
}
