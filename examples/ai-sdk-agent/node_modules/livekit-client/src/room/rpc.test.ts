import { DataPacket, DataPacket_Kind } from '@livekit/protocol';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { InternalRoomOptions } from '../options';
import type RTCEngine from './RTCEngine';
import Room from './Room';
import LocalParticipant from './participant/LocalParticipant';
import { ParticipantKind } from './participant/Participant';
import RemoteParticipant from './participant/RemoteParticipant';
import { RpcError } from './rpc';

describe('LocalParticipant', () => {
  describe('registerRpcMethod', () => {
    let room: Room;
    let mockSendDataPacket: ReturnType<typeof vi.fn>;

    beforeEach(() => {
      mockSendDataPacket = vi.fn();

      room = new Room();
      room.engine.client = {
        sendUpdateLocalMetadata: vi.fn(),
      };
      room.engine.on = vi.fn().mockReturnThis();
      room.engine.sendDataPacket = mockSendDataPacket;

      room.localParticipant.sid = 'test-sid';
      room.localParticipant.identity = 'test-identity';
    });

    it('should register an RPC method handler', async () => {
      const methodName = 'testMethod';
      const handler = vi.fn().mockResolvedValue('test response');

      room.registerRpcMethod(methodName, handler);

      const mockCaller = new RemoteParticipant(
        {} as any,
        'remote-sid',
        'remote-identity',
        'Remote Participant',
        '',
        undefined,
        undefined,
        ParticipantKind.STANDARD,
      );

      await room.handleIncomingRpcRequest(
        mockCaller.identity,
        'test-request-id',
        methodName,
        'test payload',
        5000,
        1,
      );

      expect(handler).toHaveBeenCalledWith({
        requestId: 'test-request-id',
        callerIdentity: mockCaller.identity,
        payload: 'test payload',
        responseTimeout: 5000,
      });

      // Check if sendDataPacket was called twice (once for ACK and once for response)
      expect(mockSendDataPacket).toHaveBeenCalledTimes(2);

      // Check if the first call was for ACK
      expect(mockSendDataPacket.mock.calls[0][0].value.case).toBe('rpcAck');
      expect(mockSendDataPacket.mock.calls[0][1]).toBe(DataPacket_Kind.RELIABLE);

      // Check if the second call was for response
      expect(mockSendDataPacket.mock.calls[1][0].value.case).toBe('rpcResponse');
      expect(mockSendDataPacket.mock.calls[1][1]).toBe(DataPacket_Kind.RELIABLE);
    });

    it('should catch and transform unhandled errors in the RPC method handler', async () => {
      const methodName = 'errorMethod';
      const errorMessage = 'Test error';
      const handler = vi.fn().mockRejectedValue(new Error(errorMessage));

      room.registerRpcMethod(methodName, handler);

      const mockCaller = new RemoteParticipant(
        {} as any,
        'remote-sid',
        'remote-identity',
        'Remote Participant',
        '',
        undefined,
        undefined,
        ParticipantKind.STANDARD,
      );

      await room.handleIncomingRpcRequest(
        mockCaller.identity,
        'test-error-request-id',
        methodName,
        'test payload',
        5000,
        1,
      );

      expect(handler).toHaveBeenCalledWith({
        requestId: 'test-error-request-id',
        callerIdentity: mockCaller.identity,
        payload: 'test payload',
        responseTimeout: 5000,
      });

      // Check if sendDataPacket was called twice (once for ACK and once for error response)
      expect(mockSendDataPacket).toHaveBeenCalledTimes(2);

      // Check if the second call was for error response
      const errorResponse = mockSendDataPacket.mock.calls[1][0].value.value.value.value;
      expect(errorResponse.code).toBe(RpcError.ErrorCode.APPLICATION_ERROR);
    });

    it('should pass through RpcError thrown by the RPC method handler', async () => {
      const methodName = 'rpcErrorMethod';
      const errorCode = 101;
      const errorMessage = 'some-error-message';
      const handler = vi.fn().mockRejectedValue(new RpcError(errorCode, errorMessage));

      room.localParticipant.registerRpcMethod(methodName, handler);

      const mockCaller = new RemoteParticipant(
        {} as any,
        'remote-sid',
        'remote-identity',
        'Remote Participant',
        '',
        undefined,
        undefined,
        ParticipantKind.STANDARD,
      );

      await room.handleIncomingRpcRequest(
        mockCaller.identity,
        'test-rpc-error-request-id',
        methodName,
        'test payload',
        5000,
        1,
      );

      expect(handler).toHaveBeenCalledWith({
        requestId: 'test-rpc-error-request-id',
        callerIdentity: mockCaller.identity,
        payload: 'test payload',
        responseTimeout: 5000,
      });

      // Check if sendDataPacket was called twice (once for ACK and once for error response)
      expect(mockSendDataPacket).toHaveBeenCalledTimes(2);

      // Check if the second call was for error response
      const errorResponse = mockSendDataPacket.mock.calls[1][0].value.value.value.value;
      expect(errorResponse.code).toBe(errorCode);
      expect(errorResponse.message).toBe(errorMessage);
    });
  });

  describe('performRpc', () => {
    let localParticipant: LocalParticipant;
    let mockRemoteParticipant: RemoteParticipant;
    let mockEngine: RTCEngine;
    let mockRoomOptions: InternalRoomOptions;
    let mockSendDataPacket: ReturnType<typeof vi.fn>;

    beforeEach(() => {
      mockSendDataPacket = vi.fn();
      mockEngine = {
        client: {
          sendUpdateLocalMetadata: vi.fn(),
        },
        on: vi.fn().mockReturnThis(),
        sendDataPacket: mockSendDataPacket,
      } as unknown as RTCEngine;

      mockRoomOptions = {} as InternalRoomOptions;

      localParticipant = new LocalParticipant(
        'local-sid',
        'local-identity',
        mockEngine,
        mockRoomOptions,
      );

      mockRemoteParticipant = new RemoteParticipant(
        {} as any,
        'remote-sid',
        'remote-identity',
        'Remote Participant',
        '',
        undefined,
        undefined,
        ParticipantKind.STANDARD,
      );
    });

    it('should send RPC request and receive successful response', async () => {
      const method = 'testMethod';
      const payload = 'testPayload';
      const responsePayload = 'responsePayload';

      mockSendDataPacket.mockImplementationOnce((packet: DataPacket) => {
        const requestId = packet.value.value.id;
        setTimeout(() => {
          localParticipant.handleIncomingRpcAck(requestId);
          setTimeout(() => {
            localParticipant.handleIncomingRpcResponse(requestId, responsePayload, null);
          }, 10);
        }, 10);
      });

      const result = await localParticipant.performRpc({
        destinationIdentity: mockRemoteParticipant.identity,
        method,
        payload,
      });

      expect(mockSendDataPacket).toHaveBeenCalledTimes(1);
      expect(result).toBe(responsePayload);
    });

    it('should handle RPC request timeout', async () => {
      const method = 'timeoutMethod';
      const payload = 'timeoutPayload';

      const timeout = 50;

      const resultPromise = localParticipant.performRpc({
        destinationIdentity: mockRemoteParticipant.identity,
        method,
        payload,
        responseTimeout: timeout,
      });

      mockSendDataPacket.mockImplementationOnce(() => {
        return new Promise((resolve) => {
          setTimeout(resolve, timeout + 10);
        });
      });

      const startTime = Date.now();

      await expect(resultPromise).rejects.toThrow('Response timeout');

      const elapsedTime = Date.now() - startTime;
      expect(elapsedTime).toBeGreaterThanOrEqual(timeout);
      expect(elapsedTime).toBeLessThan(timeout + 50); // Allow some margin for test execution

      expect(mockSendDataPacket).toHaveBeenCalledTimes(1);
    });

    it('should handle RPC error response', async () => {
      const method = 'errorMethod';
      const payload = 'errorPayload';
      const errorCode = 101;
      const errorMessage = 'Test error message';

      mockSendDataPacket.mockImplementationOnce((packet: DataPacket) => {
        const requestId = packet.value.value.id;
        setTimeout(() => {
          localParticipant.handleIncomingRpcAck(requestId);
          localParticipant.handleIncomingRpcResponse(
            requestId,
            null,
            new RpcError(errorCode, errorMessage),
          );
        }, 10);
      });

      await expect(
        localParticipant.performRpc({
          destinationIdentity: mockRemoteParticipant.identity,
          method,
          payload,
        }),
      ).rejects.toThrow(errorMessage);
    });

    it('should handle participant disconnection during RPC request', async () => {
      const method = 'disconnectMethod';
      const payload = 'disconnectPayload';

      mockSendDataPacket.mockImplementationOnce(() => Promise.resolve());

      const resultPromise = localParticipant.performRpc({
        destinationIdentity: mockRemoteParticipant.identity,
        method,
        payload,
      });

      // Simulate a small delay before disconnection
      await new Promise((resolve) => setTimeout(resolve, 200));
      localParticipant.handleParticipantDisconnected(mockRemoteParticipant.identity);

      await expect(resultPromise).rejects.toThrow('Recipient disconnected');
    });
  });
});
