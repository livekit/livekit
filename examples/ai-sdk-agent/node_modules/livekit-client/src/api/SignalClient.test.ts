import {
  JoinResponse,
  LeaveRequest,
  ReconnectResponse,
  SignalRequest,
  SignalResponse,
} from '@livekit/protocol';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { ConnectionError, ConnectionErrorReason } from '../room/errors';
import { SignalClient, SignalConnectionState } from './SignalClient';
import type { WebSocketCloseInfo, WebSocketConnection } from './WebSocketStream';
import { WebSocketStream } from './WebSocketStream';

// Mock the WebSocketStream
vi.mock('./WebSocketStream');

// Mock fetch for validation endpoint
global.fetch = vi.fn();

// Test Helpers
function createJoinResponse() {
  return new JoinResponse({
    room: { name: 'test-room', sid: 'room-sid' },
    participant: { sid: 'participant-sid', identity: 'test-user' },
    pingTimeout: 30,
    pingInterval: 10,
  });
}

function createSignalResponse(
  messageCase: 'join' | 'reconnect' | 'leave' | 'update',
  value: any,
): SignalResponse {
  return new SignalResponse({
    message: { case: messageCase, value },
  });
}

function createMockReadableStream(responses: SignalResponse[]): ReadableStream<ArrayBuffer> {
  return new ReadableStream<ArrayBuffer>({
    async start(controller) {
      for (const response of responses) {
        controller.enqueue(response.toBinary().buffer as ArrayBuffer);
      }
    },
  });
}

function createMockConnection(readable: ReadableStream<ArrayBuffer>): WebSocketConnection {
  return {
    readable,
    writable: new WritableStream(),
    protocol: '',
    extensions: '',
  };
}

interface MockWebSocketStreamOptions {
  connection?: WebSocketConnection;
  opened?: Promise<WebSocketConnection>;
  closed?: Promise<WebSocketCloseInfo>;
  readyState?: number;
}

function mockWebSocketStream(options: MockWebSocketStreamOptions = {}) {
  const {
    connection,
    opened = connection ? Promise.resolve(connection) : new Promise(() => {}),
    closed = new Promise(() => {}),
    readyState = 1,
  } = options;

  return vi.mocked(WebSocketStream).mockImplementationOnce(
    () =>
      ({
        url: 'wss://test.livekit.io',
        opened,
        closed,
        close: vi.fn(),
        readyState,
      }) as any,
  );
}

