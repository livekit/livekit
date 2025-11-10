import type {
  ParticipantInfo,
  SubscriptionError,
  UpdateSubscription,
  UpdateTrackSettings,
} from '@livekit/protocol';
import type { SignalClient } from '../../api/SignalClient';
import { ParticipantEvent, TrackEvent } from '../events';
import RemoteAudioTrack from '../track/RemoteAudioTrack';
import type RemoteTrack from '../track/RemoteTrack';
import RemoteTrackPublication from '../track/RemoteTrackPublication';
import RemoteVideoTrack from '../track/RemoteVideoTrack';
import { Track } from '../track/Track';
import type { TrackPublication } from '../track/TrackPublication';
import type { AudioOutputOptions } from '../track/options';
import type { AdaptiveStreamSettings } from '../track/types';
import { getLogContextFromTrack } from '../track/utils';
import type { LoggerOptions } from '../types';
import { isAudioTrack, isRemoteTrack } from '../utils';
import Participant, { ParticipantKind } from './Participant';
import type { ParticipantEventCallbacks } from './Participant';

export default class RemoteParticipant extends Participant {
  audioTrackPublications: Map<string, RemoteTrackPublication>;

  videoTrackPublications: Map<string, RemoteTrackPublication>;

  trackPublications: Map<string, RemoteTrackPublication>;

  signalClient: SignalClient;

  private volumeMap: Map<Track.Source, number>;

  private audioOutput?: AudioOutputOptions;

  /** @internal */
  static fromParticipantInfo(
    signalClient: SignalClient,
    pi: ParticipantInfo,
    loggerOptions: LoggerOptions,
  ): RemoteParticipant {
    return new RemoteParticipant(
      signalClient,
      pi.sid,
      pi.identity,
      pi.name,
      pi.metadata,
      pi.attributes,
      loggerOptions,
      pi.kind,
    );
  }

  protected get logContext() {
    return {
      ...super.logContext,
      rpID: this.sid,
      remoteParticipant: this.identity,
    };
  }

  /** @internal */
  constructor(
    signalClient: SignalClient,
    sid: string,
    identity?: string,
    name?: string,
    metadata?: string,
    attributes?: Record<string, string>,
    loggerOptions?: LoggerOptions,
    kind: ParticipantKind = ParticipantKind.STANDARD,
  ) {
    super(sid, identity || '', name, metadata, attributes, loggerOptions, kind);
    this.signalClient = signalClient;
    this.trackPublications = new Map();
    this.audioTrackPublications = new Map();
    this.videoTrackPublications = new Map();
    this.volumeMap = new Map();
  }

  protected addTrackPublication(publication: RemoteTrackPublication) {
    super.addTrackPublication(publication);

    // register action events
    publication.on(TrackEvent.UpdateSettings, (settings: UpdateTrackSettings) => {
      this.log.debug('send update settings', {
        ...this.logContext,
        ...getLogContextFromTrack(publication),
        settings,
      });
      this.signalClient.sendUpdateTrackSettings(settings);
    });
    publication.on(TrackEvent.UpdateSubscription, (sub: UpdateSubscription) => {
      sub.participantTracks.forEach((pt) => {
        pt.participantSid = this.sid;
      });
      this.signalClient.sendUpdateSubscription(sub);
    });
    publication.on(
      TrackEvent.SubscriptionPermissionChanged,
      (status: TrackPublication.PermissionStatus) => {
        this.emit(ParticipantEvent.TrackSubscriptionPermissionChanged, publication, status);
      },
    );
    publication.on(
      TrackEvent.SubscriptionStatusChanged,
      (status: TrackPublication.SubscriptionStatus) => {
        this.emit(ParticipantEvent.TrackSubscriptionStatusChanged, publication, status);
      },
    );
    publication.on(TrackEvent.Subscribed, (track: RemoteTrack) => {
      this.emit(ParticipantEvent.TrackSubscribed, track, publication);
    });
    publication.on(TrackEvent.Unsubscribed, (previousTrack: RemoteTrack) => {
      this.emit(ParticipantEvent.TrackUnsubscribed, previousTrack, publication);
    });
    publication.on(TrackEvent.SubscriptionFailed, (error: SubscriptionError) => {
      this.emit(ParticipantEvent.TrackSubscriptionFailed, publication.trackSid, error);
    });
  }

