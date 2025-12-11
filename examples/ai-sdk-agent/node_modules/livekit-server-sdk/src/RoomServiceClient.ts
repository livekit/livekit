// SPDX-FileCopyrightText: 2024 LiveKit, Inc.
//
// SPDX-License-Identifier: Apache-2.0
import type { DataPacket_Kind, RoomAgentDispatch, RoomEgress, TrackInfo } from '@livekit/protocol';
import {
  CreateRoomRequest,
  DeleteRoomRequest,
  ForwardParticipantRequest,
  ListParticipantsRequest,
  ListParticipantsResponse,
  ListRoomsRequest,
  ListRoomsResponse,
  MoveParticipantRequest,
  MuteRoomTrackRequest,
  MuteRoomTrackResponse,
  ParticipantInfo,
  ParticipantPermission,
  Room,
  RoomParticipantIdentity,
  SendDataRequest,
  UpdateParticipantRequest,
  UpdateRoomMetadataRequest,
  UpdateSubscriptionsRequest,
} from '@livekit/protocol';
import { ServiceBase } from './ServiceBase.js';
import type { Rpc } from './TwirpRPC.js';
import { TwirpRpc, livekitPackage } from './TwirpRPC.js';
import { getRandomBytes } from './crypto/uuid.js';

/**
 * Options for when creating a room
 */
export interface CreateOptions {
  /**
   * name of the room. required
   */
  name: string;

  /**
   * number of seconds to keep the room open before any participant joins
   */
  emptyTimeout?: number;

  /**
   * number of seconds to keep the room open after the last participant leaves
   * this option is helpful to give a grace period for participants to re-join
   */
  departureTimeout?: number;

  /**
   * limit to the number of participants in a room at a time
   */
  maxParticipants?: number;

  /**
   * initial room metadata
   */
  metadata?: string;

  /**
   * add egress options
   */
  egress?: RoomEgress;

  /**
   * minimum playout delay in milliseconds
   */
  minPlayoutDelay?: number;

  /**
   * maximum playout delay in milliseconds
   */
  maxPlayoutDelay?: number;

  /**
   * improves A/V sync when min_playout_delay set to a value larger than 200ms.
   * It will disables transceiver re-use -- this option is not recommended
   * for rooms with frequent subscription changes
   */
  syncStreams?: boolean;

  /**
   * agents that should be dispatched to this room
   */
  agents?: RoomAgentDispatch[];

  /**
   * override the node room is allocated to, for debugging
   * does not work with Cloud
   */
  nodeId?: string;
}

export type SendDataOptions = {
  /** If set, only deliver to listed participant identities */
  destinationIdentities?: string[];
  destinationSids?: string[];
  topic?: string;
};

export type UpdateParticipantOptions = {
  /** only attributes you'd want to update should be set, set value to empty string to remove it */
  attributes?: { [key: string]: string };
  metadata?: string;
  /** permissions are updated atomically - all desired permissions would need to be set */
  permission?: Partial<ParticipantPermission>;
  name?: string;
};

const svc = 'RoomService';

/**
 * Client to access Room APIs
 */
export class RoomServiceClient extends ServiceBase {
  private readonly rpc: Rpc;

  /**
   *
   * @param host - hostname including protocol. i.e. 'https://<project>.livekit.cloud'
   * @param apiKey - API Key, can be set in env var LIVEKIT_API_KEY
   * @param secret - API Secret, can be set in env var LIVEKIT_API_SECRET
   */
  constructor(host: string, apiKey?: string, secret?: string) {
    super(apiKey, secret);
    this.rpc = new TwirpRpc(host, livekitPackage);
  }

