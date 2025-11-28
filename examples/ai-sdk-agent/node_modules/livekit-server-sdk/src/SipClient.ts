// SPDX-FileCopyrightText: 2024 LiveKit, Inc.
//
// SPDX-License-Identifier: Apache-2.0
import { Duration } from '@bufbuild/protobuf';
import type {
  ListUpdate,
  Pagination,
  RoomConfiguration,
  SIPHeaderOptions,
} from '@livekit/protocol';
import {
  CreateSIPDispatchRuleRequest,
  CreateSIPInboundTrunkRequest,
  CreateSIPOutboundTrunkRequest,
  CreateSIPParticipantRequest,
  CreateSIPTrunkRequest,
  DeleteSIPDispatchRuleRequest,
  DeleteSIPTrunkRequest,
  ListSIPDispatchRuleRequest,
  ListSIPDispatchRuleResponse,
  ListSIPInboundTrunkRequest,
  ListSIPInboundTrunkResponse,
  ListSIPOutboundTrunkRequest,
  ListSIPOutboundTrunkResponse,
  ListSIPTrunkRequest,
  ListSIPTrunkResponse,
  SIPDispatchRule,
  SIPDispatchRuleDirect,
  SIPDispatchRuleIndividual,
  SIPDispatchRuleInfo,
  SIPInboundTrunkInfo,
  SIPOutboundTrunkInfo,
  SIPParticipantInfo,
  SIPTransport,
  SIPTrunkInfo,
  TransferSIPParticipantRequest,
  UpdateSIPDispatchRuleRequest,
  UpdateSIPInboundTrunkRequest,
  UpdateSIPOutboundTrunkRequest,
} from '@livekit/protocol';
import { ServiceBase } from './ServiceBase.js';
import type { Rpc } from './TwirpRPC.js';
import { TwirpRpc, livekitPackage } from './TwirpRPC.js';

const svc = 'SIP';

/**
 * @deprecated use CreateSipInboundTrunkOptions or CreateSipOutboundTrunkOptions
 */
export interface CreateSipTrunkOptions {
  name?: string;
  metadata?: string;
  inbound_addresses?: string[];
  inbound_numbers?: string[];
  inbound_username?: string;
  inbound_password?: string;
  outbound_address?: string;
  outbound_username?: string;
  outbound_password?: string;
}
export interface CreateSipInboundTrunkOptions {
  metadata?: string;
  /** @deprecated - use `allowedAddresses` instead */
  allowed_addresses?: string[];
  allowedAddresses?: string[];
  /** @deprecated - use `allowedNumbers` instead */
  allowed_numbers?: string[];
  allowedNumbers?: string[];
  /** @deprecated - use `authUsername` instead */
  auth_username?: string;
  authUsername?: string;
  /** @deprecated - use `authPassword` instead */
  auth_password?: string;
  authPassword?: string;
  headers?: { [key: string]: string };
  headersToAttributes?: { [key: string]: string };
  // Map SIP response headers from INVITE to sip.h.* participant attributes automatically.
  includeHeaders?: SIPHeaderOptions;
  krispEnabled?: boolean;
}
export interface CreateSipOutboundTrunkOptions {
  metadata?: string;
  transport: SIPTransport;
  destinationCountry?: string;
  /** @deprecated - use `authUsername` instead */
  auth_username?: string;
  authUsername?: string;
  /** @deprecated - use `authPassword` instead */
  auth_password?: string;
  authPassword?: string;
  headers?: { [key: string]: string };
  headersToAttributes?: { [key: string]: string };
  // Map SIP response headers from INVITE to sip.h.* participant attributes automatically.
  includeHeaders?: SIPHeaderOptions;
}

export interface SipDispatchRuleDirect {
  type: 'direct';
  roomName: string;
  pin?: string;
}

export interface SipDispatchRuleIndividual {
  type: 'individual';
  roomPrefix: string;
  pin?: string;
}

export interface CreateSipDispatchRuleOptions {
  name?: string;
  metadata?: string;
  trunkIds?: string[];
  hidePhoneNumber?: boolean;
  attributes?: { [key: string]: string };
  roomPreset?: string;
  roomConfig?: RoomConfiguration;
}