  getTrackPublication(source: Track.Source): RemoteTrackPublication | undefined {
    const track = super.getTrackPublication(source);
    if (track) {
      return track as RemoteTrackPublication;
    }
  }

  getTrackPublicationByName(name: string): RemoteTrackPublication | undefined {
    const track = super.getTrackPublicationByName(name);
    if (track) {
      return track as RemoteTrackPublication;
    }
  }

  /**
   * sets the volume on the participant's audio track
   * by default, this affects the microphone publication
   * a different source can be passed in as a second argument
   * if no track exists the volume will be applied when the microphone track is added
   */
  setVolume(
    volume: number,
    source: Track.Source.Microphone | Track.Source.ScreenShareAudio = Track.Source.Microphone,
  ) {
    this.volumeMap.set(source, volume);
    const audioPublication = this.getTrackPublication(source);
    if (audioPublication && audioPublication.track) {
      (audioPublication.track as RemoteAudioTrack).setVolume(volume);
    }
  }

  /**
   * gets the volume on the participant's microphone track
   */
  getVolume(
    source: Track.Source.Microphone | Track.Source.ScreenShareAudio = Track.Source.Microphone,
  ) {
    const audioPublication = this.getTrackPublication(source);
    if (audioPublication && audioPublication.track) {
      return (audioPublication.track as RemoteAudioTrack).getVolume();
    }
    return this.volumeMap.get(source);
  }

  /** @internal */
  addSubscribedMediaTrack(
    mediaTrack: MediaStreamTrack,
    sid: Track.SID,
    mediaStream: MediaStream,
    receiver: RTCRtpReceiver,
    adaptiveStreamSettings?: AdaptiveStreamSettings,
    triesLeft?: number,
  ) {
    // find the track publication
    // it's possible for the media track to arrive before participant info
    let publication = this.getTrackPublicationBySid(sid);

    // it's also possible that the browser didn't honor our original track id
    // FireFox would use its own local uuid instead of server track id
    if (!publication) {
      if (!sid.startsWith('TR')) {
        // find the first track that matches type
        this.trackPublications.forEach((p) => {
          if (!publication && mediaTrack.kind === p.kind.toString()) {
            publication = p;
          }
        });
      }
    }

    // when we couldn't locate the track, it's possible that the metadata hasn't
    // yet arrived. Wait a bit longer for it to arrive, or fire an error
    if (!publication) {
      if (triesLeft === 0) {
        this.log.error('could not find published track', {
          ...this.logContext,
          trackSid: sid,
        });
        this.emit(ParticipantEvent.TrackSubscriptionFailed, sid);
        return;
      }

      if (triesLeft === undefined) triesLeft = 20;
      setTimeout(() => {
        this.addSubscribedMediaTrack(
          mediaTrack,
          sid,
          mediaStream,
          receiver,
          adaptiveStreamSettings,
          triesLeft! - 1,
        );
      }, 150);
      return;
    }

    if (mediaTrack.readyState === 'ended') {
      this.log.error(
        'unable to subscribe because MediaStreamTrack is ended. Do not call MediaStreamTrack.stop()',
        { ...this.logContext, ...getLogContextFromTrack(publication) },
      );
      this.emit(ParticipantEvent.TrackSubscriptionFailed, sid);
      return;
    }

    const isVideo = mediaTrack.kind === 'video';
    let track: RemoteTrack;
    if (isVideo) {
      track = new RemoteVideoTrack(mediaTrack, sid, receiver, adaptiveStreamSettings);
    } else {
      track = new RemoteAudioTrack(mediaTrack, sid, receiver, this.audioContext, this.audioOutput);
    }

    // set track info
    track.source = publication.source;
    // keep publication's muted status
    track.isMuted = publication.isMuted;
    track.setMediaStream(mediaStream);
    track.start();

    publication.setTrack(track);
    // set participant volumes on new audio tracks
    if (this.volumeMap.has(publication.source) && isRemoteTrack(track) && isAudioTrack(track)) {
      track.setVolume(this.volumeMap.get(publication.source)!);
    }

    return publication;
  }