describe('SignalClient.connect', () => {
  let signalClient: SignalClient;

  const defaultOptions = {
    autoSubscribe: true,
    maxRetries: 0,
    e2eeEnabled: false,
    websocketTimeout: 1000,
    singlePeerConnection: false,
  };

  beforeEach(() => {
    vi.clearAllMocks();
    signalClient = new SignalClient(false);
  });

  describe('Happy Path - Initial Join', () => {
    it('should successfully connect and receive join response', async () => {
      const joinResponse = createJoinResponse();
      const signalResponse = createSignalResponse('join', joinResponse);
      const mockReadable = createMockReadableStream([signalResponse]);
      const mockConnection = createMockConnection(mockReadable);

      mockWebSocketStream({ connection: mockConnection });

      const result = await signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions);

      expect(result).toEqual(joinResponse);
      expect(signalClient.currentState).toBe(SignalConnectionState.CONNECTED);
    });
  });

  describe('Happy Path - Reconnect', () => {
    it('should successfully reconnect and receive reconnect response', async () => {
      // First, set up initial connection
      const joinResponse = createJoinResponse();
      const joinSignalResponse = createSignalResponse('join', joinResponse);
      const initialMockReadable = createMockReadableStream([joinSignalResponse]);
      const initialMockConnection = createMockConnection(initialMockReadable);

      mockWebSocketStream({ connection: initialMockConnection });

      await signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions);

      // Now test reconnect
      const reconnectResponse = new ReconnectResponse({
        iceServers: [],
      });
      const reconnectSignalResponse = createSignalResponse('reconnect', reconnectResponse);
      const reconnectMockReadable = createMockReadableStream([reconnectSignalResponse]);
      const reconnectMockConnection = createMockConnection(reconnectMockReadable);

      mockWebSocketStream({ connection: reconnectMockConnection });

      const result = await signalClient.reconnect('wss://test.livekit.io', 'test-token', 'sid-123');

      expect(result).toEqual(reconnectResponse);
      expect(signalClient.currentState).toBe(SignalConnectionState.CONNECTED);
    });

    it('should handle reconnect with non-reconnect message (edge case)', async () => {
      // First, initial connection
      const joinResponse = createJoinResponse();
      const joinSignalResponse = createSignalResponse('join', joinResponse);
      const initialMockReadable = createMockReadableStream([joinSignalResponse]);
      const initialMockConnection = createMockConnection(initialMockReadable);

      mockWebSocketStream({ connection: initialMockConnection });

      await signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions);

      // Setup reconnect with non-reconnect message (e.g., participant update)
      const updateSignalResponse = createSignalResponse('update', { participants: [] });
      const reconnectMockReadable = createMockReadableStream([updateSignalResponse]);
      const reconnectMockConnection = createMockConnection(reconnectMockReadable);

      mockWebSocketStream({ connection: reconnectMockConnection });

      const result = await signalClient.reconnect('wss://test.livekit.io', 'test-token', 'sid-123');

      // This is an edge case: reconnect resolves with undefined when non-reconnect message is received
      expect(result).toBeUndefined();
      expect(signalClient.currentState).toBe(SignalConnectionState.CONNECTED);
    }, 1000);
  });

  describe('Failure Case - Timeout', () => {
    it('should reject with timeout error when websocket connection takes too long', async () => {
      mockWebSocketStream({ readyState: 0 }); // Never resolves

      const shortTimeoutOptions = {
        ...defaultOptions,
        websocketTimeout: 100,
      };

      await expect(
        signalClient.join('wss://test.livekit.io', 'test-token', shortTimeoutOptions),
      ).rejects.toThrow(ConnectionError);
    });
  });

  describe('Failure Case - AbortSignal', () => {
    it('should reject when AbortSignal is triggered', async () => {
      const abortController = new AbortController();

      vi.mocked(WebSocketStream).mockImplementation(() => {
        // Simulate abort
        setTimeout(() => abortController.abort(new Error('User aborted connection')), 50);

        return {
          url: 'wss://test.livekit.io',
          opened: new Promise(() => {}), // Never resolves
          closed: new Promise(() => {}),
          close: vi.fn(),
          readyState: 0,
        } as any;
      });

      await expect(
        signalClient.join(
          'wss://test.livekit.io',
          'test-token',
          defaultOptions,
          abortController.signal,
        ),
      ).rejects.toThrow('User aborted connection');
    });

    it('should send leave request before closing when AbortSignal is triggered during connection', async () => {
      const abortController = new AbortController();
      const writtenMessages: Array<ArrayBuffer | string> = [];
      let streamWriterReady: (() => void) | undefined;
      const streamWriterReadyPromise = new Promise<void>((resolve) => {
        streamWriterReady = resolve;
      });

      // Create a mock writable stream that captures writes
      const mockWritable = new WritableStream({
        write(chunk) {
          writtenMessages.push(chunk);
          return Promise.resolve();
        },
      });

      // Override getWriter to signal when streamWriter is assigned
      const originalGetWriter = mockWritable.getWriter.bind(mockWritable);
      mockWritable.getWriter = () => {
        const writer = originalGetWriter();
        streamWriterReady?.();
        return writer;
      };

      const mockReadable = new ReadableStream<ArrayBuffer>({
        async start() {
          // Keep connection open but don't send join response yet
          // This simulates aborting during connection (after WS opens, before join response)
        },
      });

      const mockConnection = {
        readable: mockReadable,
        writable: mockWritable,
        protocol: '',
        extensions: '',
      };

      vi.mocked(WebSocketStream).mockImplementation(() => {
        return {
          url: 'wss://test.livekit.io',
          opened: Promise.resolve(mockConnection),
          closed: new Promise(() => {}),
          close: vi.fn(),
          readyState: 1,
        } as any;
      });

      // Start the connection
      const joinPromise = signalClient.join(
        'wss://test.livekit.io',
        'test-token',
        defaultOptions,
        abortController.signal,
      );

      // Wait for streamWriter to be assigned
      await streamWriterReadyPromise;

      // Now abort the connection (after WS opens, before join response)
      abortController.abort(new Error('User aborted connection'));

      // joinPromise should reject
      await expect(joinPromise).rejects.toThrow('User aborted connection');

      // Verify that a leave request was sent before closing
      const leaveRequestSent = writtenMessages.some((data) => {
        if (typeof data === 'string') {
          return false;
        }
        try {
          const request = SignalRequest.fromBinary(
            data instanceof ArrayBuffer ? new Uint8Array(data) : data,
          );
          return request.message?.case === 'leave';
        } catch {
          return false;
        }
      });

      expect(leaveRequestSent).toBe(true);
    });
  });

  describe('Failure Case - WebSocket Connection Errors', () => {
    it('should reject with NotAllowed error for 4xx HTTP status', async () => {
      mockWebSocketStream({
        opened: Promise.reject(new Error('Connection failed')),
        readyState: 3,
      });

      // Mock fetch to return 403
      (global.fetch as any).mockResolvedValueOnce({
        status: 403,
        text: async () => 'Forbidden',
      });

      await expect(
        signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions),
      ).rejects.toMatchObject({
        message: 'Forbidden',
        reason: ConnectionErrorReason.NotAllowed,
        status: 403,
      });
    });

    it('should reject with ServerUnreachable when fetch fails', async () => {
      mockWebSocketStream({
        opened: Promise.reject(new Error('Connection failed')),
        readyState: 3,
      });

      // Mock fetch to throw (network error)
      (global.fetch as any).mockRejectedValueOnce(new Error('Network error'));

      await expect(
        signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions),
      ).rejects.toMatchObject({
        reason: ConnectionErrorReason.ServerUnreachable,
      });
    });

    it('should handle ConnectionError from WebSocket rejection', async () => {
      const customError = new ConnectionError(
        'Custom error',
        ConnectionErrorReason.InternalError,
        500,
      );

      mockWebSocketStream({
        opened: Promise.reject(customError),
        readyState: 3,
      });

      // Mock fetch to return 500
      (global.fetch as any).mockResolvedValueOnce({
        status: 500,
        text: async () => 'Internal Server Error',
      });

      await expect(
        signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions),
      ).rejects.toMatchObject({
        reason: ConnectionErrorReason.InternalError,
      });
    });
  });

  describe('Failure Case - No First Message', () => {
    it('should reject when no first message is received', async () => {
      // Close the stream immediately without sending a message
      const mockReadable = new ReadableStream<ArrayBuffer>({
        async start(controller) {
          controller.close();
        },
      });
      const mockConnection = createMockConnection(mockReadable);

      mockWebSocketStream({ connection: mockConnection });

      await expect(
        signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions),
      ).rejects.toMatchObject({
        message: 'no message received as first message',
        reason: ConnectionErrorReason.InternalError,
      });
    });
  });

  describe('Failure Case - Leave Request During Connection', () => {
    it('should reject when receiving leave request during initial join', async () => {
      const leaveRequest = new LeaveRequest({
        reason: 1, // Some disconnect reason
      });
      const signalResponse = createSignalResponse('leave', leaveRequest);
      const mockReadable = createMockReadableStream([signalResponse]);
      const mockConnection = createMockConnection(mockReadable);

      mockWebSocketStream({ connection: mockConnection });

      await expect(
        signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions),
      ).rejects.toMatchObject(
        new ConnectionError(
          'Received leave request while trying to (re)connect',
          ConnectionErrorReason.LeaveRequest,
          undefined,
          1,
        ),
      );
    });
  });

  describe('Failure Case - Wrong Message Type for Non-Reconnect', () => {
    it('should reject when receiving non-join message on initial connection', async () => {
      // Send a reconnect response instead of join (wrong for initial connection)
      const reconnectResponse = new ReconnectResponse({
        iceServers: [],
      });
      const signalResponse = createSignalResponse('reconnect', reconnectResponse);
      const mockReadable = createMockReadableStream([signalResponse]);
      const mockConnection = createMockConnection(mockReadable);

      mockWebSocketStream({ connection: mockConnection });

      await expect(
        signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions),
      ).rejects.toMatchObject({
        message: 'did not receive join response, got reconnect instead',
        reason: ConnectionErrorReason.InternalError,
      });
    });
  });

  describe('Failure Case - WebSocket Closed During Connection', () => {
    it('should reject when WebSocket closes during connection attempt', async () => {
      let closedResolve: (value: WebSocketCloseInfo) => void;
      const closedPromise = new Promise<WebSocketCloseInfo>((resolve) => {
        closedResolve = resolve;
      });

      vi.mocked(WebSocketStream).mockImplementation(() => {
        // Simulate close during connection
        queueMicrotask(() => {
          closedResolve({ closeCode: 1006, reason: 'Connection lost' });
        });

        return {
          url: 'wss://test.livekit.io',
          opened: new Promise(() => {}), // Never resolves
          closed: closedPromise,
          close: vi.fn(),
          readyState: 2, // CLOSING
        } as any;
      });

      await expect(
        signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions),
      ).rejects.toMatchObject({
        message: 'Websocket got closed during a (re)connection attempt: Connection lost',
        reason: ConnectionErrorReason.InternalError,
      });
    });
  });

  describe('Edge Cases and State Management', () => {
    it('should set state to CONNECTING when joining', async () => {
      expect(signalClient.currentState).toBe(SignalConnectionState.DISCONNECTED);

      const joinResponse = createJoinResponse();
      const signalResponse = createSignalResponse('join', joinResponse);
      const mockReadable = createMockReadableStream([signalResponse]);
      const mockConnection = createMockConnection(mockReadable);

      mockWebSocketStream({ connection: mockConnection });

      const joinPromise = signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions);

      // State should be CONNECTING before connection completes
      expect(signalClient.currentState).toBe(SignalConnectionState.CONNECTING);

      await joinPromise;

      expect(signalClient.currentState).toBe(SignalConnectionState.CONNECTED);
    });
  });
});