export interface CreateSipParticipantOptions {
  /** Optional SIP From number to use. If empty, trunk number is used. */
  fromNumber?: string;
  /** Optional identity of the SIP participant */
  participantIdentity?: string;
  /** Optional name of the participant */
  participantName?: string;
  /** Optional display name for the SIP participant */
  displayName?: string;
  /** Optional metadata to attach to the participant */
  participantMetadata?: string;
  /** Optional attributes to attach to the participant */
  participantAttributes?: { [key: string]: string };
  /** Optionally send following DTMF digits (extension codes) when making a call.
   * Character 'w' can be used to add a 0.5 sec delay. */
  dtmf?: string;
  /** @deprecated use `playDialtone` instead */
  playRingtone?: boolean;
  /** If `true`, the SIP Participant plays a dial tone to the room until the phone is picked up. */
  playDialtone?: boolean;
  /** These headers are sent as-is and may help identify this call as coming from LiveKit for the other SIP endpoint. */
  headers?: { [key: string]: string };
  /** Map SIP response headers from INVITE to sip.h.* participant attributes automatically. */
  includeHeaders?: SIPHeaderOptions;
  hidePhoneNumber?: boolean;
  /** Maximum time for the call to ring in seconds. */
  ringingTimeout?: number;
  /** Maximum call duration in seconds. */
  maxCallDuration?: number;
  /** If `true`, Krisp noise cancellation will be enabled for the caller. */
  krispEnabled?: boolean;
  /** If `true`, this will wait until the call is answered before returning. */
  waitUntilAnswered?: boolean;
  /** Optional request timeout in seconds. default 60 seconds if waitUntilAnswered is true, otherwise 10 seconds */
  timeout?: number;
}

export interface ListSipDispatchRuleOptions {
  /** Pagination options. */
  page?: Pagination;
  /** Rule IDs to list. If this option is set, the response will contains rules in the same order. If any of the rules is missing, a nil item in that position will be sent in the response. */
  dispatchRuleIds?: string[];
  /** Only list rules that contain one of the Trunk IDs, including wildcard rules. */
  trunkIds?: string[];
}

export interface ListSipTrunkOptions {
  /** Pagination options. */
  page?: Pagination;
  /** Trunk IDs to list. If this option is set, the response will contains trunks in the same order. If any of the trunks is missing, a nil item in that position will be sent in the response. */
  trunkIds?: string[];
  /** Only list trunks that contain one of the numbers, including wildcard trunks. */
  numbers?: string[];
}

export interface SipDispatchRuleUpdateOptions {
  trunkIds?: ListUpdate;
  rule?: SIPDispatchRule;
  name?: string;
  metadata?: string;
  attributes?: { [key: string]: string };
}

export interface SipInboundTrunkUpdateOptions {
  numbers?: ListUpdate;
  allowedAddresses?: ListUpdate;
  allowedNumbers?: ListUpdate;
  authUsername?: string;
  authPassword?: string;
  name?: string;
  metadata?: string;
}

export interface SipOutboundTrunkUpdateOptions {
  numbers?: ListUpdate;
  allowedAddresses?: ListUpdate;
  allowedNumbers?: ListUpdate;
  authUsername?: string;
  authPassword?: string;
  destinationCountry?: string;
  name?: string;
  metadata?: string;
}

export interface TransferSipParticipantOptions {
  playDialtone?: boolean;
  headers?: { [key: string]: string };
}

/**
 * Client to access Egress APIs
 */
