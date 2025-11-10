import log from '../../logger';
import { RoomEvent } from '../../room/events';
import { Checker } from './Checker';

export class WebRTCCheck extends Checker {
  get description(): string {
    return 'Establishing WebRTC connection';
  }

  protected async perform(): Promise<void> {
    let hasTcp = false;
    let hasIpv4Udp = false;
    this.room.on(RoomEvent.SignalConnected, () => {
      const prevTrickle = this.room.engine.client.onTrickle;

      const candidates: RTCIceCandidate[] = [];
      this.room.engine.client.onTrickle = (sd, target) => {
        if (sd.candidate) {
          const candidate = new RTCIceCandidate(sd);
          candidates.push(candidate);
          let str = `${candidate.protocol} ${candidate.address}:${candidate.port} ${candidate.type}`;
          if (candidate.address) {
            if (isIPPrivate(candidate.address)) {
              str += ' (private)';
            } else {
              if (candidate.protocol === 'tcp' && candidate.tcpType === 'passive') {
                hasTcp = true;
                str += ' (passive)';
              } else if (candidate.protocol === 'udp') {
                hasIpv4Udp = true;
              }
            }
          }
          this.appendMessage(str);
        }
        if (prevTrickle) {
          prevTrickle(sd, target);
        }
      };

      if (this.room.engine.pcManager?.subscriber) {
        this.room.engine.pcManager.subscriber.onIceCandidateError = (ev) => {
          if (ev instanceof RTCPeerConnectionIceErrorEvent) {
            this.appendWarning(
              `error with ICE candidate: ${ev.errorCode} ${ev.errorText} ${ev.url}`,
            );
          }
        };
      }
    });
    try {
      await this.connect();
      log.info('now the room is connected');
    } catch (err) {
      this.appendWarning('ports need to be open on firewall in order to connect.');
      throw err;
    }
    if (!hasTcp) {
      this.appendWarning('Server is not configured for ICE/TCP');
    }
    if (!hasIpv4Udp) {
      this.appendWarning(
        'No public IPv4 UDP candidates were found. Your server is likely not configured correctly',
      );
    }
  }
}

function isIPPrivate(address: string): boolean {
  const parts = address.split('.');
  if (parts.length === 4) {
    if (parts[0] === '10') {
      return true;
    } else if (parts[0] === '192' && parts[1] === '168') {
      return true;
    } else if (parts[0] === '172') {
      const second = parseInt(parts[1], 10);
      if (second >= 16 && second <= 31) {
        return true;
      }
    }
  }
  return false;
}
