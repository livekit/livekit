// SPDX-FileCopyrightText: 2024 LiveKit, Inc.
//
// SPDX-License-Identifier: Apache-2.0
import type {
  DirectFileOutput,
  EncodedFileOutput,
  EncodingOptions,
  EncodingOptionsPreset,
  ImageOutput,
  SegmentedFileOutput,
  StreamOutput,
  WebhookConfig,
} from '@livekit/protocol';
import {
  AudioMixing,
  EgressInfo,
  ListEgressRequest,
  ListEgressResponse,
  ParticipantEgressRequest,
  RoomCompositeEgressRequest,
  StopEgressRequest,
  TrackCompositeEgressRequest,
  TrackEgressRequest,
  UpdateLayoutRequest,
  UpdateStreamRequest,
  WebEgressRequest,
} from '@livekit/protocol';
import { ServiceBase } from './ServiceBase.js';
import type { Rpc } from './TwirpRPC.js';
import { TwirpRpc, livekitPackage } from './TwirpRPC.js';

const svc = 'Egress';

export interface BaseOptions {
  /**
   * webhooks to call for this request, optional.
   */
  webhooks?: WebhookConfig[];
}

export interface RoomCompositeOptions extends BaseOptions {
  /**
   * egress layout. optional
   */
  layout?: string;
  /**
   * encoding options or preset. optional
   */
  encodingOptions?: EncodingOptionsPreset | EncodingOptions;
  /**
   * record audio only. optional
   */
  audioOnly?: boolean;
  /**
   * record video only. optional
   */
  videoOnly?: boolean;
  /**
   * custom template url. optional
   */
  customBaseUrl?: string;
  /**
   * audio mixing options. optional
   */
  audioMixing?: AudioMixing;
}

export interface WebOptions extends BaseOptions {
  /**
   * encoding options or preset. optional
   */
  encodingOptions?: EncodingOptionsPreset | EncodingOptions;
  /**
   * record audio only. optional
   */
  audioOnly?: boolean;
  /**
   * record video only. optional
   */
  videoOnly?: boolean;
  /**
   * await START_RECORDING chrome log
   */
  awaitStartSignal?: boolean;
}

export interface ParticipantEgressOptions extends BaseOptions {
  /**
   * true to capture source screenshare and screenshare_audio
   * false to capture camera and microphone
   */
  screenShare?: boolean;
  /**
   * encoding options or preset. optional
   */
  encodingOptions?: EncodingOptionsPreset | EncodingOptions;
}

export interface TrackCompositeOptions extends BaseOptions {
  /**
   * audio track ID
   */
  audioTrackId?: string;
  /**
   * video track ID
   */
  videoTrackId?: string;
  /**
   * encoding options or preset. optional
   */
  encodingOptions?: EncodingOptionsPreset | EncodingOptions;
}

/**
 * Used to supply multiple outputs with an egress request
 */
export interface EncodedOutputs {
  file?: EncodedFileOutput | undefined;
  stream?: StreamOutput | undefined;
  segments?: SegmentedFileOutput | undefined;
  images?: ImageOutput | undefined;
}

export interface ListEgressOptions {
  roomName?: string;
  egressId?: string;
  active?: boolean;
}

/**
 * Client to access Egress APIs
 */
