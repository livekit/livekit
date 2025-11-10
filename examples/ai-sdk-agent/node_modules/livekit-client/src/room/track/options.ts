import type { Track } from './Track';
import type {
  AudioProcessorOptions,
  TrackProcessor,
  VideoProcessorOptions,
} from './processor/types';

export interface TrackPublishDefaults {
  /**
   * encoding parameters for camera track
   */
  videoEncoding?: VideoEncoding;

  /**
   * Advanced codecs (VP9/AV1/H265) are not supported by all browser clients. When backupCodec is
   * set, when an incompatible client attempts to subscribe to the track, LiveKit
   * will automatically publish a secondary track encoded with the backup codec.
   *
   * You could customize specific encoding parameters of the backup track by
   * explicitly setting codec and encoding fields.
   *
   * Defaults to `true`
   */
  backupCodec?: true | false | { codec: BackupVideoCodec; encoding?: VideoEncoding };

  /**
   * When backup codec is enabled, there are two options to decide whether to
   * send the primary codec at the same time:
   *   * codec regression: publisher stops sending primary codec and all subscribers
   *       will receive backup codec even if the primary codec is supported on their browser. It is the default
   *       behavior and provides maximum compatibility. It also reduces CPU
   *       and bandwidth consumption for publisher.
   *   * multi-codec simulcast: publisher encodes and sends both codecs at same time,
   *       subscribers will get most efficient codec. It will provide most bandwidth
   *       efficiency, especially in the large 1:N room but requires more device performance
   *       and bandwidth consumption for publisher.
   */
  backupCodecPolicy?: BackupCodecPolicy;

  /**
   * encoding parameters for screen share track
   */
  screenShareEncoding?: VideoEncoding;

  /**
   * codec, defaults to vp8; for svc codecs, auto enable vp8
   * as backup. (TBD)
   */
  videoCodec?: VideoCodec;

  /**
   * which audio preset should be used for publishing (audio) tracks
   * defaults to [[AudioPresets.music]]
   */
  audioPreset?: AudioPreset;

  /**
   * dtx (Discontinuous Transmission of audio), enabled by default for mono tracks.
   */
  dtx?: boolean;

  /**
   * red (Redundant Audio Data), enabled by default for mono tracks.
   */
  red?: boolean;

  /**
   * publish track in stereo mode (or set to false to disable). defaults determined by capture channel count.
   */
  forceStereo?: boolean;

  /**
   * use simulcast, defaults to true.
   * When using simulcast, LiveKit will publish up to three versions of the stream
   * at various resolutions.
   */
  simulcast?: boolean;

  /**
   * scalability mode for svc codecs, defaults to 'L3T3_KEY'.
   * for svc codecs, simulcast is disabled.
   */
  scalabilityMode?: ScalabilityMode;

  /**
   * degradation preference
   */
  degradationPreference?: RTCDegradationPreference;

  /**
   * Up to two additional simulcast layers to publish in addition to the original
   * Track.
   * When left blank, it defaults to h180, h360.
   * If a SVC codec is used (VP9 or AV1), this field has no effect.
   *
   * To publish three total layers, you would specify:
   * {
   *   videoEncoding: {...}, // encoding of the primary layer
   *   videoSimulcastLayers: [
   *     VideoPresets.h540,
   *     VideoPresets.h216,
   *   ],
   * }
   */
  videoSimulcastLayers?: Array<VideoPreset>;

  /**
   * custom video simulcast layers for screen tracks
   * Note: the layers need to be ordered from lowest to highest quality
   */
  screenShareSimulcastLayers?: Array<VideoPreset>;

  /**
   * For local tracks, stop the underlying MediaStreamTrack when the track is muted (or paused)
   * on some platforms, this option is necessary to disable the microphone recording indicator.
   * Note: when this is enabled, and BT devices are connected, they will transition between
   * profiles (e.g. HFP to A2DP) and there will be an audible difference in playback.
   *
   * defaults to false
   */
  stopMicTrackOnMute?: boolean;

