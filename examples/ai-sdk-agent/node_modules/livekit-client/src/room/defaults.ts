import type { InternalRoomConnectOptions, InternalRoomOptions } from '../options';
import DefaultReconnectPolicy from './DefaultReconnectPolicy';
import type {
  AudioCaptureOptions,
  TrackPublishDefaults,
  VideoCaptureOptions,
} from './track/options';
import { AudioPresets, ScreenSharePresets, VideoPresets } from './track/options';

export const defaultVideoCodec = 'vp8';

export const publishDefaults: TrackPublishDefaults = {
  audioPreset: AudioPresets.music,
  dtx: true,
  red: true,
  forceStereo: false,
  simulcast: true,
  screenShareEncoding: ScreenSharePresets.h1080fps15.encoding,
  stopMicTrackOnMute: false,
  videoCodec: defaultVideoCodec,
  backupCodec: true,
  preConnectBuffer: false,
} as const;

export const audioDefaults: AudioCaptureOptions = {
  deviceId: { ideal: 'default' },
  autoGainControl: true,
  echoCancellation: true,
  noiseSuppression: true,
  voiceIsolation: true,
};

export const videoDefaults: VideoCaptureOptions = {
  deviceId: { ideal: 'default' },
  resolution: VideoPresets.h720.resolution,
};

export const roomOptionDefaults: InternalRoomOptions = {
  adaptiveStream: false,
  dynacast: false,
  stopLocalTrackOnUnpublish: true,
  reconnectPolicy: new DefaultReconnectPolicy(),
  disconnectOnPageLeave: true,
  webAudioMix: false,
  singlePeerConnection: false,
} as const;

export const roomConnectOptionDefaults: InternalRoomConnectOptions = {
  autoSubscribe: true,
  maxRetries: 1,
  peerConnectionTimeout: 15_000,
  websocketTimeout: 15_000,
} as const;