  /**
   * Creates a new room. Explicit room creation is not required, since rooms will
   * be automatically created when the first participant joins. This method can be
   * used to customize room settings.
   * @param options -
   */
  async createRoom(options: CreateOptions): Promise<Room> {
    const data = await this.rpc.request(
      svc,
      'CreateRoom',
      new CreateRoomRequest(options).toJson(),
      await this.authHeader({ roomCreate: true }),
    );
    return Room.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * List active rooms
   * @param names - when undefined or empty, list all rooms.
   *                otherwise returns rooms with matching names
   * @returns
   */
  async listRooms(names?: string[]): Promise<Room[]> {
    const data = await this.rpc.request(
      svc,
      'ListRooms',
      new ListRoomsRequest({ names: names ?? [] }).toJson(),
      await this.authHeader({ roomList: true }),
    );
    const res = ListRoomsResponse.fromJson(data, { ignoreUnknownFields: true });
    return res.rooms ?? [];
  }

  async deleteRoom(room: string): Promise<void> {
    await this.rpc.request(
      svc,
      'DeleteRoom',
      new DeleteRoomRequest({ room }).toJson(),
      await this.authHeader({ roomCreate: true }),
    );
  }

  /**
   * Update metadata of a room
   * @param room - name of the room
   * @param metadata - the new metadata for the room
   */
  async updateRoomMetadata(room: string, metadata: string) {
    const data = await this.rpc.request(
      svc,
      'UpdateRoomMetadata',
      new UpdateRoomMetadataRequest({ room, metadata }).toJson(),
      await this.authHeader({ roomAdmin: true, room }),
    );
    return Room.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * List participants in a room
   * @param room - name of the room
   */
  async listParticipants(room: string): Promise<ParticipantInfo[]> {
    const data = await this.rpc.request(
      svc,
      'ListParticipants',
      new ListParticipantsRequest({ room }).toJson(),
      await this.authHeader({ roomAdmin: true, room }),
    );
    const res = ListParticipantsResponse.fromJson(data, { ignoreUnknownFields: true });
    return res.participants ?? [];
  }

  /**
   * Get information on a specific participant, including the tracks that participant
   * has published
   * @param room - name of the room
   * @param identity - identity of the participant to return
   */
  async getParticipant(room: string, identity: string): Promise<ParticipantInfo> {
    const data = await this.rpc.request(
      svc,
      'GetParticipant',
      new RoomParticipantIdentity({ room, identity }).toJson(),
      await this.authHeader({ roomAdmin: true, room }),
    );

    return ParticipantInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Removes a participant in the room. This will disconnect the participant
   * and will emit a Disconnected event for that participant.
   * Even after being removed, the participant can still re-join the room.
   * @param room -
   * @param identity -
   */
  async removeParticipant(room: string, identity: string): Promise<void> {
    await this.rpc.request(
      svc,
      'RemoveParticipant',
      new RoomParticipantIdentity({ room, identity }).toJson(),
      await this.authHeader({ roomAdmin: true, room }),
    );
  }

  /**
   * Forwards a participant's track to another room. This will create a
   * participant to join the destination room that has same information
   * with the source participant except the kind to be `Forwarded`. All
   * changes to the source participant will be reflected to the forwarded
   * participant. When the source participant disconnects or the
   * `RemoveParticipant` method is called in the destination room, the
   * forwarding will be stopped.
   * @param room -
   * @param identity -
   * @param destinationRoom - the room to forward the participant to
   */
  async forwardParticipant(room: string, identity: string, destinationRoom: string): Promise<void> {
    await this.rpc.request(
      svc,
      'ForwardParticipant',
      new ForwardParticipantRequest({ room, identity, destinationRoom }).toJson(),
      await this.authHeader({ roomAdmin: true, room, destinationRoom }),
    );
  }

  /**
   * Move a connected participant to a different room. Requires `roomAdmin` and `destinationRoom`.
   * The participant will be removed from the current room and added to the destination room.
   * From the other observers' perspective, the participant would've disconnected from the previous room and joined the new one.
   * @param room -
   * @param identity -
   * @param destinationRoom - the room to move the participant to
   */
  async moveParticipant(room: string, identity: string, destinationRoom: string): Promise<void> {
    await this.rpc.request(
      svc,
      'MoveParticipant',
      new MoveParticipantRequest({ room, identity, destinationRoom }).toJson(),
      await this.authHeader({ roomAdmin: true, room, destinationRoom }),
    );
  }

  /**
   * Mutes a track that the participant has published.
   * @param room -
   * @param identity -
   * @param trackSid - sid of the track to be muted
   * @param muted - true to mute, false to unmute
   */
  async mutePublishedTrack(
    room: string,
    identity: string,
    trackSid: string,
    muted: boolean,
  ): Promise<TrackInfo> {
    const req = new MuteRoomTrackRequest({
      room,
      identity,
      trackSid,
      muted,
    }).toJson();
    const data = await this.rpc.request(
      svc,
      'MutePublishedTrack',
      req,
      await this.authHeader({ roomAdmin: true, room }),
    );
    const res = MuteRoomTrackResponse.fromJson(data, { ignoreUnknownFields: true });
    return res.track!;
  }

  /**
   * Updates a participant's state or permissions
   * @param room - target room
   * @param identity - participant identity
   * @param options - participant fields to update
   */
  async updateParticipant(
    room: string,
    identity: string,
    options: UpdateParticipantOptions,
  ): Promise<ParticipantInfo>;
  /**
   * Updates a participant's state or permissions
   * @param room - target room
   * @param identity - participant identity
   * @param options - participant fields to update
   */
  async updateParticipant(
    room: string,
    identity: string,
    metadata?: string,
    permission?: Partial<ParticipantPermission>,
    name?: string,
  ): Promise<ParticipantInfo>;
  async updateParticipant(
    room: string,
    identity: string,
    metadataOrOptions?: string | UpdateParticipantOptions,
    maybePermission?: Partial<ParticipantPermission>,
    maybeName?: string,
  ): Promise<ParticipantInfo> {
    const hasOptions = typeof metadataOrOptions === 'object';
    const metadata = hasOptions ? metadataOrOptions?.metadata : metadataOrOptions;
    const permission = hasOptions ? metadataOrOptions.permission : maybePermission;
    const name = hasOptions ? metadataOrOptions.name : maybeName;
    const attributes: Record<string, string> | undefined = hasOptions
      ? metadataOrOptions.attributes
      : {};

    const req = new UpdateParticipantRequest({
      room,
      identity,
      attributes,
      metadata,
      name,
    });
    if (permission) {
      req.permission = new ParticipantPermission(permission);
    }
    const data = await this.rpc.request(
      svc,
      'UpdateParticipant',
      req.toJson(),
      await this.authHeader({ roomAdmin: true, room }),
    );
    return ParticipantInfo.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Updates a participant's subscription to tracks
   * @param room -
   * @param identity -
   * @param trackSids -
   * @param subscribe - true to subscribe, false to unsubscribe
   */
  async updateSubscriptions(
    room: string,
    identity: string,
    trackSids: string[],
    subscribe: boolean,
  ): Promise<void> {
    const req = new UpdateSubscriptionsRequest({
      room,
      identity,
      trackSids,
      subscribe,
      participantTracks: [],
    }).toJson();
    await this.rpc.request(
      svc,
      'UpdateSubscriptions',
      req,
      await this.authHeader({ roomAdmin: true, room }),
    );
  }

  /**
   * Sends data message to participants in the room
   * @param room -
   * @param data - opaque payload to send
   * @param kind - delivery reliability
   * @param options - optionally specify a topic and destinationSids (when destinationSids is empty, message is sent to everyone)
   */
  async sendData(
    room: string,
    data: Uint8Array,
    kind: DataPacket_Kind,
    options: SendDataOptions,
  ): Promise<void>;
  /**
   * Sends data message to participants in the room
   * @deprecated use sendData(room, data, kind, options) instead
   * @param room -
   * @param data - opaque payload to send
   * @param kind - delivery reliability
   * @param destinationSids - optional. when empty, message is sent to everyone
   */
  async sendData(
    room: string,
    data: Uint8Array,
    kind: DataPacket_Kind,
    destinationSids?: string[],
  ): Promise<void>;
  async sendData(
    room: string,
    data: Uint8Array,
    kind: DataPacket_Kind,
    options: SendDataOptions | string[] = {},
  ): Promise<void> {
    const destinationSids = Array.isArray(options) ? options : options.destinationSids;
    const topic = Array.isArray(options) ? undefined : options.topic;
    const req = new SendDataRequest({
      room,
      data,
      kind,
      destinationSids: destinationSids ?? [],
      topic,
    });
    if (!Array.isArray(options) && options.destinationIdentities) {
      req.destinationIdentities = options.destinationIdentities;
    }
    req.nonce = await getRandomBytes(16);
    await this.rpc.request(
      svc,
      'SendData',
      req.toJson(),
      await this.authHeader({ roomAdmin: true, room }),
    );
  }
}