  /**
   * Enables preconnect buffer for a user's microphone track.
   * This is useful for reducing perceived latency when the user starts to speak before the connection is established.
   * Only works for agent use cases.
   *
   * Defaults to false.
   */
  preConnectBuffer?: boolean;
}

/**
 * Options when publishing tracks
 */
export interface TrackPublishOptions extends TrackPublishDefaults {
  /**
   * set a track name
   */
  name?: string;

  /**
   * Source of track, camera, microphone, or screen
   */
  source?: Track.Source;

  /**
   * Set stream name for the track. Audio and video tracks with the same stream name
   * will be placed in the same `MediaStream` and offer better synchronization.
   * By default, camera and microphone will be placed in a stream; as would screen_share and screen_share_audio
   */
  stream?: string;
}

export interface CreateLocalTracksOptions {
  /**
   * audio track options, true to create with defaults. false if audio shouldn't be created
   * default true
   */
  audio?: boolean | AudioCaptureOptions;

  /**
   * video track options, true to create with defaults. false if video shouldn't be created
   * default true
   */
  video?: boolean | VideoCaptureOptions;
}

export interface VideoCaptureOptions {
  /**
   * A ConstrainDOMString object specifying a device ID or an array of device
   * IDs which are acceptable and/or required.
   */
  deviceId?: ConstrainDOMString;

  /**
   * A ConstrainDouble specifying the frame rate or range of frame rates which are acceptable and/or required.
   */
  frameRate?: ConstrainDouble;

  /**
   * a facing or an array of facings which are acceptable and/or required.
   */
  facingMode?: 'user' | 'environment' | 'left' | 'right';

  resolution?: VideoResolution;

  /**
   * initialize the track with a given processor
   */
  processor?: TrackProcessor<Track.Kind.Video, VideoProcessorOptions>;
}

export interface ScreenShareCaptureOptions {
  /**
   * true to capture audio shared. browser support for audio capturing in
   * screenshare is limited: https://developer.mozilla.org/en-US/docs/Web/API/MediaDevices/getDisplayMedia#browser_compatibility
   */
  audio?: boolean | AudioCaptureOptions;

  /**
   * only allows for 'true' and chrome allows for additional options to be passed in
   * https://developer.chrome.com/docs/web-platform/screen-sharing-controls/#displaySurface
   */
  video?: true | { displaySurface?: 'window' | 'browser' | 'monitor' };

  /**
   * capture resolution, defaults to 1080 for all browsers other than Safari
   * On Safari 17, default resolution is not capped, due to a bug, specifying
   * any resolution at all would lead to a low-resolution capture.
   * https://bugs.webkit.org/show_bug.cgi?id=263015
   */
  resolution?: VideoResolution;

  /** a CaptureController object instance containing methods that can be used to further manipulate the capture session if included. */
  controller?: unknown; // TODO replace type with CaptureController once it lands in TypeScript

  /** specifies whether the browser should allow the user to select the current tab for capture */
  selfBrowserSurface?: 'include' | 'exclude';

  /** specifies whether the browser should display a control to allow the user to dynamically switch the shared tab during screen-sharing. */
  surfaceSwitching?: 'include' | 'exclude';

  /** specifies whether the browser should include the system audio among the possible audio sources offered to the user */
  systemAudio?: 'include' | 'exclude';

  /** specify the type of content, see: https://www.w3.org/TR/mst-content-hint/#video-content-hints */
  contentHint?: 'detail' | 'text' | 'motion';

  /**
   * Experimental option to control whether the audio playing in a tab will continue to be played out of a user's
   * local speakers when the tab is captured.
   */
  suppressLocalAudioPlayback?: boolean;

  /**
   * Experimental option to instruct the browser to offer the current tab as the most prominent capture source
   * @experimental
   * @see https://developer.mozilla.org/en-US/docs/Web/API/MediaDevices/getDisplayMedia#prefercurrenttab
   */
  preferCurrentTab?: boolean;
}

export interface AudioCaptureOptions {
  /**
   * specifies whether automatic gain control is preferred and/or required
   */
  autoGainControl?: ConstrainBoolean;

