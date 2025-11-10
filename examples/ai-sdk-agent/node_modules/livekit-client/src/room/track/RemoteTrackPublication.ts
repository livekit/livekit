import {
  ParticipantTracks,
  SubscriptionError,
  TrackInfo,
  UpdateSubscription,
  UpdateTrackSettings,
} from '@livekit/protocol';
import { TrackEvent } from '../events';
import type { LoggerOptions } from '../types';
import { isRemoteVideoTrack } from '../utils';
import type RemoteTrack from './RemoteTrack';
import { Track, VideoQuality } from './Track';
import { TrackPublication } from './TrackPublication';
import { areDimensionsSmaller, layerDimensionsFor } from './utils';

export default class RemoteTrackPublication extends TrackPublication {
  track?: RemoteTrack = undefined;

  /** @internal */
  protected allowed = true;

  // keeps track of client's desire to subscribe to a track, also true if autoSubscribe is active
  protected subscribed?: boolean;

  protected requestedDisabled: boolean | undefined = undefined;

  protected visible: boolean = true;

  protected videoDimensionsAdaptiveStream?: Track.Dimensions;

  protected requestedVideoDimensions?: Track.Dimensions;

  protected requestedMaxQuality?: VideoQuality;

  protected fps?: number;

  protected subscriptionError?: SubscriptionError;

  constructor(
    kind: Track.Kind,
    ti: TrackInfo,
    autoSubscribe: boolean | undefined,
    loggerOptions?: LoggerOptions,
  ) {
    super(kind, ti.sid, ti.name, loggerOptions);
    this.subscribed = autoSubscribe;
    this.updateInfo(ti);
  }

  /**
   * Subscribe or unsubscribe to this remote track
   * @param subscribed true to subscribe to a track, false to unsubscribe
   */
  setSubscribed(subscribed: boolean) {
    const prevStatus = this.subscriptionStatus;
    const prevPermission = this.permissionStatus;
    this.subscribed = subscribed;
    // reset allowed status when desired subscription state changes
    // server will notify client via signal message if it's not allowed
    if (subscribed) {
      this.allowed = true;
    }

    const sub = new UpdateSubscription({
      trackSids: [this.trackSid],
      subscribe: this.subscribed,
      participantTracks: [
        new ParticipantTracks({
          // sending an empty participant id since TrackPublication doesn't keep it
          // this is filled in by the participant that receives this message
          participantSid: '',
          trackSids: [this.trackSid],
        }),
      ],
    });
    this.emit(TrackEvent.UpdateSubscription, sub);
    this.emitSubscriptionUpdateIfChanged(prevStatus);
    this.emitPermissionUpdateIfChanged(prevPermission);
  }

  get subscriptionStatus(): TrackPublication.SubscriptionStatus {
    if (this.subscribed === false) {
      return TrackPublication.SubscriptionStatus.Unsubscribed;
    }
    if (!super.isSubscribed) {
      return TrackPublication.SubscriptionStatus.Desired;
    }
    return TrackPublication.SubscriptionStatus.Subscribed;
  }

  get permissionStatus(): TrackPublication.PermissionStatus {
    return this.allowed
      ? TrackPublication.PermissionStatus.Allowed
      : TrackPublication.PermissionStatus.NotAllowed;
  }

  /**
   * Returns true if track is subscribed, and ready for playback
   */
  get isSubscribed(): boolean {
    if (this.subscribed === false) {
      return false;
    }
    return super.isSubscribed;
  }

  // returns client's desire to subscribe to a track, also true if autoSubscribe is enabled
  get isDesired(): boolean {
    return this.subscribed !== false;
  }

  get isEnabled(): boolean {
    return this.requestedDisabled !== undefined
      ? !this.requestedDisabled
      : this.isAdaptiveStream
        ? this.visible
        : true;
  }

  get isLocal() {
    return false;
  }

  /**
   * disable server from sending down data for this track. this is useful when
   * the participant is off screen, you may disable streaming down their video
   * to reduce bandwidth requirements
   * @param enabled
   */
  setEnabled(enabled: boolean) {
    if (!this.isManualOperationAllowed() || this.requestedDisabled === !enabled) {
      return;
    }
    this.requestedDisabled = !enabled;

    this.emitTrackUpdate();
  }

