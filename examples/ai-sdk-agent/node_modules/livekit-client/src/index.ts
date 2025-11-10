import { Mutex } from '@livekit/mutex';
import {
  DataPacket_Kind,
  DisconnectReason,
  Encryption_Type,
  SubscriptionError,
  TrackType,
} from '@livekit/protocol';
import { LogLevel, LoggerNames, getLogger, setLogExtension, setLogLevel } from './logger';
import DefaultReconnectPolicy from './room/DefaultReconnectPolicy';
import type { ReconnectContext, ReconnectPolicy } from './room/ReconnectPolicy';
import Room, { ConnectionState } from './room/Room';
import * as attributes from './room/attribute-typings';
import LocalParticipant from './room/participant/LocalParticipant';
import Participant, { ConnectionQuality, ParticipantKind } from './room/participant/Participant';
import type { ParticipantTrackPermission } from './room/participant/ParticipantTrackPermission';
import RemoteParticipant from './room/participant/RemoteParticipant';
import type {
  AudioReceiverStats,
  AudioSenderStats,
  VideoReceiverStats,
  VideoSenderStats,
} from './room/stats';
import CriticalTimers from './room/timers';
import LocalAudioTrack from './room/track/LocalAudioTrack';
import LocalTrack from './room/track/LocalTrack';
import LocalTrackPublication from './room/track/LocalTrackPublication';
import LocalVideoTrack from './room/track/LocalVideoTrack';
import RemoteAudioTrack from './room/track/RemoteAudioTrack';
import RemoteTrack from './room/track/RemoteTrack';
import RemoteTrackPublication from './room/track/RemoteTrackPublication';
import type { ElementInfo } from './room/track/RemoteVideoTrack';
import RemoteVideoTrack from './room/track/RemoteVideoTrack';
import { TrackPublication } from './room/track/TrackPublication';
import type { LiveKitReactNativeInfo } from './room/types';
import type { AudioAnalyserOptions } from './room/utils';
import {
  compareVersions,
  createAudioAnalyser,
  getEmptyAudioStreamTrack,
  getEmptyVideoStreamTrack,
  isAudioCodec,
  isAudioTrack,
  isBrowserSupported,
  isLocalParticipant,
  isLocalTrack,
  isRemoteParticipant,
  isRemoteTrack,
  isVideoCodec,
  isVideoTrack,
  supportsAV1,
  supportsAdaptiveStream,
  supportsAudioOutputSelection,
  supportsDynacast,
  supportsVP9,
} from './room/utils';
import { getBrowser } from './utils/browserParser';

export { RpcError, type RpcInvocationData, type PerformRpcParams } from './room/rpc';

export * from './connectionHelper/ConnectionCheck';
export * from './connectionHelper/checks/Checker';
export * from './e2ee';
export type { BaseE2EEManager } from './e2ee/E2eeManager';
export * from './options';
export * from './room/errors';
export * from './room/events';
export * from './room/track/Track';
export * from './room/track/create';
export * from './room/token-source/TokenSource';
export * from './room/token-source/types';
export { facingModeFromDeviceLabel, facingModeFromLocalTrack } from './room/track/facingMode';
export * from './room/track/options';
export * from './room/track/processor/types';
export * from './room/track/types';
export type * from './room/data-stream/incoming/StreamReader';
export type * from './room/data-stream/outgoing/StreamWriter';
export type {
  DataPublishOptions,
  SimulationScenario,
  TranscriptionSegment,
  ChatMessage,
  SendTextOptions,
} from './room/types';
export * from './version';
export {
  /** @internal */
  attributes,
  ConnectionQuality,
  ConnectionState,
  CriticalTimers,
  DataPacket_Kind,
  Encryption_Type,
  DefaultReconnectPolicy,
  DisconnectReason,
  LocalAudioTrack,
  LocalParticipant,
  LocalTrack,
  LocalTrackPublication,
  LocalVideoTrack,
  LogLevel,
  LoggerNames,
  Participant,
  RemoteAudioTrack,
  RemoteParticipant,
  ParticipantKind,
  RemoteTrack,
  RemoteTrackPublication,
  RemoteVideoTrack,
  Room,
  SubscriptionError,
  TrackPublication,
  TrackType,
  compareVersions,
  createAudioAnalyser,
  getBrowser,
  getEmptyAudioStreamTrack,
  getEmptyVideoStreamTrack,
  getLogger,
  isBrowserSupported,
  setLogExtension,
  setLogLevel,
  supportsAV1,
  supportsAdaptiveStream,
  supportsAudioOutputSelection,
  supportsDynacast,
  supportsVP9,
  Mutex,
  isAudioCodec,
  isAudioTrack,
  isLocalTrack,
  isRemoteTrack,
  isVideoCodec,
  isVideoTrack,
  isLocalParticipant,
  isRemoteParticipant,
};
export type {
  AudioAnalyserOptions,
  ElementInfo,
  LiveKitReactNativeInfo,
  ParticipantTrackPermission,
  AudioReceiverStats,
  AudioSenderStats,
  VideoReceiverStats,
  VideoSenderStats,
  ReconnectContext,
  ReconnectPolicy,
};

export { LocalTrackRecorder } from './room/track/record';
