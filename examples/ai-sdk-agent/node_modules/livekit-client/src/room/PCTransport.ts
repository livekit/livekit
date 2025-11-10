import { Mutex } from '@livekit/mutex';
import { EventEmitter } from 'events';
import type { MediaDescription, SessionDescription } from 'sdp-transform';
import { parse, write } from 'sdp-transform';
import { debounce } from 'ts-debounce';
import log, { LoggerNames, getLogger } from '../logger';
import { NegotiationError, UnexpectedConnectionState } from './errors';
import type { LoggerOptions } from './types';
import { ddExtensionURI, isSVCCodec, isSafari } from './utils';

/** @internal */
interface TrackBitrateInfo {
  cid?: string;
  transceiver?: RTCRtpTransceiver;
  codec: string;
  maxbr: number;
}

/* The svc codec (av1/vp9) would use a very low bitrate at the begining and
increase slowly by the bandwidth estimator until it reach the target bitrate. The
process commonly cost more than 10 seconds cause subscriber will get blur video at
the first few seconds. So we use a 70% of target bitrate here as the start bitrate to
eliminate this issue.
*/
const startBitrateForSVC = 0.7;

const debounceInterval = 20;

export const PCEvents = {
  NegotiationStarted: 'negotiationStarted',
  NegotiationComplete: 'negotiationComplete',
  RTPVideoPayloadTypes: 'rtpVideoPayloadTypes',
} as const;

/** @internal */
export default class PCTransport extends EventEmitter {
  private _pc: RTCPeerConnection | null;

  private get pc() {
    if (!this._pc) {
      this._pc = this.createPC();
    }
    return this._pc;
  }

  private config?: RTCConfiguration;

  private log = log;

  private loggerOptions: LoggerOptions;

  private ddExtID = 0;

  private latestOfferId: number = 0;

  private offerLock: Mutex;

  pendingCandidates: RTCIceCandidateInit[] = [];

  restartingIce: boolean = false;

  renegotiate: boolean = false;

  trackBitrates: TrackBitrateInfo[] = [];

  remoteStereoMids: string[] = [];

  remoteNackMids: string[] = [];

  onOffer?: (offer: RTCSessionDescriptionInit, offerId: number) => void;

  onIceCandidate?: (candidate: RTCIceCandidate) => void;

  onIceCandidateError?: (ev: Event) => void;

  onConnectionStateChange?: (state: RTCPeerConnectionState) => void;

  onIceConnectionStateChange?: (state: RTCIceConnectionState) => void;

  onSignalingStatechange?: (state: RTCSignalingState) => void;

  onDataChannel?: (ev: RTCDataChannelEvent) => void;

  onTrack?: (ev: RTCTrackEvent) => void;

  constructor(config?: RTCConfiguration, loggerOptions: LoggerOptions = {}) {
    super();
    this.log = getLogger(loggerOptions.loggerName ?? LoggerNames.PCTransport);
    this.loggerOptions = loggerOptions;
    this.config = config;
    this._pc = this.createPC();
    this.offerLock = new Mutex();
  }

  private createPC() {
    const pc = new RTCPeerConnection(this.config);

    pc.onicecandidate = (ev) => {
      if (!ev.candidate) return;
      this.onIceCandidate?.(ev.candidate);
    };
    pc.onicecandidateerror = (ev) => {
      this.onIceCandidateError?.(ev);
    };

    pc.oniceconnectionstatechange = () => {
      this.onIceConnectionStateChange?.(pc.iceConnectionState);
    };

    pc.onsignalingstatechange = () => {
      this.onSignalingStatechange?.(pc.signalingState);
    };

    pc.onconnectionstatechange = () => {
      this.onConnectionStateChange?.(pc.connectionState);
    };
    pc.ondatachannel = (ev) => {
      this.onDataChannel?.(ev);
    };
    pc.ontrack = (ev) => {
      this.onTrack?.(ev);
    };
    return pc;
  }

  private get logContext() {
    return {
      ...this.loggerOptions.loggerContextCb?.(),
    };
  }

  get isICEConnected(): boolean {
    return (
      this._pc !== null &&
      (this.pc.iceConnectionState === 'connected' || this.pc.iceConnectionState === 'completed')
    );
  }