  /**
   * the channel count or range of channel counts which are acceptable and/or required
   */
  channelCount?: ConstrainULong;

  /**
   * A ConstrainDOMString object specifying a device ID or an array of device
   * IDs which are acceptable and/or required.
   */
  deviceId?: ConstrainDOMString;

  /**
   * whether or not echo cancellation is preferred and/or required
   */
  echoCancellation?: ConstrainBoolean;

  /**
   * the latency or range of latencies which are acceptable and/or required.
   */
  latency?: ConstrainDouble;

  /**
   * whether noise suppression is preferred and/or required.
   */
  noiseSuppression?: ConstrainBoolean;

  /**
   * @experimental
   * a stronger version of 'noiseSuppression', browser support is not widespread yet.
   * If this is set (and supported) the value for 'noiseSuppression' will be ignored
   * @see https://w3c.github.io/mediacapture-extensions/#voiceisolation-constraint
   */
  voiceIsolation?: ConstrainBoolean;

  /**
   * the sample rate or range of sample rates which are acceptable and/or required.
   */
  sampleRate?: ConstrainULong;

  /**
   * sample size or range of sample sizes which are acceptable and/or required.
   */
  sampleSize?: ConstrainULong;

  /**
   * initialize the track with a given processor
   */
  processor?: TrackProcessor<Track.Kind.Audio, AudioProcessorOptions>;
}

export interface AudioOutputOptions {
  /**
   * deviceId to output audio
   *
   * Only supported on browsers where `setSinkId` is available
   */
  deviceId?: string;
}

export interface VideoResolution {
  width: number;
  height: number;
  frameRate?: number;
  aspectRatio?: number;
}

export interface VideoEncoding {
  maxBitrate: number;
  maxFramerate?: number;
  priority?: RTCPriorityType;
}

export interface VideoPresetOptions {
  width: number;
  height: number;
  aspectRatio?: number;
  maxBitrate: number;
  maxFramerate?: number;
  priority?: RTCPriorityType;
}

export class VideoPreset {
  encoding: VideoEncoding;

  width: number;

  height: number;

  aspectRatio?: number;

  constructor(videoPresetOptions: VideoPresetOptions);
  constructor(
    width: number,
    height: number,
    maxBitrate: number,
    maxFramerate?: number,
    priority?: RTCPriorityType,
  );
  constructor(
    widthOrOptions: number | VideoPresetOptions,
    height?: number,
    maxBitrate?: number,
    maxFramerate?: number,
    priority?: RTCPriorityType,
  ) {
    if (typeof widthOrOptions === 'object') {
      this.width = widthOrOptions.width;
      this.height = widthOrOptions.height;
      this.aspectRatio = widthOrOptions.aspectRatio;
      this.encoding = {
        maxBitrate: widthOrOptions.maxBitrate,
        maxFramerate: widthOrOptions.maxFramerate,
        priority: widthOrOptions.priority,
      };
    } else if (height !== undefined && maxBitrate !== undefined) {
      this.width = widthOrOptions;
      this.height = height;
      this.aspectRatio = widthOrOptions / height;
      this.encoding = {
        maxBitrate,
        maxFramerate,
        priority,
      };
    } else {
      throw new TypeError('Unsupported options: provide at least width, height and maxBitrate');
    }
  }

  get resolution(): VideoResolution {
    return {
      width: this.width,
      height: this.height,
      frameRate: this.encoding.maxFramerate,
      aspectRatio: this.aspectRatio,
    };
  }
}

export interface AudioPreset {
  maxBitrate: number;
  priority?: RTCPriorityType;
}

// `red` is not technically a codec, but treated as one in signalling protocol
export const audioCodecs = ['opus', 'red'] as const;

export type AudioCodec = (typeof audioCodecs)[number];

const backupVideoCodecs = ['vp8', 'h264'] as const;

export const videoCodecs = ['vp8', 'h264', 'vp9', 'av1', 'h265'] as const;

export type VideoCodec = (typeof videoCodecs)[number];

export type BackupVideoCodec = (typeof backupVideoCodecs)[number];