  /**
   * for tracks that support simulcasting, adjust subscribed quality
   *
   * This indicates the highest quality the client can accept. if network
   * bandwidth does not allow, server will automatically reduce quality to
   * optimize for uninterrupted video
   */
  setVideoQuality(quality: VideoQuality) {
    if (!this.isManualOperationAllowed() || this.requestedMaxQuality === quality) {
      return;
    }
    this.requestedMaxQuality = quality;
    this.requestedVideoDimensions = undefined;

    this.emitTrackUpdate();
  }

  /**
   * Explicitly set the video dimensions for this track.
   *
   * This will take precedence over adaptive stream dimensions.
   *
   * @param dimensions The video dimensions to set.
   */
  setVideoDimensions(dimensions: Track.Dimensions) {
    if (!this.isManualOperationAllowed()) {
      return;
    }
    if (
      this.requestedVideoDimensions?.width === dimensions.width &&
      this.requestedVideoDimensions?.height === dimensions.height
    ) {
      return;
    }
    if (isRemoteVideoTrack(this.track)) {
      this.requestedVideoDimensions = dimensions;
    }
    this.requestedMaxQuality = undefined;

    this.emitTrackUpdate();
  }

  setVideoFPS(fps: number) {
    if (!this.isManualOperationAllowed()) {
      return;
    }

    if (!isRemoteVideoTrack(this.track)) {
      return;
    }

    if (this.fps === fps) {
      return;
    }

    this.fps = fps;
    this.emitTrackUpdate();
  }

  get videoQuality(): VideoQuality | undefined {
    return this.requestedMaxQuality ?? VideoQuality.HIGH;
  }

  /** @internal */
  setTrack(track?: RemoteTrack) {
    const prevStatus = this.subscriptionStatus;
    const prevPermission = this.permissionStatus;
    const prevTrack = this.track;
    if (prevTrack === track) {
      return;
    }
    if (prevTrack) {
      // unregister listener
      prevTrack.off(TrackEvent.VideoDimensionsChanged, this.handleVideoDimensionsChange);
      prevTrack.off(TrackEvent.VisibilityChanged, this.handleVisibilityChange);
      prevTrack.off(TrackEvent.Ended, this.handleEnded);
      prevTrack.detach();
      prevTrack.stopMonitor();
      this.emit(TrackEvent.Unsubscribed, prevTrack);
    }
    super.setTrack(track);
    if (track) {
      track.sid = this.trackSid;
      track.on(TrackEvent.VideoDimensionsChanged, this.handleVideoDimensionsChange);
      track.on(TrackEvent.VisibilityChanged, this.handleVisibilityChange);
      track.on(TrackEvent.Ended, this.handleEnded);
      this.emit(TrackEvent.Subscribed, track);
    }
    this.emitPermissionUpdateIfChanged(prevPermission);
    this.emitSubscriptionUpdateIfChanged(prevStatus);
  }

  /** @internal */
  setAllowed(allowed: boolean) {
    const prevStatus = this.subscriptionStatus;
    const prevPermission = this.permissionStatus;
    this.allowed = allowed;
    this.emitPermissionUpdateIfChanged(prevPermission);
    this.emitSubscriptionUpdateIfChanged(prevStatus);
  }

  /** @internal */
  setSubscriptionError(error: SubscriptionError) {
    this.emit(TrackEvent.SubscriptionFailed, error);
  }

  /** @internal */
  updateInfo(info: TrackInfo) {
    super.updateInfo(info);
    const prevMetadataMuted = this.metadataMuted;
    this.metadataMuted = info.muted;
    if (this.track) {
      this.track.setMuted(info.muted);
    } else if (prevMetadataMuted !== info.muted) {
      this.emit(info.muted ? TrackEvent.Muted : TrackEvent.Unmuted);
    }
  }

  private emitSubscriptionUpdateIfChanged(previousStatus: TrackPublication.SubscriptionStatus) {
    const currentStatus = this.subscriptionStatus;
    if (previousStatus === currentStatus) {
      return;
    }
    this.emit(TrackEvent.SubscriptionStatusChanged, currentStatus, previousStatus);
  }

