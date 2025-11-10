import { createLocalVideoTrack } from '../../room/track/create';
import { Checker } from './Checker';

export class PublishVideoCheck extends Checker {
  get description(): string {
    return 'Can publish video';
  }

  async perform(): Promise<void> {
    const room = await this.connect();

    const track = await createLocalVideoTrack();

    // check if we have video from camera
    await this.checkForVideo(track.mediaStreamTrack);

    room.localParticipant.publishTrack(track);
    // wait for a few seconds to publish
    await new Promise((resolve) => setTimeout(resolve, 5000));

    // verify RTC stats that it's publishing
    const stats = await track.sender?.getStats();
    if (!stats) {
      throw new Error('Could not get RTCStats');
    }
    let numPackets = 0;
    stats.forEach((stat) => {
      if (
        stat.type === 'outbound-rtp' &&
        (stat.kind === 'video' || (!stat.kind && stat.mediaType === 'video'))
      ) {
        numPackets += stat.packetsSent;
      }
    });
    if (numPackets === 0) {
      throw new Error('Could not determine packets are sent');
    }
    this.appendMessage(`published ${numPackets} video packets`);
  }

  async checkForVideo(track: MediaStreamTrack) {
    const stream = new MediaStream();
    stream.addTrack(track.clone());

    // Create video element to check frames
    const video = document.createElement('video');
    video.srcObject = stream;
    video.muted = true;
    video.autoplay = true;
    video.playsInline = true;
    // For iOS Safari
    video.setAttribute('playsinline', 'true');
    document.body.appendChild(video);

    await new Promise<void>((resolve) => {
      video.onplay = () => {
        setTimeout(() => {
          const canvas = document.createElement('canvas');
          const settings = track.getSettings();
          const width = settings.width ?? video.videoWidth ?? 1280;
          const height = settings.height ?? video.videoHeight ?? 720;
          canvas.width = width;
          canvas.height = height;
          const ctx = canvas.getContext('2d')!;

          // Draw video frame to canvas
          ctx.drawImage(video, 0, 0);

          // Get image data and check if all pixels are black
          const imageData = ctx.getImageData(0, 0, canvas.width, canvas.height);
          const data = imageData.data;
          let isAllBlack = true;
          for (let i = 0; i < data.length; i += 4) {
            if (data[i] !== 0 || data[i + 1] !== 0 || data[i + 2] !== 0) {
              isAllBlack = false;
              break;
            }
          }

          if (isAllBlack) {
            this.appendError('camera appears to be producing only black frames');
          } else {
            this.appendMessage('received video frames');
          }
          resolve();
        }, 1000);
      };
      video.play();
    });

    stream.getTracks().forEach((t) => t.stop());
    video.remove();
  }
}
