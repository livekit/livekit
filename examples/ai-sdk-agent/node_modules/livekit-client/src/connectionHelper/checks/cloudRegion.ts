import { RegionUrlProvider } from '../../room/RegionUrlProvider';
import { type CheckInfo, Checker } from './Checker';

export interface RegionStats {
  region: string;
  rtt: number;
  duration: number;
}

/**
 * Checks for connections quality to closests Cloud regions and determining the best quality
 */
export class CloudRegionCheck extends Checker {
  private bestStats?: RegionStats;

  get description(): string {
    return 'Cloud regions';
  }

  async perform(): Promise<void> {
    const regionProvider = new RegionUrlProvider(this.url, this.token);
    if (!regionProvider.isCloud()) {
      this.skip();
      return;
    }

    const regionStats: RegionStats[] = [];
    const seenUrls: Set<string> = new Set();
    for (let i = 0; i < 3; i++) {
      const regionUrl = await regionProvider.getNextBestRegionUrl();
      if (!regionUrl) {
        break;
      }
      if (seenUrls.has(regionUrl)) {
        continue;
      }
      seenUrls.add(regionUrl);
      const stats = await this.checkCloudRegion(regionUrl);
      this.appendMessage(`${stats.region} RTT: ${stats.rtt}ms, duration: ${stats.duration}ms`);
      regionStats.push(stats);
    }

    regionStats.sort((a, b) => {
      return (a.duration - b.duration) * 0.5 + (a.rtt - b.rtt) * 0.5;
    });
    const bestRegion = regionStats[0];
    this.bestStats = bestRegion;
    this.appendMessage(`best Cloud region: ${bestRegion.region}`);
  }

  getInfo(): CheckInfo {
    const info = super.getInfo();
    info.data = this.bestStats;
    return info;
  }

  private async checkCloudRegion(url: string): Promise<RegionStats> {
    await this.connect(url);
    if (this.options.protocol === 'tcp') {
      await this.switchProtocol('tcp');
    }
    const region = this.room.serverInfo?.region;
    if (!region) {
      throw new Error('Region not found');
    }

    const writer = await this.room.localParticipant.streamText({ topic: 'test' });
    const chunkSize = 1000; // each chunk is about 1000 bytes
    const totalSize = 1_000_000; // approximately 1MB of data
    const numChunks = totalSize / chunkSize; // will yield 1000 chunks
    const chunkData = 'A'.repeat(chunkSize); // create a string of 1000 'A' characters

    const startTime = Date.now();
    for (let i = 0; i < numChunks; i++) {
      await writer.write(chunkData);
    }
    await writer.close();
    const endTime = Date.now();
    const stats = await this.room.engine.pcManager?.publisher.getStats();
    const regionStats: RegionStats = {
      region: region,
      rtt: 10000,
      duration: endTime - startTime,
    };
    stats?.forEach((stat) => {
      if (stat.type === 'candidate-pair' && stat.nominated) {
        regionStats.rtt = stat.currentRoundTripTime * 1000;
      }
    });

    await this.disconnect();
    return regionStats;
  }
}