export function isBackupVideoCodec(codec: string): codec is BackupVideoCodec {
  return !!backupVideoCodecs.find((backup) => backup === codec);
}

/** @deprecated Use {@link isBackupVideoCodec} instead */
export const isBackupCodec = isBackupVideoCodec;

export enum BackupCodecPolicy {
  // codec regression is preferred, the sfu will try to regress codec if possible but not guaranteed
  PREFER_REGRESSION = 0,
  // multi-codec simulcast, publish both primary and backup codec at the same time
  SIMULCAST = 1,
  // always use backup codec only
  REGRESSION = 2,
}

/**
 * scalability modes for svc.
 */
export type ScalabilityMode =
  | 'L1T1'
  | 'L1T2'
  | 'L1T3'
  | 'L2T1'
  | 'L2T1h'
  | 'L2T1_KEY'
  | 'L2T2'
  | 'L2T2h'
  | 'L2T2_KEY'
  | 'L2T3'
  | 'L2T3h'
  | 'L2T3_KEY'
  | 'L3T1'
  | 'L3T1h'
  | 'L3T1_KEY'
  | 'L3T2'
  | 'L3T2h'
  | 'L3T2_KEY'
  | 'L3T3'
  | 'L3T3h'
  | 'L3T3_KEY';

export namespace AudioPresets {
  export const telephone: AudioPreset = {
    maxBitrate: 12_000,
  };
  export const speech: AudioPreset = {
    maxBitrate: 24_000,
  };
  export const music: AudioPreset = {
    maxBitrate: 48_000,
  };
  export const musicStereo: AudioPreset = {
    maxBitrate: 64_000,
  };
  export const musicHighQuality: AudioPreset = {
    maxBitrate: 96_000,
  };
  export const musicHighQualityStereo: AudioPreset = {
    maxBitrate: 128_000,
  };
}

/**
 * Sane presets for video resolution/encoding
 */
export const VideoPresets = {
  h90: new VideoPreset(160, 90, 90_000, 20),
  h180: new VideoPreset(320, 180, 160_000, 20),
  h216: new VideoPreset(384, 216, 180_000, 20),
  h360: new VideoPreset(640, 360, 450_000, 20),
  h540: new VideoPreset(960, 540, 800_000, 25),
  h720: new VideoPreset(1280, 720, 1_700_000, 30),
  h1080: new VideoPreset(1920, 1080, 3_000_000, 30),
  h1440: new VideoPreset(2560, 1440, 5_000_000, 30),
  h2160: new VideoPreset(3840, 2160, 8_000_000, 30),
} as const;

/**
 * Four by three presets
 */
export const VideoPresets43 = {
  h120: new VideoPreset(160, 120, 70_000, 20),
  h180: new VideoPreset(240, 180, 125_000, 20),
  h240: new VideoPreset(320, 240, 140_000, 20),
  h360: new VideoPreset(480, 360, 330_000, 20),
  h480: new VideoPreset(640, 480, 500_000, 20),
  h540: new VideoPreset(720, 540, 600_000, 25),
  h720: new VideoPreset(960, 720, 1_300_000, 30),
  h1080: new VideoPreset(1440, 1080, 2_300_000, 30),
  h1440: new VideoPreset(1920, 1440, 3_800_000, 30),
} as const;

export const ScreenSharePresets = {
  h360fps3: new VideoPreset(640, 360, 200_000, 3, 'medium'),
  h360fps15: new VideoPreset(640, 360, 400_000, 15, 'medium'),
  h720fps5: new VideoPreset(1280, 720, 800_000, 5, 'medium'),
  h720fps15: new VideoPreset(1280, 720, 1_500_000, 15, 'medium'),
  h720fps30: new VideoPreset(1280, 720, 2_000_000, 30, 'medium'),
  h1080fps15: new VideoPreset(1920, 1080, 2_500_000, 15, 'medium'),
  h1080fps30: new VideoPreset(1920, 1080, 5_000_000, 30, 'medium'),
  // original resolution, without resizing
  original: new VideoPreset(0, 0, 7_000_000, 30, 'medium'),
} as const;
