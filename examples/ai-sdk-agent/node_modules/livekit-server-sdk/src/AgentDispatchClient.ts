// SPDX-FileCopyrightText: 2024 LiveKit, Inc.
//
// SPDX-License-Identifier: Apache-2.0
import {
  AgentDispatch,
  CreateAgentDispatchRequest,
  DeleteAgentDispatchRequest,
  ListAgentDispatchRequest,
  ListAgentDispatchResponse,
} from '@livekit/protocol';
import { ServiceBase } from './ServiceBase.js';
import { type Rpc, TwirpRpc, livekitPackage } from './TwirpRPC.js';

interface CreateDispatchOptions {
  // any custom data to send along with the job.
  // note: this is different from room and participant metadata
  metadata?: string;
}

const svc = 'AgentDispatchService';

/**
 * Client to access Agent APIs
 */
export class AgentDispatchClient extends ServiceBase {
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
   * Create an explicit dispatch for an agent to join a room. To use explicit
   * dispatch, your agent must be registered with an `agentName`.
   * @param roomName - name of the room to dispatch to
   * @param agentName - name of the agent to dispatch
   * @param options - optional metadata to send along with the dispatch
   * @returns the dispatch that was created
   */
  async createDispatch(
    roomName: string,
    agentName: string,
    options?: CreateDispatchOptions,
  ): Promise<AgentDispatch> {
    const req = new CreateAgentDispatchRequest({
      room: roomName,
      agentName,
      metadata: options?.metadata,
    }).toJson();
    const data = await this.rpc.request(
      svc,
      'CreateDispatch',
      req,
      await this.authHeader({ roomAdmin: true, room: roomName }),
    );
    return AgentDispatch.fromJson(data, { ignoreUnknownFields: true });
  }

  /**
   * Delete an explicit dispatch for an agent in a room.
   * @param dispatchId - id of the dispatch to delete
   * @param roomName - name of the room the dispatch is for
   */
  async deleteDispatch(dispatchId: string, roomName: string): Promise<void> {
    const req = new DeleteAgentDispatchRequest({
      dispatchId,
      room: roomName,
    }).toJson();
    await this.rpc.request(
      svc,
      'DeleteDispatch',
      req,
      await this.authHeader({ roomAdmin: true, room: roomName }),
    );
  }

  /**
   * Get an Agent dispatch by ID
   * @param dispatchId - id of the dispatch to get
   * @param roomName - name of the room the dispatch is for
   * @returns the dispatch that was found, or undefined if not found
   */
  async getDispatch(dispatchId: string, roomName: string): Promise<AgentDispatch | undefined> {
    const req = new ListAgentDispatchRequest({
      dispatchId,
      room: roomName,
    }).toJson();
    const data = await this.rpc.request(
      svc,
      'ListDispatch',
      req,
      await this.authHeader({ roomAdmin: true, room: roomName }),
    );
    const res = ListAgentDispatchResponse.fromJson(data, { ignoreUnknownFields: true });
    if (res.agentDispatches.length === 0) {
      return undefined;
    }
    return res.agentDispatches[0];
  }

  /**
   * List all agent dispatches for a room
   * @param roomName - name of the room to list dispatches for
   * @returns the list of dispatches
   */
  async listDispatch(roomName: string): Promise<AgentDispatch[]> {
    const req = new ListAgentDispatchRequest({
      room: roomName,
    }).toJson();
    const data = await this.rpc.request(
      svc,
      'ListDispatch',
      req,
      await this.authHeader({ roomAdmin: true, room: roomName }),
    );
    const res = ListAgentDispatchResponse.fromJson(data, { ignoreUnknownFields: true });
    return res.agentDispatches;
  }
}