  async addIceCandidate(candidate: RTCIceCandidateInit): Promise<void> {
    if (this.pc.remoteDescription && !this.restartingIce) {
      return this.pc.addIceCandidate(candidate);
    }
    this.pendingCandidates.push(candidate);
  }

  async setRemoteDescription(sd: RTCSessionDescriptionInit, offerId: number): Promise<boolean> {
    if (
      sd.type === 'answer' &&
      this.latestOfferId > 0 &&
      offerId > 0 &&
      offerId !== this.latestOfferId
    ) {
      this.log.warn('ignoring answer for old offer', {
        ...this.logContext,
        offerId,
        latestOfferId: this.latestOfferId,
      });
      return false;
    }
    let mungedSDP: string | undefined = undefined;
    if (sd.type === 'offer') {
      let { stereoMids, nackMids } = extractStereoAndNackAudioFromOffer(sd);
      this.remoteStereoMids = stereoMids;
      this.remoteNackMids = nackMids;
    } else if (sd.type === 'answer') {
      const sdpParsed = parse(sd.sdp ?? '');
      sdpParsed.media.forEach((media) => {
        const mid = getMidString(media.mid!);
        if (media.type === 'audio') {
          // munge sdp for opus bitrate settings
          this.trackBitrates.some((trackbr): boolean => {
            if (!trackbr.transceiver || mid != trackbr.transceiver.mid) {
              return false;
            }

            let codecPayload = 0;
            media.rtp.some((rtp): boolean => {
              if (rtp.codec.toUpperCase() === trackbr.codec.toUpperCase()) {
                codecPayload = rtp.payload;
                return true;
              }
              return false;
            });

            if (codecPayload === 0) {
              return true;
            }

            let fmtpFound = false;
            for (const fmtp of media.fmtp) {
              if (fmtp.payload === codecPayload) {
                fmtp.config = fmtp.config
                  .split(';')
                  .filter((attr) => !attr.includes('maxaveragebitrate'))
                  .join(';');
                if (trackbr.maxbr > 0) {
                  fmtp.config += `;maxaveragebitrate=${trackbr.maxbr * 1000}`;
                }
                fmtpFound = true;
                break;
              }
            }

            if (!fmtpFound) {
              if (trackbr.maxbr > 0) {
                media.fmtp.push({
                  payload: codecPayload,
                  config: `maxaveragebitrate=${trackbr.maxbr * 1000}`,
                });
              }
            }

            return true;
          });
        }
      });
      mungedSDP = write(sdpParsed);
    }
    await this.setMungedSDP(sd, mungedSDP, true);

    this.pendingCandidates.forEach((candidate) => {
      this.pc.addIceCandidate(candidate);
    });
    this.pendingCandidates = [];
    this.restartingIce = false;

    if (this.renegotiate) {
      this.renegotiate = false;
      await this.createAndSendOffer();
    } else if (sd.type === 'answer') {
      this.emit(PCEvents.NegotiationComplete);
      if (sd.sdp) {
        const sdpParsed = parse(sd.sdp);
        sdpParsed.media.forEach((media) => {
          if (media.type === 'video') {
            this.emit(PCEvents.RTPVideoPayloadTypes, media.rtp);
          }
        });
      }
    }
    return true;
  }

  // debounced negotiate interface
  negotiate = debounce(async (onError?: (e: Error) => void) => {
    this.emit(PCEvents.NegotiationStarted);
    try {
      await this.createAndSendOffer();
    } catch (e) {
      if (onError) {
        onError(e as Error);
      } else {
        throw e;
      }
    }
  }, debounceInterval);

