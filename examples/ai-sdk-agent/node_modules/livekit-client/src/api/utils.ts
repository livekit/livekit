import { SignalResponse } from '@livekit/protocol';
import { toHttpUrl, toWebsocketUrl } from '../room/utils';

export function createRtcUrl(url: string, searchParams: URLSearchParams) {
  const urlObj = new URL(toWebsocketUrl(url));
  searchParams.forEach((value, key) => {
    urlObj.searchParams.set(key, value);
  });
  return appendUrlPath(urlObj, 'rtc');
}

export function createValidateUrl(rtcWsUrl: string) {
  const urlObj = new URL(toHttpUrl(rtcWsUrl));
  return appendUrlPath(urlObj, 'validate');
}

function ensureTrailingSlash(path: string) {
  return path.endsWith('/') ? path : `${path}/`;
}

function appendUrlPath(urlObj: URL, path: string) {
  urlObj.pathname = `${ensureTrailingSlash(urlObj.pathname)}${path}`;
  return urlObj.toString();
}

export function parseSignalResponse(value: ArrayBuffer | string) {
  if (typeof value === 'string') {
    return SignalResponse.fromJson(JSON.parse(value), { ignoreUnknownFields: true });
  } else if (value instanceof ArrayBuffer) {
    return SignalResponse.fromBinary(new Uint8Array(value));
  }
  throw new Error(`could not decode websocket message: ${typeof value}`);
}

export function getAbortReasonAsString(
  signal: AbortSignal | Error | unknown,
  defaultMessage = 'Unknown reason',
) {
  if (!(signal instanceof AbortSignal)) {
    return defaultMessage;
  }
  const reason = signal.reason;
  switch (typeof reason) {
    case 'string':
      return reason;
    case 'object':
      return reason instanceof Error ? reason.message : defaultMessage;
    default:
      return 'toString' in reason ? reason.toString() : defaultMessage;
  }
}