  private emitPermissionUpdateIfChanged(
    previousPermissionStatus: TrackPublication.PermissionStatus,
  ) {
    const currentPermissionStatus = this.permissionStatus;
    if (currentPermissionStatus !== previousPermissionStatus) {
      this.emit(
        TrackEvent.SubscriptionPermissionChanged,
        this.permissionStatus,
        previousPermissionStatus,
      );
    }
  }

  private isManualOperationAllowed(): boolean {
    if (!this.isDesired) {
      this.log.warn('cannot update track settings when not subscribed', this.logContext);
      return false;
    }
    return true;
  }

  protected handleEnded = (track: RemoteTrack) => {
    this.setTrack(undefined);
    this.emit(TrackEvent.Ended, track);
  };

  protected get isAdaptiveStream(): boolean {
    return isRemoteVideoTrack(this.track) && this.track.isAdaptiveStream;
  }

  protected handleVisibilityChange = (visible: boolean) => {
    this.log.debug(
      `adaptivestream video visibility ${this.trackSid}, visible=${visible}`,
      this.logContext,
    );
    this.visible = visible;
    this.emitTrackUpdate();
  };

  protected handleVideoDimensionsChange = (dimensions: Track.Dimensions) => {
    this.log.debug(
      `adaptivestream video dimensions ${dimensions.width}x${dimensions.height}`,
      this.logContext,
    );
    this.videoDimensionsAdaptiveStream = dimensions;
    this.emitTrackUpdate();
  };

  /* @internal */
  emitTrackUpdate() {
    const settings: UpdateTrackSettings = new UpdateTrackSettings({
      trackSids: [this.trackSid],
      disabled: !this.isEnabled,
      fps: this.fps,
    });

    if (this.kind === Track.Kind.Video) {
      let minDimensions = this.requestedVideoDimensions;

      if (this.videoDimensionsAdaptiveStream !== undefined) {
        if (minDimensions) {
          // check whether the adaptive stream dimensions are smaller than the requested dimensions and use smaller one
          const smallerAdaptive = areDimensionsSmaller(
            this.videoDimensionsAdaptiveStream,
            minDimensions,
          );
          if (smallerAdaptive) {
            this.log.debug('using adaptive stream dimensions instead of requested', {
              ...this.logContext,
              ...this.videoDimensionsAdaptiveStream,
            });
            minDimensions = this.videoDimensionsAdaptiveStream;
          }
        } else if (this.requestedMaxQuality !== undefined && this.trackInfo) {
          // check whether adaptive stream dimensions are smaller than the max quality layer and use smaller one
          const maxQualityLayer = layerDimensionsFor(this.trackInfo, this.requestedMaxQuality);

          if (
            maxQualityLayer &&
            areDimensionsSmaller(this.videoDimensionsAdaptiveStream, maxQualityLayer)
          ) {
            this.log.debug('using adaptive stream dimensions instead of max quality layer', {
              ...this.logContext,
              ...this.videoDimensionsAdaptiveStream,
            });
            minDimensions = this.videoDimensionsAdaptiveStream;
          }
        } else {
          this.log.debug('using adaptive stream dimensions', {
            ...this.logContext,
            ...this.videoDimensionsAdaptiveStream,
          });
          minDimensions = this.videoDimensionsAdaptiveStream;
        }
      }

      if (minDimensions) {
        settings.width = Math.ceil(minDimensions.width);
        settings.height = Math.ceil(minDimensions.height);
      } else if (this.requestedMaxQuality !== undefined) {
        this.log.debug('using requested max quality', {
          ...this.logContext,
          quality: this.requestedMaxQuality,
        });
        settings.quality = this.requestedMaxQuality;
      } else {
        this.log.debug('using default quality', {
          ...this.logContext,
          quality: VideoQuality.HIGH,
        });
        // defaults to high quality
        settings.quality = VideoQuality.HIGH;
      }
    }

    this.emit(TrackEvent.UpdateSettings, settings);
  }
}