  async createAndSendOffer(options?: RTCOfferOptions) {
    const unlock = await this.offerLock.lock();

    try {
      if (this.onOffer === undefined) {
        return;
      }

      if (options?.iceRestart) {
        this.log.debug('restarting ICE', this.logContext);
        this.restartingIce = true;
      }

      if (this._pc && this._pc.signalingState === 'have-local-offer') {
        // we're waiting for the peer to accept our offer, so we'll just wait
        // the only exception to this is when ICE restart is needed
        const currentSD = this._pc.remoteDescription;
        if (options?.iceRestart && currentSD) {
          // TODO: handle when ICE restart is needed but we don't have a remote description
          // the best thing to do is to recreate the peerconnection
          await this._pc.setRemoteDescription(currentSD);
        } else {
          this.renegotiate = true;
          return;
        }
      } else if (!this._pc || this._pc.signalingState === 'closed') {
        this.log.warn('could not createOffer with closed peer connection', this.logContext);
        return;
      }

      // actually negotiate
      this.log.debug('starting to negotiate', this.logContext);
      // increase the offer id at the start to ensure the offer is always > 0 so that we can use 0 as a default value for legacy behavior
      const offerId = this.latestOfferId + 1;
      this.latestOfferId = offerId;
      const offer = await this.pc.createOffer(options);
      this.log.debug('original offer', { sdp: offer.sdp, ...this.logContext });

      const sdpParsed = parse(offer.sdp ?? '');
      sdpParsed.media.forEach((media) => {
        ensureIPAddrMatchVersion(media);
        if (media.type === 'audio') {
          ensureAudioNackAndStereo(media, ['all'], []);
        } else if (media.type === 'video') {
          this.trackBitrates.some((trackbr): boolean => {
            if (!media.msid || !trackbr.cid || !media.msid.includes(trackbr.cid)) {
              return false;
            }

            let codecPayload = 0;
            media.rtp.some((rtp): boolean => {
              if (rtp.codec.toUpperCase() === trackbr.codec.toUpperCase()) {
                codecPayload = rtp.payload;
                return true;
              }
              return false;
            });

            if (codecPayload === 0) {
              return true;
            }

            if (isSVCCodec(trackbr.codec) && !isSafari()) {
              this.ensureVideoDDExtensionForSVC(media, sdpParsed);
            }

            // TODO: av1 slow starting issue already fixed in chrome 124, clean this after some versions
            // mung sdp for av1 bitrate setting that can't apply by sendEncoding
            if (trackbr.codec !== 'av1') {
              return true;
            }

            const startBitrate = Math.round(trackbr.maxbr * startBitrateForSVC);

            for (const fmtp of media.fmtp) {
              if (fmtp.payload === codecPayload) {
                // if another track's fmtp already is set, we cannot override the bitrate
                // this has the unfortunate consequence of being forced to use the
                // initial track's bitrate for all tracks
                if (!fmtp.config.includes('x-google-start-bitrate')) {
                  fmtp.config += `;x-google-start-bitrate=${startBitrate}`;
                }
                break;
              }
            }
            return true;
          });
        }
      });
      if (this.latestOfferId > offerId) {
        this.log.warn('latestOfferId mismatch', {
          ...this.logContext,
          latestOfferId: this.latestOfferId,
          offerId,
        });
        return;
      }
      await this.setMungedSDP(offer, write(sdpParsed));
      this.onOffer(offer, this.latestOfferId);
    } finally {
      unlock();
    }
  }

  async createAndSetAnswer(): Promise<RTCSessionDescriptionInit> {
    const answer = await this.pc.createAnswer();
    const sdpParsed = parse(answer.sdp ?? '');
    sdpParsed.media.forEach((media) => {
      ensureIPAddrMatchVersion(media);
      if (media.type === 'audio') {
        ensureAudioNackAndStereo(media, this.remoteStereoMids, this.remoteNackMids);
      }
    });
    await this.setMungedSDP(answer, write(sdpParsed));
    return answer;
  }

  createDataChannel(label: string, dataChannelDict: RTCDataChannelInit) {
    return this.pc.createDataChannel(label, dataChannelDict);
  }

  addTransceiver(mediaStreamTrack: MediaStreamTrack, transceiverInit: RTCRtpTransceiverInit) {
    return this.pc.addTransceiver(mediaStreamTrack, transceiverInit);
  }

  addTransceiverOfKind(kind: 'audio' | 'video', transceiverInit: RTCRtpTransceiverInit) {
    return this.pc.addTransceiver(kind, transceiverInit);
  }

  addTrack(track: MediaStreamTrack) {
    if (!this._pc) {
      throw new UnexpectedConnectionState('PC closed, cannot add track');
    }
    return this._pc.addTrack(track);
  }

