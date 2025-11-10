import { describe, expect, it } from 'vitest';
import DeviceManager from './DeviceManager';

class MockDeviceManager extends DeviceManager {
  dummyDevices?: MediaDeviceInfo[];

  async getDevices(
    kind?: MediaDeviceKind | undefined,
    requestPermissions?: boolean,
  ): Promise<MediaDeviceInfo[]> {
    if (this.dummyDevices) {
      return this.dummyDevices;
    } else {
      return super.getDevices(kind, requestPermissions);
    }
  }
}

describe('Active device switch', () => {
  const deviceManager = new MockDeviceManager();
  it('normalizes default ID correctly', async () => {
    deviceManager.dummyDevices = [
      {
        deviceId: 'default',
        kind: 'audiooutput',
        label: 'Default - Speakers (Intel® Smart Sound Technology for I2S Audio)',
        groupId: 'c94fea7109a30d468722f3b7778302c716d683a619f41b264b0cf8b2ec202d9g',
        toJSON: () => 'dummy',
      },
      {
        deviceId: 'communications',
        kind: 'audiooutput',
        label: 'Communications - Speakers (2- USB Advanced Audio Device) (0d8c:016c)',
        groupId: '5146b7ad442c53c9366b141edceca8be30c0a4b31c181968cc732a100176e765',
        toJSON: () => 'dummy',
      },
      {
        deviceId: 'bfbbf4fbdba0ce0b12159f2f615ba856bdf17743d8b791265db22952d98c28cf',
        kind: 'audiooutput',
        label: 'Speakers (2- USB Advanced Audio Device) (0d8c:016c)',
        groupId: '5146b7ad442c53c9366b141edceca8be30c0a4b31c181968cc732a100176e765',
        toJSON: () => 'dummy',
      },
      {
        deviceId: '6ca3eb8140dc3d2919d6747f73d8277e6ecaf0f179426695154e98615bafd2b9',
        kind: 'audiooutput',
        label: 'Speakers (Intel® Smart Sound Technology for I2S Audio)',
        groupId: 'c94fea7109a30d468722f3b7778302c716d683a619f41b264b0cf8b2ec202d9g',
        toJSON: () => 'dummy',
      },
    ];

    const normalizedID = await deviceManager.normalizeDeviceId('audiooutput', 'default');
    expect(normalizedID).toBe('6ca3eb8140dc3d2919d6747f73d8277e6ecaf0f179426695154e98615bafd2b9');
  });
  it('returns undefined when default cannot be determined', async () => {
    deviceManager.dummyDevices = [
      {
        deviceId: 'default',
        kind: 'audiooutput',
        label: 'Default',
        groupId: 'default',
        toJSON: () => 'dummy',
      },
      {
        deviceId: 'd5a1ad8b1314736ad1936aae1d74fa524f954c3281b4af3b65b2492330c3a830',
        kind: 'audiooutput',
        label: 'Alder Lake PCH-P High Definition Audio Controller HDMI / DisplayPort 3 Output',
        groupId: 'af6745746c55f7697eadbb5e31a8f28ef836b4d8aefdc3655189a9e7d81eb8d',
        toJSON: () => 'dummy',
      },
      {
        deviceId: '093f4e51743557382b19da4c0250869b9c6d176423b241f0d52ed665f636e9d2',
        kind: 'audiooutput',
        label: 'Alder Lake PCH-P High Definition Audio Controller HDMI / DisplayPort 2 Output',
        groupId: 'e53791b0ce4bad2b3a515cb1e154acf4758bb563b11942949130190d5c2e0d4',
        toJSON: () => 'dummy',
      },
      {
        deviceId: 'a6ffa042ac4a88e9ff552ce50b016d0bbf60a9e3c2173a444b064ef1aa022fb5',
        kind: 'audiooutput',
        label: 'Alder Lake PCH-P High Definition Audio Controller HDMI / DisplayPort 1 Output',
        groupId: '69bb8042d093e8b33e9c33710cdfb8c0bba08889904b012e1a186704d74b39a',
        toJSON: () => 'dummy',
      },
      {
        deviceId: 'd746e22bcfa3f8f76dfce7ee887612982c226eb1f2ed77502ed621b9d7cdae00',
        kind: 'audiooutput',
        label: 'Alder Lake PCH-P High Definition Audio Controller Speaker + Headphones',
        groupId: 'd08b9d0b8d1460c8c120333bdcbc42fbb92fa8e902926fb8b1f35d43ad7f10f',
        toJSON: () => 'dummy',
      },
      {
        deviceId: 'c43858eb7092870122d5bc3af7b7b7e2f9baf9b3aa829adb34cc84c9f65538a3',
        kind: 'audiooutput',
        label: 'T11',
        groupId: '1ecff3666059160ac3ae559e97286de0ee2487bce8808e9040cba26805d3e15',
        toJSON: () => 'dummy',
      },
    ];

    const normalizedID = await deviceManager.normalizeDeviceId('audiooutput', 'default');
    expect(normalizedID).toBe(undefined);
  });
});