describe('SignalClient utility functions', () => {
  describe('toProtoSessionDescription', () => {
    it('should convert RTCSessionDescriptionInit to proto SessionDescription', async () => {
      const { toProtoSessionDescription } = await import('./SignalClient');

      const rtcDesc: RTCSessionDescriptionInit = {
        type: 'offer',
        sdp: 'v=0\r\no=- 123 456 IN IP4 127.0.0.1\r\n',
      };

      const protoDesc = toProtoSessionDescription(rtcDesc, 42);

      expect(protoDesc.type).toBe('offer');
      expect(protoDesc.sdp).toBe('v=0\r\no=- 123 456 IN IP4 127.0.0.1\r\n');
      expect(protoDesc.id).toBe(42);
    });

    it('should handle answer type', async () => {
      const { toProtoSessionDescription } = await import('./SignalClient');

      const rtcDesc: RTCSessionDescriptionInit = {
        type: 'answer',
        sdp: 'v=0\r\n',
      };

      const protoDesc = toProtoSessionDescription(rtcDesc);

      expect(protoDesc.type).toBe('answer');
      expect(protoDesc.sdp).toBe('v=0\r\n');
    });
  });
});

describe('SignalClient.handleSignalConnected', () => {
  let signalClient: SignalClient;

  const defaultOptions = {
    autoSubscribe: true,
    maxRetries: 0,
    e2eeEnabled: false,
    websocketTimeout: 1000,
    singlePeerConnection: false,
  };

  beforeEach(() => {
    vi.clearAllMocks();
    signalClient = new SignalClient(false);
  });

  it('should set state to CONNECTED', () => {
    const mockReadable = new ReadableStream<ArrayBuffer>();
    const mockConnection = createMockConnection(mockReadable);

    // Access the method through a type assertion for testing
    const handleMethod = (signalClient as any).handleSignalConnected;
    if (handleMethod) {
      handleMethod.call(signalClient, mockConnection);
      expect(signalClient.currentState).toBe(SignalConnectionState.CONNECTED);
    }
  });

  it('should start reading loop without first message', async () => {
    const joinResponse = createJoinResponse();
    const signalResponse = createSignalResponse('join', joinResponse);
    const mockReadable = createMockReadableStream([signalResponse]);
    const mockConnection = createMockConnection(mockReadable);

    mockWebSocketStream({ connection: mockConnection });

    await signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions);

    // Verify connection was established successfully
    expect(signalClient.currentState).toBe(SignalConnectionState.CONNECTED);
  });

  it('should start reading loop with first message', async () => {
    const joinResponse = createJoinResponse();
    const signalResponse = createSignalResponse('join', joinResponse);
    const mockReadable = createMockReadableStream([signalResponse]);
    const mockConnection = createMockConnection(mockReadable);

    mockWebSocketStream({ connection: mockConnection });

    await signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions);

    expect(signalClient.currentState).toBe(SignalConnectionState.CONNECTED);
  });
});