  setTrackCodecBitrate(info: TrackBitrateInfo) {
    this.trackBitrates.push(info);
  }

  setConfiguration(rtcConfig: RTCConfiguration) {
    if (!this._pc) {
      throw new UnexpectedConnectionState('PC closed, cannot configure');
    }
    return this._pc?.setConfiguration(rtcConfig);
  }

  canRemoveTrack(): boolean {
    return !!this._pc?.removeTrack;
  }

  removeTrack(sender: RTCRtpSender) {
    return this._pc?.removeTrack(sender);
  }

  getConnectionState() {
    return this._pc?.connectionState ?? 'closed';
  }

  getICEConnectionState() {
    return this._pc?.iceConnectionState ?? 'closed';
  }

  getSignallingState() {
    return this._pc?.signalingState ?? 'closed';
  }

  getTransceivers() {
    return this._pc?.getTransceivers() ?? [];
  }

  getSenders() {
    return this._pc?.getSenders() ?? [];
  }

  getLocalDescription() {
    return this._pc?.localDescription;
  }

  getRemoteDescription() {
    return this.pc?.remoteDescription;
  }

  getStats() {
    return this.pc.getStats();
  }

  async getConnectedAddress(): Promise<string | undefined> {
    if (!this._pc) {
      return;
    }
    let selectedCandidatePairId = '';
    const candidatePairs = new Map<string, RTCIceCandidatePairStats>();
    // id -> candidate ip
    const candidates = new Map<string, string>();
    const stats: RTCStatsReport = await this._pc.getStats();
    stats.forEach((v) => {
      switch (v.type) {
        case 'transport':
          selectedCandidatePairId = v.selectedCandidatePairId;
          break;
        case 'candidate-pair':
          if (selectedCandidatePairId === '' && v.selected) {
            selectedCandidatePairId = v.id;
          }
          candidatePairs.set(v.id, v);
          break;
        case 'remote-candidate':
          candidates.set(v.id, `${v.address}:${v.port}`);
          break;
        default:
      }
    });

    if (selectedCandidatePairId === '') {
      return undefined;
    }
    const selectedID = candidatePairs.get(selectedCandidatePairId)?.remoteCandidateId;
    if (selectedID === undefined) {
      return undefined;
    }
    return candidates.get(selectedID);
  }

  close = () => {
    if (!this._pc) {
      return;
    }
    this._pc.close();
    this._pc.onconnectionstatechange = null;
    this._pc.oniceconnectionstatechange = null;
    this._pc.onicegatheringstatechange = null;
    this._pc.ondatachannel = null;
    this._pc.onnegotiationneeded = null;
    this._pc.onsignalingstatechange = null;
    this._pc.onicecandidate = null;
    this._pc.ondatachannel = null;
    this._pc.ontrack = null;
    this._pc.onconnectionstatechange = null;
    this._pc.oniceconnectionstatechange = null;
    this._pc = null;
  };

  private async setMungedSDP(sd: RTCSessionDescriptionInit, munged?: string, remote?: boolean) {
    if (munged) {
      const originalSdp = sd.sdp;
      sd.sdp = munged;
      try {
        this.log.debug(
          `setting munged ${remote ? 'remote' : 'local'} description`,
          this.logContext,
        );
        if (remote) {
          await this.pc.setRemoteDescription(sd);
        } else {
          await this.pc.setLocalDescription(sd);
        }
        return;
      } catch (e) {
        this.log.warn(`not able to set ${sd.type}, falling back to unmodified sdp`, {
          ...this.logContext,
          error: e,
          sdp: munged,
        });
        sd.sdp = originalSdp;
      }
    }

    try {
      if (remote) {
        await this.pc.setRemoteDescription(sd);
      } else {
        await this.pc.setLocalDescription(sd);
      }
    } catch (e) {
      let msg = 'unknown error';
      if (e instanceof Error) {
        msg = e.message;
      } else if (typeof e === 'string') {
        msg = e;
      }

      const fields: any = {
        error: msg,
        sdp: sd.sdp,
      };
      if (!remote && this.pc.remoteDescription) {
        fields.remoteSdp = this.pc.remoteDescription;
      }
      this.log.error(`unable to set ${sd.type}`, { ...this.logContext, fields });
      throw new NegotiationError(msg);
    }
  }

