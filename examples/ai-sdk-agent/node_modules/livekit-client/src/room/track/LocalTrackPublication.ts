import { AudioTrackFeature, TrackInfo } from '@livekit/protocol';
import { TrackEvent } from '../events';
import type { LoggerOptions } from '../types';
import { isAudioTrack, isVideoTrack } from '../utils';
import LocalAudioTrack from './LocalAudioTrack';
import type LocalTrack from './LocalTrack';
import type LocalVideoTrack from './LocalVideoTrack';
import type { Track } from './Track';
import { TrackPublication } from './TrackPublication';
import type { TrackPublishOptions } from './options';

export default class LocalTrackPublication extends TrackPublication {
  track?: LocalTrack = undefined;

  options?: TrackPublishOptions;

  get isUpstreamPaused() {
    return this.track?.isUpstreamPaused;
  }

  constructor(kind: Track.Kind, ti: TrackInfo, track?: LocalTrack, loggerOptions?: LoggerOptions) {
    super(kind, ti.sid, ti.name, loggerOptions);

    this.updateInfo(ti);
    this.setTrack(track);
  }

  setTrack(track?: Track) {
    if (this.track) {
      this.track.off(TrackEvent.Ended, this.handleTrackEnded);
      this.track.off(TrackEvent.CpuConstrained, this.handleCpuConstrained);
    }

    super.setTrack(track);

    if (track) {
      track.on(TrackEvent.Ended, this.handleTrackEnded);
      track.on(TrackEvent.CpuConstrained, this.handleCpuConstrained);
    }
  }

  get isMuted(): boolean {
    if (this.track) {
      return this.track.isMuted;
    }
    return super.isMuted;
  }

  get audioTrack(): LocalAudioTrack | undefined {
    return super.audioTrack as LocalAudioTrack | undefined;
  }

  get videoTrack(): LocalVideoTrack | undefined {
    return super.videoTrack as LocalVideoTrack | undefined;
  }

  get isLocal() {
    return true;
  }

  /**
   * Mute the track associated with this publication
   */
  async mute() {
    return this.track?.mute();
  }

  /**
   * Unmute track associated with this publication
   */
  async unmute() {
    return this.track?.unmute();
  }

  /**
   * Pauses the media stream track associated with this publication from being sent to the server
   * and signals "muted" event to other participants
   * Useful if you want to pause the stream without pausing the local media stream track
   */
  async pauseUpstream() {
    await this.track?.pauseUpstream();
  }

  /**
   * Resumes sending the media stream track associated with this publication to the server after a call to [[pauseUpstream()]]
   * and signals "unmuted" event to other participants (unless the track is explicitly muted)
   */
  async resumeUpstream() {
    await this.track?.resumeUpstream();
  }

  getTrackFeatures() {
    if (isAudioTrack(this.track)) {
      const settings = this.track!.getSourceTrackSettings();
      const features: Set<AudioTrackFeature> = new Set();
      if (settings.autoGainControl) {
        features.add(AudioTrackFeature.TF_AUTO_GAIN_CONTROL);
      }
      if (settings.echoCancellation) {
        features.add(AudioTrackFeature.TF_ECHO_CANCELLATION);
      }
      if (settings.noiseSuppression) {
        features.add(AudioTrackFeature.TF_NOISE_SUPPRESSION);
      }
      if (settings.channelCount && settings.channelCount > 1) {
        features.add(AudioTrackFeature.TF_STEREO);
      }
      if (!this.options?.dtx) {
        features.add(AudioTrackFeature.TF_NO_DTX);
      }
      if (this.track.enhancedNoiseCancellation) {
        features.add(AudioTrackFeature.TF_ENHANCED_NOISE_CANCELLATION);
      }
      return Array.from(features.values());
    } else return [];
  }

  handleTrackEnded = () => {
    this.emit(TrackEvent.Ended);
  };

  private handleCpuConstrained = () => {
    if (this.track && isVideoTrack(this.track)) {
      this.emit(TrackEvent.CpuConstrained, this.track);
    }
  };
}
