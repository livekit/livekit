import { ServerInfo_Edition } from '@livekit/protocol';
import { SignalClient } from '../../api/SignalClient';
import { Checker } from './Checker';

export class WebSocketCheck extends Checker {
  get description(): string {
    return 'Connecting to signal connection via WebSocket';
  }

  protected async perform(): Promise<void> {
    if (this.url.startsWith('ws:') || this.url.startsWith('http:')) {
      this.appendWarning('Server is insecure, clients may block connections to it');
    }

    let signalClient = new SignalClient();
    const joinRes = await signalClient.join(this.url, this.token, {
      autoSubscribe: true,
      maxRetries: 0,
      e2eeEnabled: false,
      websocketTimeout: 15_000,
      singlePeerConnection: false,
    });
    this.appendMessage(`Connected to server, version ${joinRes.serverVersion}.`);
    if (joinRes.serverInfo?.edition === ServerInfo_Edition.Cloud && joinRes.serverInfo?.region) {
      this.appendMessage(`LiveKit Cloud: ${joinRes.serverInfo?.region}`);
    }
    await signalClient.close();
  }
}