  private ensureVideoDDExtensionForSVC(
    media: {
      type: string;
      port: number;
      protocol: string;
      payloads?: string | undefined;
    } & MediaDescription,
    sdp: SessionDescription,
  ) {
    const ddFound = media.ext?.some((ext): boolean => {
      if (ext.uri === ddExtensionURI) {
        return true;
      }
      return false;
    });

    if (!ddFound) {
      if (this.ddExtID === 0) {
        let maxID = 0;
        sdp.media.forEach((m) => {
          if (m.type !== 'video') {
            return;
          }
          m.ext?.forEach((ext) => {
            if (ext.value > maxID) {
              maxID = ext.value;
            }
          });
        });
        this.ddExtID = maxID + 1;
      }
      media.ext?.push({
        value: this.ddExtID,
        uri: ddExtensionURI,
      });
    }
  }
}

function ensureAudioNackAndStereo(
  media: {
    type: string;
    port: number;
    protocol: string;
    payloads?: string | undefined;
  } & MediaDescription,
  stereoMids: string[],
  nackMids: string[],
) {
  // sdp-transform types don't include number however the parser outputs mids as numbers in some cases
  const mid = getMidString(media.mid!);

  // found opus codec to add nack fb
  let opusPayload = 0;
  media.rtp.some((rtp): boolean => {
    if (rtp.codec === 'opus') {
      opusPayload = rtp.payload;
      return true;
    }
    return false;
  });

  // add nack rtcpfb if not exist
  if (opusPayload > 0) {
    if (!media.rtcpFb) {
      media.rtcpFb = [];
    }

    if (
      nackMids.includes(mid) &&
      !media.rtcpFb.some((fb) => fb.payload === opusPayload && fb.type === 'nack')
    ) {
      media.rtcpFb.push({
        payload: opusPayload,
        type: 'nack',
      });
    }

    if (stereoMids.includes(mid) || (stereoMids.length === 1 && stereoMids[0] === 'all')) {
      media.fmtp.some((fmtp): boolean => {
        if (fmtp.payload === opusPayload) {
          if (!fmtp.config.includes('stereo=1')) {
            fmtp.config += ';stereo=1';
          }
          return true;
        }
        return false;
      });
    }
  }
}

function extractStereoAndNackAudioFromOffer(offer: RTCSessionDescriptionInit): {
  stereoMids: string[];
  nackMids: string[];
} {
  const stereoMids: string[] = [];
  const nackMids: string[] = [];
  const sdpParsed = parse(offer.sdp ?? '');
  let opusPayload = 0;
  sdpParsed.media.forEach((media) => {
    const mid = getMidString(media.mid!);
    if (media.type === 'audio') {
      media.rtp.some((rtp): boolean => {
        if (rtp.codec === 'opus') {
          opusPayload = rtp.payload;
          return true;
        }
        return false;
      });

      if (media.rtcpFb?.some((fb) => fb.payload === opusPayload && fb.type === 'nack')) {
        nackMids.push(mid);
      }

      media.fmtp.some((fmtp): boolean => {
        if (fmtp.payload === opusPayload) {
          if (fmtp.config.includes('sprop-stereo=1')) {
            stereoMids.push(mid);
          }
          return true;
        }
        return false;
      });
    }
  });
  return { stereoMids, nackMids };
}

function ensureIPAddrMatchVersion(media: MediaDescription) {
  // Chrome could generate sdp with c = IN IP4 <ipv6 addr>
  // in edge case and return error when set sdp.This is not a
  // sdk error but correct it if the issue detected.
  if (media.connection) {
    const isV6 = media.connection.ip.indexOf(':') >= 0;
    if ((media.connection.version === 4 && isV6) || (media.connection.version === 6 && !isV6)) {
      // fallback to dummy address
      media.connection.ip = '0.0.0.0';
      media.connection.version = 4;
    }
  }
}

function getMidString(mid: string | number) {
  return typeof mid === 'number' ? mid.toFixed(0) : mid;
}