export class EgressClient extends ServiceBase {
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
   * @param roomName - room name
   * @param output - file or stream output
   * @param opts - RoomCompositeOptions
   */
  async startRoomCompositeEgress(
    roomName: string,
    output: EncodedOutputs | EncodedFileOutput | StreamOutput | SegmentedFileOutput,
    opts?: RoomCompositeOptions,
  ): Promise<EgressInfo>;
  /**
   * @deprecated use RoomCompositeOptions instead
   */
  async startRoomCompositeEgress(
    roomName: string,
    output: EncodedOutputs | EncodedFileOutput | StreamOutput | SegmentedFileOutput,
    layout?: string,
    options?: EncodingOptionsPreset | EncodingOptions,
    audioOnly?: boolean,
    videoOnly?: boolean,
    customBaseUrl?: string,
    audioMixing?: AudioMixing,
  ): Promise<EgressInfo>;
  async startRoomCompositeEgress(
    roomName: string,
    output: EncodedOutputs | EncodedFileOutput | StreamOutput | SegmentedFileOutput,
    optsOrLayout?: RoomCompositeOptions | string,
    options?: EncodingOptionsPreset | EncodingOptions,
    audioOnly?: boolean,
    videoOnly?: boolean,
    customBaseUrl?: string,
    audioMixing?: AudioMixing,
  ): Promise<EgressInfo> {
    let layout: string | undefined;
    let webhooks: WebhookConfig[] | undefined;
    if (optsOrLayout !== undefined) {
      if (typeof optsOrLayout === 'string') {
        layout = optsOrLayout;
      } else {
        const opts = <RoomCompositeOptions>optsOrLayout;
        layout = opts.layout;
        options = opts.encodingOptions;
        audioOnly = opts.audioOnly;
        videoOnly = opts.videoOnly;
        customBaseUrl = opts.customBaseUrl;
        audioMixing = opts.audioMixing;
        webhooks = opts.webhooks;
      }
    }

    layout ??= '';
    audioOnly ??= false;
    videoOnly ??= false;
    customBaseUrl ??= '';
    audioMixing ??= AudioMixing.DEFAULT_MIXING;

    const {
      output: legacyOutput,
      options: egressOptions,
      fileOutputs,
      streamOutputs,
      segmentOutputs,
      imageOutputs,
    } = this.getOutputParams(output, options);

    const req = new RoomCompositeEgressRequest({
      roomName,
      layout,
      audioOnly,
      audioMixing,
      videoOnly,
      customBaseUrl,
      output: legacyOutput,
      options: egressOptions,
      fileOutputs,
      streamOutputs,
      segmentOutputs,
      imageOutputs,
      webhooks,
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'StartRoomCompositeEgress',
      req,
      await this.authHeader({ roomRecord: true }),
    );
    return EgressInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * @param url - url
   * @param output - file or stream output
   * @param opts - WebOptions
   */
  async startWebEgress(
    url: string,
    output: EncodedOutputs | EncodedFileOutput | StreamOutput | SegmentedFileOutput,
    opts?: WebOptions,
  ): Promise<EgressInfo> {
    const audioOnly = opts?.audioOnly || false;
    const videoOnly = opts?.videoOnly || false;
    const awaitStartSignal = opts?.awaitStartSignal || false;
    const webhooks = opts?.webhooks || [];
    const {
      output: legacyOutput,
      options,
      fileOutputs,
      streamOutputs,
      segmentOutputs,
      imageOutputs,
    } = this.getOutputParams(output, opts?.encodingOptions);

    const req = new WebEgressRequest({
      url,
      audioOnly,
      videoOnly,
      awaitStartSignal,
      output: legacyOutput,
      options,
      fileOutputs,
      streamOutputs,
      segmentOutputs,
      imageOutputs,
      webhooks,
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'StartWebEgress',
      req,
      await this.authHeader({ roomRecord: true }),
    );
    return EgressInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Export a participant's audio and video tracks,
   *
   * @param roomName - room name
   * @param output - one or more outputs
   * @param opts - ParticipantEgressOptions
   */
  async startParticipantEgress(
    roomName: string,
    identity: string,
    output: EncodedOutputs,
    opts?: ParticipantEgressOptions,
  ): Promise<EgressInfo> {
    const webhooks = opts?.webhooks || [];
    const { options, fileOutputs, streamOutputs, segmentOutputs, imageOutputs } =
      this.getOutputParams(output, opts?.encodingOptions);
    const req = new ParticipantEgressRequest({
      roomName,
      identity,
      screenShare: opts?.screenShare ?? false,
      options,
      fileOutputs,
      streamOutputs,
      segmentOutputs,
      imageOutputs,
      webhooks,
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'StartParticipantEgress',
      req,
      await this.authHeader({ roomRecord: true }),
    );
    return EgressInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * @param roomName - room name
   * @param output - file or stream output
   * @param opts - TrackCompositeOptions
   */
  async startTrackCompositeEgress(
    roomName: string,
    output: EncodedOutputs | EncodedFileOutput | StreamOutput | SegmentedFileOutput,
    opts?: TrackCompositeOptions,
  ): Promise<EgressInfo>;
  /**
   * @deprecated use TrackCompositeOptions instead
   */
  async startTrackCompositeEgress(
    roomName: string,
    output: EncodedOutputs | EncodedFileOutput | StreamOutput | SegmentedFileOutput,
    audioTrackId?: string,
    videoTrackId?: string,
    options?: EncodingOptionsPreset | EncodingOptions,
  ): Promise<EgressInfo>;
  async startTrackCompositeEgress(
    roomName: string,
    output: EncodedOutputs | EncodedFileOutput | StreamOutput | SegmentedFileOutput,
    optsOrAudioTrackId?: TrackCompositeOptions | string,
    videoTrackId?: string,
    options?: EncodingOptionsPreset | EncodingOptions,
  ): Promise<EgressInfo> {
    let audioTrackId: string | undefined;
    let webhooks: WebhookConfig[] | undefined;
    if (optsOrAudioTrackId !== undefined) {
      if (typeof optsOrAudioTrackId === 'string') {
        audioTrackId = optsOrAudioTrackId;
      } else {
        const opts = <TrackCompositeOptions>optsOrAudioTrackId;
        audioTrackId = opts.audioTrackId;
        videoTrackId = opts.videoTrackId;
        options = opts.encodingOptions;
        webhooks = opts.webhooks;
      }
    }

    audioTrackId ??= '';
    videoTrackId ??= '';

    const {
      output: legacyOutput,
      options: egressOptions,
      fileOutputs,
      streamOutputs,
      segmentOutputs,
      imageOutputs,
    } = this.getOutputParams(output, options);
    const req = new TrackCompositeEgressRequest({
      roomName,
      audioTrackId,
      videoTrackId,
      output: legacyOutput,
      options: egressOptions,
      fileOutputs,
      streamOutputs,
      segmentOutputs,
      imageOutputs,
      webhooks,
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'StartTrackCompositeEgress',
      req,
      await this.authHeader({ roomRecord: true }),
    );
    return EgressInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  private isEncodedOutputs(output: any): output is EncodedOutputs {
    return (
      (<EncodedOutputs>output).file !== undefined ||
      (<EncodedOutputs>output).stream !== undefined ||
      (<EncodedOutputs>output).segments !== undefined ||
      (<EncodedOutputs>output).images !== undefined
    );
  }

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  private isEncodedFileOutput(output: any): output is EncodedFileOutput {
    return (
      (<EncodedFileOutput>output).filepath !== undefined ||
      (<EncodedFileOutput>output).fileType !== undefined
    );
  }

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  private isSegmentedFileOutput(output: any): output is SegmentedFileOutput {
    return (
      (<SegmentedFileOutput>output).filenamePrefix !== undefined ||
      (<SegmentedFileOutput>output).playlistName !== undefined ||
      (<SegmentedFileOutput>output).filenameSuffix !== undefined
    );
  }

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  private isStreamOutput(output: any): output is StreamOutput {
    return (
      (<StreamOutput>output).protocol !== undefined || (<StreamOutput>output).urls !== undefined
    );
  }

  private getOutputParams(
    output: EncodedOutputs | EncodedFileOutput | StreamOutput | SegmentedFileOutput,
    opts?: EncodingOptionsPreset | EncodingOptions,
  ) {
    let file: EncodedFileOutput | undefined;
    let fileOutputs: Array<EncodedFileOutput> | undefined;
    let stream: StreamOutput | undefined;
    let streamOutputs: Array<StreamOutput> | undefined;
    let segments: SegmentedFileOutput | undefined;
    let segmentOutputs: Array<SegmentedFileOutput> | undefined;
    let imageOutputs: Array<ImageOutput> | undefined;

    if (this.isEncodedOutputs(output)) {
      if (output.file !== undefined) {
        fileOutputs = [output.file];
      }
      if (output.stream !== undefined) {
        streamOutputs = [output.stream];
      }
      if (output.segments !== undefined) {
        segmentOutputs = [output.segments];
      }
      if (output.images !== undefined) {
        imageOutputs = [output.images];
      }
    } else if (this.isEncodedFileOutput(output)) {
      file = output;
      fileOutputs = [file];
    } else if (this.isSegmentedFileOutput(output)) {
      segments = output;
      segmentOutputs = [segments];
    } else if (this.isStreamOutput(output)) {
      stream = output;
      streamOutputs = [stream];
    }

    let legacyOutput:
      | {
          value: EncodedFileOutput;
          case: 'file';
        }
      | {
          value: StreamOutput;
          case: 'stream';
        }
      | {
          value: SegmentedFileOutput;
          case: 'segments';
        }
      | undefined;

    if (file) {
      legacyOutput = {
        case: 'file',
        value: file,
      };
    } else if (stream) {
      legacyOutput = {
        case: 'stream',
        value: stream,
      };
    } else if (segments) {
      legacyOutput = {
        case: 'segments',
        value: segments,
      };
    }
    let egressOptions:
      | {
          value: EncodingOptionsPreset;
          case: 'preset';
        }
      | {
          value: EncodingOptions;
          case: 'advanced';
        }
      | undefined;
    if (opts) {
      if (typeof opts === 'number') {
        egressOptions = {
          case: 'preset',
          value: opts,
        };
      } else {
        egressOptions = {
          case: 'advanced',
          value: <EncodingOptions>opts,
        };
      }
    }

    return {
      output: legacyOutput,
      options: egressOptions,
      fileOutputs,
      streamOutputs,
      segmentOutputs,
      imageOutputs,
    };
  }

  /**
   * @param roomName - room name
   * @param output - file or websocket output
   * @param trackId - track Id
   */
  async startTrackEgress(
    roomName: string,
    output: DirectFileOutput | string,
    trackId: string,
    webhooks?: WebhookConfig[],
  ): Promise<EgressInfo> {
    let legacyOutput:
      | {
          value: DirectFileOutput;
          case: 'file';
        }
      | {
          value: string;
          case: 'websocketUrl';
        }
      | undefined;

    if (typeof output === 'string') {
      legacyOutput = {
        case: 'websocketUrl',
        value: output,
      };
    } else {
      legacyOutput = {
        case: 'file',
        value: output,
      };
    }

    const req = new TrackEgressRequest({
      roomName,
      trackId,
      output: legacyOutput,
      webhooks,
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'StartTrackEgress',
      req,
      await this.authHeader({ roomRecord: true }),
    );
    return EgressInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * @param egressId -
   * @param layout -
   */
  async updateLayout(egressId: string, layout: string): Promise<EgressInfo> {
    const data = await this.rpc.request(
      svc,
      'UpdateLayout',
      new UpdateLayoutRequest({ egressId, layout }).toJson(),
      await this.authHeader({ roomRecord: true }),
    );
    return EgressInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * @param egressId -
   * @param addOutputUrls -
   * @param removeOutputUrls -
   */
  async updateStream(
    egressId: string,
    addOutputUrls?: string[],
    removeOutputUrls?: string[],
  ): Promise<EgressInfo> {
    addOutputUrls ??= [];
    removeOutputUrls ??= [];

    const data = await this.rpc.request(
      svc,
      'UpdateStream',
      new UpdateStreamRequest({ egressId, addOutputUrls, removeOutputUrls }).toJson(),
      await this.authHeader({ roomRecord: true }),
    );
    return EgressInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * @param options - options to filter listed Egresses, by default returns all
   * Egress instances
   */
  async listEgress(options?: ListEgressOptions): Promise<Array<EgressInfo>>;
  /**
   * @deprecated use `listEgress(options?: ListEgressOptions)` instead
   * @param roomName - list egress for one room only
   */
  async listEgress(roomName?: string): Promise<Array<EgressInfo>>;
  /**
   * @param roomName - list egress for one room only
   */
  async listEgress(options?: string | ListEgressOptions): Promise<Array<EgressInfo>> {
    let req: Partial<ListEgressRequest> = {};
    if (typeof options === 'string') {
      req.roomName = options;
    } else if (options !== undefined) {
      req = options;
    }

    const data = await this.rpc.request(
      svc,
      'ListEgress',
      new ListEgressRequest(req).toJson(),
      await this.authHeader({ roomRecord: true }),
    );
    return ListEgressResponse.fromJson(data, { ignoreUnknownFields: true }).items ?? [];
  }

  /**
   * @param egressId -
   */
  async stopEgress(egressId: string): Promise<EgressInfo> {
    const data = await this.rpc.request(
      svc,
      'StopEgress',
      new StopEgressRequest({ egressId }).toJson(),
      await this.authHeader({ roomRecord: true }),
    );
    return EgressInfo.fromJson(data, { ignoreUnknownFields: true });
  }
}
