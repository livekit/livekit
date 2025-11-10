import { SignalClient } from '../../api/SignalClient';
import { Checker } from './Checker';

export class TURNCheck extends Checker {
  get description(): string {
    return 'Can connect via TURN';
  }

  async perform(): Promise<void> {
    const signalClient = new SignalClient();
    const joinRes = await signalClient.join(this.url, this.token, {
      autoSubscribe: true,
      maxRetries: 0,
      e2eeEnabled: false,
      websocketTimeout: 15_000,
      singlePeerConnection: false,
    });

    let hasTLS = false;
    let hasTURN = false;
    let hasSTUN = false;

    for (let iceServer of joinRes.iceServers) {
      for (let url of iceServer.urls) {
        if (url.startsWith('turn:')) {
          hasTURN = true;
          hasSTUN = true;
        } else if (url.startsWith('turns:')) {
          hasTURN = true;
          hasSTUN = true;
          hasTLS = true;
        }
        if (url.startsWith('stun:')) {
          hasSTUN = true;
        }
      }
    }
    if (!hasSTUN) {
      this.appendWarning('No STUN servers configured on server side.');
    } else if (hasTURN && !hasTLS) {
      this.appendWarning('TURN is configured server side, but TURN/TLS is unavailable.');
    }
    await signalClient.close();
    if (this.connectOptions?.rtcConfig?.iceServers || hasTURN) {
      await this.room!.connect(this.url, this.token, {
        rtcConfig: {
          iceTransportPolicy: 'relay',
        },
      });
    } else {
      this.appendWarning('No TURN servers configured.');
      this.skip();
      await new Promise((resolve) => setTimeout(resolve, 0));
    }
  }
}