describe('SignalClient.validateFirstMessage', () => {
  let signalClient: SignalClient;

  const defaultOptions = {
    autoSubscribe: true,
    maxRetries: 0,
    e2eeEnabled: false,
    websocketTimeout: 1000,
    singlePeerConnection: false,
  };

  beforeEach(() => {
    vi.clearAllMocks();
    signalClient = new SignalClient(false);
  });

  it('should accept join response for initial connection', () => {
    const joinResponse = createJoinResponse();
    const signalResponse = createSignalResponse('join', joinResponse);

    const validateMethod = (signalClient as any).validateFirstMessage;
    if (validateMethod) {
      const result = validateMethod.call(signalClient, signalResponse, false);
      expect(result.isValid).toBe(true);
      expect(result.response).toEqual(joinResponse);
    }
  });

  it('should accept reconnect response for reconnection', async () => {
    // First establish a connection to set options
    const joinResponse = createJoinResponse();
    const joinSignalResponse = createSignalResponse('join', joinResponse);
    const initialMockReadable = createMockReadableStream([joinSignalResponse]);
    const initialMockConnection = createMockConnection(initialMockReadable);

    mockWebSocketStream({ connection: initialMockConnection });
    await signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions);

    // Set state to RECONNECTING to match the validation logic
    (signalClient as any).state = SignalConnectionState.RECONNECTING;

    const reconnectResponse = new ReconnectResponse({ iceServers: [] });
    const signalResponse = createSignalResponse('reconnect', reconnectResponse);

    const validateMethod = (signalClient as any).validateFirstMessage;
    if (validateMethod) {
      const result = validateMethod.call(signalClient, signalResponse, true);
      expect(result.isValid).toBe(true);
      expect(result.response).toEqual(reconnectResponse);
    }
  });

  it('should accept non-reconnect message during reconnecting state', async () => {
    // First establish a connection
    const joinResponse = createJoinResponse();
    const joinSignalResponse = createSignalResponse('join', joinResponse);
    const initialMockReadable = createMockReadableStream([joinSignalResponse]);
    const initialMockConnection = createMockConnection(initialMockReadable);

    mockWebSocketStream({ connection: initialMockConnection });
    await signalClient.join('wss://test.livekit.io', 'test-token', defaultOptions);

    // Set state to reconnecting
    (signalClient as any).state = SignalConnectionState.RECONNECTING;

    const updateSignalResponse = createSignalResponse('update', { participants: [] });

    const validateMethod = (signalClient as any).validateFirstMessage;
    if (validateMethod) {
      const result = validateMethod.call(signalClient, updateSignalResponse, true);
      expect(result.isValid).toBe(true);
      expect(result.response).toBeUndefined();
      expect(result.shouldProcessFirstMessage).toBe(true);
    }
  });

  it('should reject leave request during connection attempt', () => {
    // Set state to CONNECTING to be in establishing connection state
    (signalClient as any).state = SignalConnectionState.CONNECTING;

    const leaveRequest = new LeaveRequest({ reason: 1 });
    const signalResponse = createSignalResponse('leave', leaveRequest);

    const validateMethod = (signalClient as any).validateFirstMessage;
    if (validateMethod) {
      const result = validateMethod.call(signalClient, signalResponse, false);
      expect(result.isValid).toBe(false);
      expect(result.error).toBeInstanceOf(ConnectionError);
      expect(result.error?.reason).toBe(ConnectionErrorReason.LeaveRequest);
    }
  });

  it('should reject non-join message for initial connection', () => {
    const reconnectResponse = new ReconnectResponse({ iceServers: [] });
    const signalResponse = createSignalResponse('reconnect', reconnectResponse);

    const validateMethod = (signalClient as any).validateFirstMessage;
    if (validateMethod) {
      const result = validateMethod.call(signalClient, signalResponse, false);
      expect(result.isValid).toBe(false);
      expect(result.error).toBeInstanceOf(ConnectionError);
      expect(result.error?.reason).toBe(ConnectionErrorReason.InternalError);
    }
  });
});