  /** @internal */
  get hasMetadata(): boolean {
    return !!this.participantInfo;
  }

  /**
   * @internal
   */
  getTrackPublicationBySid(sid: Track.SID): RemoteTrackPublication | undefined {
    return this.trackPublications.get(sid);
  }

  /** @internal */
  updateInfo(info: ParticipantInfo): boolean {
    if (!super.updateInfo(info)) {
      return false;
    }

    // we are getting a list of all available tracks, reconcile in here
    // and send out events for changes

    // reconcile track publications, publish events only if metadata is already there
    // i.e. changes since the local participant has joined
    const validTracks = new Map<string, RemoteTrackPublication>();
    const newTracks = new Map<string, RemoteTrackPublication>();

    info.tracks.forEach((ti) => {
      let publication = this.getTrackPublicationBySid(ti.sid);
      if (!publication) {
        // new publication
        const kind = Track.kindFromProto(ti.type);
        if (!kind) {
          return;
        }
        publication = new RemoteTrackPublication(
          kind,
          ti,
          this.signalClient.connectOptions?.autoSubscribe,
          { loggerContextCb: () => this.logContext, loggerName: this.loggerOptions?.loggerName },
        );
        publication.updateInfo(ti);
        newTracks.set(ti.sid, publication);
        const existingTrackOfSource = Array.from(this.trackPublications.values()).find(
          (publishedTrack) => publishedTrack.source === publication?.source,
        );
        if (existingTrackOfSource && publication.source !== Track.Source.Unknown) {
          this.log.debug(
            `received a second track publication for ${this.identity} with the same source: ${publication.source}`,
            {
              ...this.logContext,
              oldTrack: getLogContextFromTrack(existingTrackOfSource),
              newTrack: getLogContextFromTrack(publication),
            },
          );
        }
        this.addTrackPublication(publication);
      } else {
        publication.updateInfo(ti);
      }
      validTracks.set(ti.sid, publication);
    });

    // detect removed tracks
    this.trackPublications.forEach((publication) => {
      if (!validTracks.has(publication.trackSid)) {
        this.log.trace('detected removed track on remote participant, unpublishing', {
          ...this.logContext,
          ...getLogContextFromTrack(publication),
        });
        this.unpublishTrack(publication.trackSid, true);
      }
    });

    // always emit events for new publications, Room will not forward them unless it's ready
    newTracks.forEach((publication) => {
      this.emit(ParticipantEvent.TrackPublished, publication);
    });
    return true;
  }

  /** @internal */
  unpublishTrack(sid: Track.SID, sendUnpublish?: boolean) {
    const publication = <RemoteTrackPublication>this.trackPublications.get(sid);
    if (!publication) {
      return;
    }

    // also send unsubscribe, if track is actively subscribed
    const { track } = publication;
    if (track) {
      track.stop();
      publication.setTrack(undefined);
    }

    // remove track from maps only after unsubscribed has been fired
    this.trackPublications.delete(sid);

    // remove from the right type map
    switch (publication.kind) {
      case Track.Kind.Audio:
        this.audioTrackPublications.delete(sid);
        break;
      case Track.Kind.Video:
        this.videoTrackPublications.delete(sid);
        break;
      default:
        break;
    }

    if (sendUnpublish) {
      this.emit(ParticipantEvent.TrackUnpublished, publication);
    }
  }

  /**
   * @internal
   */
  async setAudioOutput(output: AudioOutputOptions) {
    this.audioOutput = output;
    const promises: Promise<void>[] = [];
    this.audioTrackPublications.forEach((pub) => {
      if (isAudioTrack(pub.track) && isRemoteTrack(pub.track)) {
        promises.push(pub.track.setSinkId(output.deviceId ?? 'default'));
      }
    });
    await Promise.all(promises);
  }

  /** @internal */
  emit<E extends keyof ParticipantEventCallbacks>(
    event: E,
    ...args: Parameters<ParticipantEventCallbacks[E]>
  ): boolean {
    this.log.trace('participant event', { ...this.logContext, event, args });
    return super.emit(event, ...args);
  }
}
