import { createLocalAudioTrack } from '../../room/track/create';
import { detectSilence } from '../../room/track/utils';
import { Checker } from './Checker';

export class PublishAudioCheck extends Checker {
  get description(): string {
    return 'Can publish audio';
  }

  async perform(): Promise<void> {
    const room = await this.connect();

    const track = await createLocalAudioTrack();

    const trackIsSilent = await detectSilence(track, 1000);
    if (trackIsSilent) {
      throw new Error('unable to detect audio from microphone');
    }
    this.appendMessage('detected audio from microphone');

    room.localParticipant.publishTrack(track);
    // wait for a few seconds to publish
    await new Promise((resolve) => setTimeout(resolve, 3000));

    // verify RTC stats that it's publishing
    const stats = await track.sender?.getStats();
    if (!stats) {
      throw new Error('Could not get RTCStats');
    }
    let numPackets = 0;
    stats.forEach((stat) => {
      if (
        stat.type === 'outbound-rtp' &&
        (stat.kind === 'audio' || (!stat.kind && stat.mediaType === 'audio'))
      ) {
        numPackets = stat.packetsSent;
      }
    });
    if (numPackets === 0) {
      throw new Error('Could not determine packets are sent');
    }
    this.appendMessage(`published ${numPackets} audio packets`);
  }
}