export class SipClient extends ServiceBase {
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
   * @param number - phone number of the trunk
   * @param opts - CreateSipTrunkOptions
   * @deprecated use `createSipInboundTrunk` or `createSipOutboundTrunk`
   */
  async createSipTrunk(number: string, opts?: CreateSipTrunkOptions): Promise<SIPTrunkInfo> {
    let inboundAddresses: string[] | undefined;
    let inboundNumbers: string[] | undefined;
    let inboundUsername: string = '';
    let inboundPassword: string = '';
    let outboundAddress: string = '';
    let outboundUsername: string = '';
    let outboundPassword: string = '';
    let name: string = '';
    let metadata: string = '';

    if (opts !== undefined) {
      inboundAddresses = opts.inbound_addresses;
      inboundNumbers = opts.inbound_numbers;
      inboundUsername = opts.inbound_username || '';
      inboundPassword = opts.inbound_password || '';
      outboundAddress = opts.outbound_address || '';
      outboundUsername = opts.outbound_username || '';
      outboundPassword = opts.outbound_password || '';
      name = opts.name || '';
      metadata = opts.metadata || '';
    }

    const req = new CreateSIPTrunkRequest({
      name: name,
      metadata: metadata,
      inboundAddresses: inboundAddresses,
      inboundNumbers: inboundNumbers,
      inboundUsername: inboundUsername,
      inboundPassword: inboundPassword,
      outboundNumber: number,
      outboundAddress: outboundAddress,
      outboundUsername: outboundUsername,
      outboundPassword: outboundPassword,
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'CreateSIPTrunk',
      req,
      await this.authHeader({}, { admin: true }),
    );
    return SIPTrunkInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Create a new SIP inbound trunk.
   *
   * @param name - human-readable name of the trunk
   * @param numbers - phone numbers of the trunk
   * @param opts - CreateSipTrunkOptions
   * @returns Created SIP inbound trunk
   */
  async createSipInboundTrunk(
    name: string,
    numbers: string[],
    opts?: CreateSipInboundTrunkOptions,
  ): Promise<SIPInboundTrunkInfo> {
    if (opts === undefined) {
      opts = {};
    }
    const req = new CreateSIPInboundTrunkRequest({
      trunk: new SIPInboundTrunkInfo({
        name: name,
        numbers: numbers,
        metadata: opts?.metadata,
        allowedAddresses: opts.allowedAddresses ?? opts.allowed_addresses,
        allowedNumbers: opts.allowedNumbers ?? opts.allowed_numbers,
        authUsername: opts.authUsername ?? opts.auth_username,
        authPassword: opts.authPassword ?? opts.auth_password,
        headers: opts.headers,
        headersToAttributes: opts.headersToAttributes,
        includeHeaders: opts.includeHeaders,
        krispEnabled: opts.krispEnabled,
      }),
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'CreateSIPInboundTrunk',
      req,
      await this.authHeader({}, { admin: true }),
    );
    return SIPInboundTrunkInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Create a new SIP outbound trunk.
   *
   * @param name - human-readable name of the trunk
   * @param address - hostname and port of the SIP server to dial
   * @param numbers - phone numbers of the trunk
   * @param opts - CreateSipTrunkOptions
   * @returns Created SIP outbound trunk
   */
  async createSipOutboundTrunk(
    name: string,
    address: string,
    numbers: string[],
    opts?: CreateSipOutboundTrunkOptions,
  ): Promise<SIPOutboundTrunkInfo> {
    if (opts === undefined) {
      opts = {
        transport: SIPTransport.SIP_TRANSPORT_AUTO,
      };
    }

    const req = new CreateSIPOutboundTrunkRequest({
      trunk: new SIPOutboundTrunkInfo({
        name: name,
        address: address,
        numbers: numbers,
        metadata: opts.metadata,
        transport: opts.transport,
        authUsername: opts.authUsername ?? opts.auth_username,
        authPassword: opts.authPassword ?? opts.auth_password,
        headers: opts.headers,
        headersToAttributes: opts.headersToAttributes,
        includeHeaders: opts.includeHeaders,
        destinationCountry: opts.destinationCountry,
      }),
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'CreateSIPOutboundTrunk',
      req,
      await this.authHeader({}, { admin: true }),
    );
    return SIPOutboundTrunkInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * @deprecated use `listSipInboundTrunk` or `listSipOutboundTrunk`
   */
  async listSipTrunk(): Promise<Array<SIPTrunkInfo>> {
    const req: Partial<ListSIPTrunkRequest> = {};
    const data = await this.rpc.request(
      svc,
      'ListSIPTrunk',
      new ListSIPTrunkRequest(req).toJson(),
      await this.authHeader({}, { admin: true }),
    );
    return ListSIPTrunkResponse.fromJson(data, { ignoreUnknownFields: true }).items ?? [];
  }

  /**
   * List SIP inbound trunks with optional filtering.
   *
   * @param list - Request with optional filtering parameters
   * @returns Response containing list of SIP inbound trunks
   */
  async listSipInboundTrunk(list: ListSipTrunkOptions = {}): Promise<Array<SIPInboundTrunkInfo>> {
    const req = new ListSIPInboundTrunkRequest(list).toJson();
    const data = await this.rpc.request(
      svc,
      'ListSIPInboundTrunk',
      req,
      await this.authHeader({}, { admin: true }),
    );
    return ListSIPInboundTrunkResponse.fromJson(data, { ignoreUnknownFields: true }).items ?? [];
  }

  /**
   * List SIP outbound trunks with optional filtering.
   *
   * @param list - Request with optional filtering parameters
   * @returns Response containing list of SIP outbound trunks
   */
  async listSipOutboundTrunk(list: ListSipTrunkOptions = {}): Promise<Array<SIPOutboundTrunkInfo>> {
    const req = new ListSIPOutboundTrunkRequest(list).toJson();
    const data = await this.rpc.request(
      svc,
      'ListSIPOutboundTrunk',
      req,
      await this.authHeader({}, { admin: true }),
    );
    return ListSIPOutboundTrunkResponse.fromJson(data, { ignoreUnknownFields: true }).items ?? [];
  }

  /**
   * Delete a SIP trunk.
   *
   * @param sipTrunkId - ID of the SIP trunk to delete
   * @returns Deleted trunk information
   */
  async deleteSipTrunk(sipTrunkId: string): Promise<SIPTrunkInfo> {
    const data = await this.rpc.request(
      svc,
      'DeleteSIPTrunk',
      new DeleteSIPTrunkRequest({ sipTrunkId }).toJson(),
      await this.authHeader({}, { admin: true }),
    );
    return SIPTrunkInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Create a new SIP dispatch rule.
   *
   * @param rule - SIP dispatch rule to create
   * @param opts - CreateSipDispatchRuleOptions
   * @returns Created SIP dispatch rule
   */
  async createSipDispatchRule(
    rule: SipDispatchRuleDirect | SipDispatchRuleIndividual,
    opts?: CreateSipDispatchRuleOptions,
  ): Promise<SIPDispatchRuleInfo> {
    if (opts === undefined) {
      opts = {};
    }
    let ruleProto: SIPDispatchRule | undefined = undefined;
    if (rule.type == 'direct') {
      ruleProto = new SIPDispatchRule({
        rule: {
          case: 'dispatchRuleDirect',
          value: new SIPDispatchRuleDirect({
            roomName: rule.roomName,
            pin: rule.pin || '',
          }),
        },
      });
    } else if (rule.type == 'individual') {
      ruleProto = new SIPDispatchRule({
        rule: {
          case: 'dispatchRuleIndividual',
          value: new SIPDispatchRuleIndividual({
            roomPrefix: rule.roomPrefix,
            pin: rule.pin || '',
          }),
        },
      });
    }

    const req = new CreateSIPDispatchRuleRequest({
      rule: ruleProto,
      trunkIds: opts.trunkIds,
      hidePhoneNumber: opts.hidePhoneNumber,
      name: opts.name,
      metadata: opts.metadata,
      attributes: opts.attributes,
      roomPreset: opts.roomPreset,
      roomConfig: opts.roomConfig,
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'CreateSIPDispatchRule',
      req,
      await this.authHeader({}, { admin: true }),
    );
    return SIPDispatchRuleInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Updates an existing SIP dispatch rule by replacing it entirely.
   *
   * @param sipDispatchRuleId - ID of the SIP dispatch rule to update
   * @param rule - new SIP dispatch rule
   * @returns Updated SIP dispatch rule
   */
  async updateSipDispatchRule(
    sipDispatchRuleId: string,
    rule: SIPDispatchRuleInfo,
  ): Promise<SIPDispatchRuleInfo> {
    const req = new UpdateSIPDispatchRuleRequest({
      sipDispatchRuleId: sipDispatchRuleId,
      action: {
        case: 'replace',
        value: rule,
      },
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'UpdateSIPDispatchRule',
      req,
      await this.authHeader({}, { admin: true }),
    );

    return SIPDispatchRuleInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Updates specific fields of an existing SIP dispatch rule.
   * Only provided fields will be updated.
   *
   * @param sipDispatchRuleId - ID of the SIP dispatch rule to update
   * @param fields - Fields of the dispatch rule to update
   * @returns Updated SIP dispatch rule
   */
  async updateSipDispatchRuleFields(
    sipDispatchRuleId: string,
    fields: SipDispatchRuleUpdateOptions = {},
  ): Promise<SIPDispatchRuleInfo> {
    const req = new UpdateSIPDispatchRuleRequest({
      sipDispatchRuleId: sipDispatchRuleId,
      action: {
        case: 'update',
        value: fields,
      },
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'UpdateSIPDispatchRule',
      req,
      await this.authHeader({}, { admin: true }),
    );

    return SIPDispatchRuleInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Updates an existing SIP inbound trunk by replacing it entirely.
   *
   * @param sipTrunkId - ID of the SIP inbound trunk to update
   * @param trunk - SIP inbound trunk to update with
   * @returns Updated SIP inbound trunk
   */
  async updateSipInboundTrunk(
    sipTrunkId: string,
    trunk: SIPInboundTrunkInfo,
  ): Promise<SIPInboundTrunkInfo> {
    const req = new UpdateSIPInboundTrunkRequest({
      sipTrunkId,
      action: {
        case: 'replace',
        value: trunk,
      },
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'UpdateSIPInboundTrunk',
      req,
      await this.authHeader({}, { admin: true }),
    );

    return SIPInboundTrunkInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Updates specific fields of an existing SIP inbound trunk.
   * Only provided fields will be updated.
   *
   * @param sipTrunkId - ID of the SIP inbound trunk to update
   * @param fields - Fields of the inbound trunk to update
   * @returns Updated SIP inbound trunk
   */
  async updateSipInboundTrunkFields(
    sipTrunkId: string,
    fields: SipInboundTrunkUpdateOptions,
  ): Promise<SIPInboundTrunkInfo> {
    const req = new UpdateSIPInboundTrunkRequest({
      sipTrunkId,
      action: {
        case: 'update',
        value: fields,
      },
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'UpdateSIPInboundTrunk',
      req,
      await this.authHeader({}, { admin: true }),
    );

    return SIPInboundTrunkInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Updates an existing SIP outbound trunk by replacing it entirely.
   *
   * @param sipTrunkId - ID of the SIP outbound trunk to update
   * @param trunk - SIP outbound trunk to update with
   * @returns Updated SIP outbound trunk
   */
  async updateSipOutboundTrunk(
    sipTrunkId: string,
    trunk: SIPOutboundTrunkInfo,
  ): Promise<SIPOutboundTrunkInfo> {
    const req = new UpdateSIPOutboundTrunkRequest({
      sipTrunkId,
      action: {
        case: 'replace',
        value: trunk,
      },
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'UpdateSIPOutboundTrunk',
      req,
      await this.authHeader({}, { admin: true }),
    );

    return SIPOutboundTrunkInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Updates specific fields of an existing SIP outbound trunk.
   * Only provided fields will be updated.
   *
   * @param sipTrunkId - ID of the SIP outbound trunk to update
   * @param fields - Fields of the outbound trunk to update
   * @returns Updated SIP outbound trunk
   */
  async updateSipOutboundTrunkFields(
    sipTrunkId: string,
    fields: SipOutboundTrunkUpdateOptions,
  ): Promise<SIPOutboundTrunkInfo> {
    const req = new UpdateSIPOutboundTrunkRequest({
      sipTrunkId,
      action: {
        case: 'update',
        value: fields,
      },
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'UpdateSIPOutboundTrunk',
      req,
      await this.authHeader({}, { admin: true }),
    );

    return SIPOutboundTrunkInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * List SIP dispatch rules with optional filtering.
   *
   * @param list - Request with optional filtering parameters
   * @returns Response containing list of SIP dispatch rules
   */
  async listSipDispatchRule(
    list: ListSipDispatchRuleOptions = {},
  ): Promise<Array<SIPDispatchRuleInfo>> {
    const req = new ListSIPDispatchRuleRequest(list).toJson();
    const data = await this.rpc.request(
      svc,
      'ListSIPDispatchRule',
      req,
      await this.authHeader({}, { admin: true }),
    );
    return ListSIPDispatchRuleResponse.fromJson(data, { ignoreUnknownFields: true }).items ?? [];
  }

  /**
   * Delete a SIP dispatch rule.
   *
   * @param sipDispatchRuleId - ID of the SIP dispatch rule to delete
   * @returns Deleted rule information
   */
  async deleteSipDispatchRule(sipDispatchRuleId: string): Promise<SIPDispatchRuleInfo> {
    const data = await this.rpc.request(
      svc,
      'DeleteSIPDispatchRule',
      new DeleteSIPDispatchRuleRequest({ sipDispatchRuleId }).toJson(),
      await this.authHeader({}, { admin: true }),
    );
    return SIPDispatchRuleInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Create a new SIP participant.
   *
   * @param sipTrunkId - sip trunk to use for the call
   * @param number - number to dial
   * @param roomName - room to attach the call to
   * @param opts - CreateSipParticipantOptions
   * @returns Created SIP participant
   */
  async createSipParticipant(
    sipTrunkId: string,
    number: string,
    roomName: string,
    opts?: CreateSipParticipantOptions,
  ): Promise<SIPParticipantInfo> {
    if (opts === undefined) {
      opts = {};
    }

    if (opts.timeout === undefined) {
      opts.timeout = opts.waitUntilAnswered ? 60 : 10;
    }

    const req = new CreateSIPParticipantRequest({
      sipTrunkId: sipTrunkId,
      sipCallTo: number,
      sipNumber: opts.fromNumber,
      roomName: roomName,
      participantIdentity: opts.participantIdentity || 'sip-participant',
      participantName: opts.participantName,
      displayName: opts.displayName,
      participantMetadata: opts.participantMetadata,
      participantAttributes: opts.participantAttributes,
      dtmf: opts.dtmf,
      playDialtone: opts.playDialtone ?? opts.playRingtone,
      headers: opts.headers,
      hidePhoneNumber: opts.hidePhoneNumber,
      includeHeaders: opts.includeHeaders,
      ringingTimeout: opts.ringingTimeout
        ? new Duration({ seconds: BigInt(opts.ringingTimeout) })
        : undefined,
      maxCallDuration: opts.maxCallDuration
        ? new Duration({ seconds: BigInt(opts.maxCallDuration) })
        : undefined,
      krispEnabled: opts.krispEnabled,
      waitUntilAnswered: opts.waitUntilAnswered,
    }).toJson();

    const data = await this.rpc.request(
      svc,
      'CreateSIPParticipant',
      req,
      await this.authHeader({}, { call: true }),
      opts.timeout,
    );
    return SIPParticipantInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Transfer a SIP participant to a different room.
   *
   * @param roomName - room the SIP participant to transfer is connectd to
   * @param participantIdentity - identity of the SIP participant to transfer
   * @param transferTo - SIP URL to transfer the participant to
   * @param opts - TransferSipParticipantOptions
   */
  async transferSipParticipant(
    roomName: string,
    participantIdentity: string,
    transferTo: string,
    opts?: TransferSipParticipantOptions,
  ): Promise<void> {
    if (opts === undefined) {
      opts = {};
    }

    const req = new TransferSIPParticipantRequest({
      participantIdentity: participantIdentity,
      roomName: roomName,
      transferTo: transferTo,
      playDialtone: opts.playDialtone,
      headers: opts.headers,
    }).toJson();

    await this.rpc.request(
      svc,
      'TransferSIPParticipant',
      req,
      await this.authHeader({ roomAdmin: true, room: roomName }, { call: true }),
    );
  }
}