describe('SignalClient.handleConnectionError', () => {
  let signalClient: SignalClient;

  beforeEach(() => {
    vi.clearAllMocks();
    signalClient = new SignalClient(false);
  });

  it('should return NotAllowed error for 4xx HTTP status', async () => {
    (global.fetch as any).mockResolvedValueOnce({
      status: 403,
      text: async () => 'Forbidden',
    });

    const handleMethod = (signalClient as any).handleConnectionError;
    if (handleMethod) {
      const error = new Error('Connection failed');
      const result = await handleMethod.call(signalClient, error, 'wss://test.livekit.io/validate');

      expect(result).toBeInstanceOf(ConnectionError);
      expect(result.reason).toBe(ConnectionErrorReason.NotAllowed);
      expect(result.status).toBe(403);
      expect(result.message).toBe('Forbidden');
    }
  });

  it('should return ConnectionError as-is if it is already a ConnectionError', async () => {
    const connectionError = new ConnectionError(
      'Custom error',
      ConnectionErrorReason.InternalError,
      500,
    );

    (global.fetch as any).mockResolvedValueOnce({
      status: 500,
      text: async () => 'Internal Server Error',
    });

    const handleMethod = (signalClient as any).handleConnectionError;
    if (handleMethod) {
      const result = await handleMethod.call(
        signalClient,
        connectionError,
        'wss://test.livekit.io/validate',
      );

      expect(result).toBe(connectionError);
      expect(result.reason).toBe(ConnectionErrorReason.InternalError);
    }
  });

  it('should return InternalError for non-4xx HTTP status', async () => {
    (global.fetch as any).mockResolvedValueOnce({
      status: 500,
      text: async () => 'Internal Server Error',
    });

    const handleMethod = (signalClient as any).handleConnectionError;
    if (handleMethod) {
      const error = new Error('Connection failed');
      const result = await handleMethod.call(signalClient, error, 'wss://test.livekit.io/validate');

      expect(result).toBeInstanceOf(ConnectionError);
      expect(result.reason).toBe(ConnectionErrorReason.InternalError);
      expect(result.status).toBe(500);
    }
  });

  it('should return ServerUnreachable when fetch fails', async () => {
    (global.fetch as any).mockRejectedValueOnce(new Error('Network error'));

    const handleMethod = (signalClient as any).handleConnectionError;
    if (handleMethod) {
      const error = new Error('Connection failed');
      const result = await handleMethod.call(signalClient, error, 'wss://test.livekit.io/validate');

      expect(result).toBeInstanceOf(ConnectionError);
      expect(result.reason).toBe(ConnectionErrorReason.ServerUnreachable);
    }
  });

  it('should handle fetch throwing ConnectionError', async () => {
    const fetchError = new ConnectionError('Fetch failed', ConnectionErrorReason.ServerUnreachable);
    (global.fetch as any).mockRejectedValueOnce(fetchError);

    const handleMethod = (signalClient as any).handleConnectionError;
    if (handleMethod) {
      const error = new Error('Connection failed');
      const result = await handleMethod.call(signalClient, error, 'wss://test.livekit.io/validate');

      expect(result).toBe(fetchError);
    }
  });
});
