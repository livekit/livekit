// SPDX-FileCopyrightText: 2024 LiveKit, Inc.
//
// SPDX-License-Identifier: Apache-2.0
import type { IngressAudioOptions, IngressInput, IngressVideoOptions } from '@livekit/protocol';
import {
  CreateIngressRequest,
  DeleteIngressRequest,
  IngressInfo,
  ListIngressRequest,
  ListIngressResponse,
  UpdateIngressRequest,
} from '@livekit/protocol';
import { ServiceBase } from './ServiceBase.js';
import type { Rpc } from './TwirpRPC.js';
import { TwirpRpc, livekitPackage } from './TwirpRPC.js';

const svc = 'Ingress';

export interface CreateIngressOptions {
  /**
   * ingress name. optional
   */
  name?: string;
  /**
   * name of the room to send media to. required
   */
  roomName?: string;
  /**
   * unique identity of the participant. required
   */
  participantIdentity: string;
  /**
   * participant display name
   */
  participantName?: string;
  /**
   * metadata to attach to the participant
   */
  participantMetadata?: string;
  /**
   * @deprecated use `enableTranscoding` instead.
   * whether to skip transcoding and forward the input media directly. Only supported by WHIP
   */
  bypassTranscoding?: boolean;
  /**
   * whether to enable transcoding or forward the input media directly.
   * Transcoding is required for all input types except WHIP. For WHIP, the default is to not transcode.
   */
  enableTranscoding?: boolean | undefined;
  /**
   * url of the media to pull for ingresses of type URL
   */
  url?: string;
  /**
   * custom audio encoding parameters. optional
   */
  audio?: IngressAudioOptions;
  /**
   * custom video encoding parameters. optional
   */
  video?: IngressVideoOptions;
}

export interface UpdateIngressOptions {
  /**
   * ingress name. optional
   */
  name: string;
  /**
   * name of the room to send media to.
   */
  roomName?: string;
  /**
   * unique identity of the participant.
   */
  participantIdentity?: string;
  /**
   * participant display name
   */
  participantName?: string;
  /**
   * metadata to attach to the participant
   */
  participantMetadata?: string;
  /**
   * @deprecated use `enableTranscoding` instead
   * whether to skip transcoding and forward the input media directly. Only supported by WHIP
   */
  bypassTranscoding?: boolean | undefined;
  /**
   * whether to enable transcoding or forward the input media directly.
   * Transcoding is required for all input types except WHIP. For WHIP, the default is to not transcode.
   */
  enableTranscoding?: boolean | undefined;
  /**
   * custom audio encoding parameters. optional
   */
  audio?: IngressAudioOptions;
  /**
   * custom video encoding parameters. optional
   */
  video?: IngressVideoOptions;
}

export interface ListIngressOptions {
  /**
   * list ingress for one room only
   */
  roomName?: string;

  /**
   * list ingress by ID
   */
  ingressId?: string;
}

/**
 * Client to access Ingress APIs
 */
export class IngressClient extends ServiceBase {
  private readonly rpc: Rpc;

  /**
   * @param host - hostname including protocol. i.e. 'https://<project>.livekit.cloud'
   * @param apiKey - API Key, can be set in env var LIVEKIT_API_KEY
   * @param secret - API Secret, can be set in env var LIVEKIT_API_SECRET
   */
  constructor(host: string, apiKey?: string, secret?: string) {
    super(apiKey, secret);
    this.rpc = new TwirpRpc(host, livekitPackage);
  }

  /**
   * @param inputType - protocol for the ingress
   * @param opts - CreateIngressOptions
   */
  async createIngress(inputType: IngressInput, opts: CreateIngressOptions): Promise<IngressInfo> {
    let name: string = '';
    let participantName: string = '';
    let participantIdentity: string = '';
    let bypassTranscoding: boolean = false;
    let url: string = '';

    if (opts == null) {
      throw new Error('options dictionary is required');
    }

    const roomName: string | undefined = opts.roomName;
    const enableTranscoding: boolean | undefined = opts.enableTranscoding;
    const audio: IngressAudioOptions | undefined = opts.audio;
    const video: IngressVideoOptions | undefined = opts.video;
    const participantMetadata: string | undefined = opts.participantMetadata;

    name = opts.name || '';
    participantName = opts.participantName || '';
    participantIdentity = opts.participantIdentity || '';
    bypassTranscoding = opts.bypassTranscoding || false;
    url = opts.url || '';

    if (typeof roomName == 'undefined') {
      throw new Error('required roomName option not provided');
    }

    if (participantIdentity == '') {
      throw new Error('required participantIdentity option not provided');
    }

    const req = new CreateIngressRequest({
      inputType,
      name,
      roomName,
      participantIdentity,
      participantMetadata,
      participantName,
      bypassTranscoding,
      enableTranscoding,
      url,
      audio,
      video,
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'CreateIngress',
      req,
      await this.authHeader({ ingressAdmin: true }),
    );
    return IngressInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * @param ingressId - ID of the ingress to update
   * @param opts - UpdateIngressOptions
   */
  async updateIngress(ingressId: string, opts: UpdateIngressOptions): Promise<IngressInfo> {
    const name: string = opts.name || '';
    const roomName: string = opts.roomName || '';
    const participantName: string = opts.participantName || '';
    const participantIdentity: string = opts.participantIdentity || '';
    const { participantMetadata } = opts;
    const { audio, video, bypassTranscoding, enableTranscoding } = opts;

    const req = new UpdateIngressRequest({
      ingressId,
      name,
      roomName,
      participantIdentity,
      participantName,
      participantMetadata,
      bypassTranscoding,
      enableTranscoding,
      audio,
      video,
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'UpdateIngress',
      req,
      await this.authHeader({ ingressAdmin: true }),
    );
    return IngressInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * @deprecated use `listIngress(opts)` or `listIngress(arg)` instead
   * @param roomName - list ingress for one room only
   */
  async listIngress(roomName?: string): Promise<Array<IngressInfo>>;
  /**
   * @param opts - list options
   */
  async listIngress(opts?: ListIngressOptions): Promise<Array<IngressInfo>>;
  /**
   * @param arg - list room name or options
   */
  async listIngress(arg?: string | ListIngressOptions): Promise<Array<IngressInfo>> {
    let req: Partial<ListIngressRequest> = {};
    if (typeof arg === 'string') {
      req.roomName = arg;
    } else if (arg) {
      req = arg;
    }
    const data = await this.rpc.request(
      svc,
      'ListIngress',
      new ListIngressRequest(req).toJson(),
      await this.authHeader({ ingressAdmin: true }),
    );
    return ListIngressResponse.fromJson(data, { ignoreUnknownFields: true }).items ?? [];
  }

  /**
   * @param ingressId - ingress to delete
   */
  async deleteIngress(ingressId: string): Promise<IngressInfo> {
    const data = await this.rpc.request(
      svc,
      'DeleteIngress',
      new DeleteIngressRequest({ ingressId }).toJson(),
      await this.authHeader({ ingressAdmin: true }),
    );
    return IngressInfo.fromJson(data, { ignoreUnknownFields: true });
  }
}
