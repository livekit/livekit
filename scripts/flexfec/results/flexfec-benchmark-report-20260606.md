# FlexFEC Benchmark Report - 2026-06-06

## Summary

FlexFEC is functionally active on both tested legs:

- Publisher to SFU: the SFU enabled its FlexFEC decoder and recovered media packets from publisher-side repair packets.
- SFU to subscriber: the subscriber received FlexFEC repair packets and accepted recovered payloads.

The clearest quality win in this run was on the SFU-to-subscriber leg. Under the long fade profile, RTX-only delivery averaged 1694.3 ms capture-to-decode latency, while FlexFEC reduced that to 295.0 ms with `fec5x1` and 298.1 ms with `fec10x2`.

The publisher-to-SFU leg also proved correction, but the single-run quality result was more configuration-sensitive. `fec8x1` was the best uplink long-fade result in this run: 146 SFU-recovered packets, 21 subscriber-reported lost packets vs. 195 for RTX-only, and 141.2 ms capture-to-decode average vs. 154.1 ms for RTX-only.

## Environment

- Host: `DC-Linux`
- OS: Ubuntu 24.04.2 LTS
- Kernel: `Linux dc-linux 6.11.0-25-generic`
- Go: `/home/dc/go1.26/bin/go`, `go1.26.4 linux/amd64`
- SFU source: synced from local branch `dc/exp/flexfec` at `ec606596` plus local benchmark script changes
- Remote SFU workspace: `/home/dc/workspace/livekit-flexfec-bench`
- Local video binaries: `/home/dc/workspace/rust-sdks5/target/debug/publisher` and `subscriber`

## Validation

Passed locally:

```bash
go test ./pkg/config ./pkg/sfu/flexfec ./pkg/sfu/buffer ./pkg/rtc
bash -n scripts/flexfec/sweep-linux.sh scripts/flexfec/netns.sh scripts/flexfec/netem.sh scripts/flexfec/run-leg-linux.sh scripts/flexfec/run-publisher.sh scripts/flexfec/run-sfu.sh scripts/flexfec/run-subscriber.sh
git diff --check
```

Passed on `DC-Linux` after syncing:

```bash
/home/dc/go1.26/bin/go test ./pkg/config ./pkg/sfu/flexfec ./pkg/sfu/buffer ./pkg/rtc
bash -n scripts/flexfec/sweep-linux.sh scripts/flexfec/netns.sh scripts/flexfec/netem.sh scripts/flexfec/run-leg-linux.sh
```

## Methodology

The sweep ran each case for 45 seconds after publisher and subscriber startup.

FEC matrix:

| Label | Meaning |
|---|---|
| `rtx` | FEC disabled, RTX baseline |
| `fec5x1` | 5 media packets, 1 repair packet |
| `fec8x1` | 8 media packets, 1 repair packet |
| `fec10x2` | 10 media packets, 2 repair packets |

Loss profiles:

| Profile | Loss | Burst | Gap | One-way delay | Jitter | Correlation |
|---|---:|---:|---:|---:|---:|---:|
| `fade-short` | 35% | 250 ms | 1750 ms | 40 ms | 10 ms | 70% |
| `fade-long` | 70% | 750 ms | 5000 ms | 60 ms | 20 ms | 85% |

Legs:

| Leg | Topology | Impairment | Primary correction signal |
|---|---|---|---|
| Publisher to SFU | publisher in `robot` netns -> SFU | `tc netem` on robot veth | SFU `packetsRecovered` |
| SFU to subscriber | SFU -> subscriber on loopback/root netns | `tc netem` on SFU UDP/7882 downlink | Subscriber accepted FEC recovery payloads |

Raw artifacts:

- Downlink/full sweep: `scripts/flexfec/results/flexfec-full-20260606-120329/`
- Corrected uplink sweep: `scripts/flexfec/results/flexfec-uplink-rerun-20260606-124334/`
- Superseded uplink sweep with launcher failure: `scripts/flexfec/results/flexfec-uplink-20260606-122520/`

The first uplink sweep was not used for conclusions because publisher namespace startup failed in FEC cases. The sweep scripts were fixed to invoke helper scripts via `bash`, then the uplink-only sweep was rerun successfully.

## Publisher to SFU Results

Subscriber FEC counters are expected to stay zero in this leg because the downlink is clean and subscriber-side FlexFEC is disabled. The relevant correction signal is SFU `packetsRecovered`.

### Uplink `fade-short`

| Config | SFU recovered | Decoded | Freezes | NACK | Lost | Capture-to-decode avg | Delta vs RTX | Capture-to-decode max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| `rtx` | 0 | 1515 | 8 | 40 | 0 | 98.4 ms | 0.0 ms | 99.7 ms |
| `fec5x1` | 212 | 1134 | 12 | 507 | 25 | 102.9 ms | +4.5 ms | 104.9 ms |
| `fec8x1` | 118 | 1361 | 8 | 123 | 4 | 105.1 ms | +6.7 ms | 231.0 ms |
| `fec10x2` | 50 | 1485 | 10 | 88 | 0 | 103.3 ms | +4.9 ms | 209.5 ms |

Interpretation:

- All FEC configurations except RTX produced SFU packet recovery.
- RTX-only had the best short-fade quality metrics in this single run.
- `fec8x1` was the least disruptive FEC option in this profile: same freeze count as RTX and only 4 lost packets, while still proving 118 recovered packets at the SFU.

### Uplink `fade-long`

| Config | SFU recovered | Decoded | Freezes | NACK | Lost | Capture-to-decode avg | Delta vs RTX | Capture-to-decode max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| `rtx` | 0 | 1224 | 9 | 239 | 195 | 154.1 ms | 0.0 ms | 156.2 ms |
| `fec5x1` | 32 | 1148 | 9 | 261 | 105 | 395.7 ms | +241.6 ms | 4141.8 ms |
| `fec8x1` | 146 | 1295 | 9 | 213 | 21 | 141.2 ms | -12.9 ms | 142.7 ms |
| `fec10x2` | 0 | 991 | 13 | 262 | 31 | 386.0 ms | +231.9 ms | 4139.4 ms |

Interpretation:

- `fec8x1` was the best uplink long-fade configuration in this run.
- `fec8x1` recovered 146 packets at the SFU, decoded more frames than RTX, reduced reported lost packets from 195 to 21, and reduced average capture-to-decode latency by 12.9 ms.
- `fec5x1` and `fec10x2` showed large latency spikes in this run, despite lower reported lost packets than RTX. That makes them poor uplink choices until repeated runs show otherwise.
- `fec10x2` enabled the decoder, but its last SFU recovery counter was zero in this case. That needs repeat testing; burst alignment can dominate a single 45-second run.

## SFU to Subscriber Results

Here the correction signal is subscriber-side accepted recovery payloads. The `fec_recv` column comes from the last video quality log line and can be slightly lower than accepted total when the accepted counter logged later.

### Downlink `fade-short`

| Config | Subscriber FEC accepted | FEC recv | Decoded | Freezes | NACK | Lost | RTX recovered | Capture-to-decode avg | Delta vs RTX | Capture-to-decode max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `rtx` | 0 | 0 | 1425 | 14 | 82 | 57 | 57 | 131.1 ms | 0.0 ms | 270.0 ms |
| `fec5x1` | 325 | 315 | 1392 | 15 | 81 | 63 | 50 | 113.0 ms | -18.1 ms | 254.9 ms |
| `fec8x1` | 203 | 203 | 1450 | 15 | 80 | 60 | 52 | 108.6 ms | -22.5 ms | 255.4 ms |
| `fec10x2` | 327 | 327 | 1452 | 13 | 90 | 69 | 59 | 118.5 ms | -12.6 ms | 270.5 ms |

Interpretation:

- All FEC configurations generated accepted recovery payloads at the subscriber.
- Average capture-to-decode latency improved by 12.6 to 22.5 ms vs. RTX-only.
- Freeze counts were broadly similar in this short profile, so latency and accepted recovery are the stronger signals.
- `fec8x1` had the lowest average latency; `fec10x2` decoded the most frames and had the fewest freezes.

### Downlink `fade-long`

| Config | Subscriber FEC accepted | FEC recv | Decoded | Freezes | NACK | Lost | RTX recovered | Capture-to-decode avg | Delta vs RTX | Capture-to-decode max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `rtx` | 0 | 0 | 839 | 10 | 284 | 123 | 105 | 1694.3 ms | 0.0 ms | 3262.7 ms |
| `fec5x1` | 307 | 295 | 632 | 12 | 309 | 118 | 101 | 295.0 ms | -1399.3 ms | 3260.3 ms |
| `fec8x1` | 198 | 187 | 843 | 16 | 313 | 125 | 105 | 527.0 ms | -1167.3 ms | 3754.8 ms |
| `fec10x2` | 312 | 300 | 747 | 11 | 307 | 130 | 112 | 298.1 ms | -1396.2 ms | 3259.6 ms |

Interpretation:

- Downlink FlexFEC strongly reduced average decode latency under long fades.
- `fec5x1` and `fec10x2` performed similarly on latency: 295.0 ms and 298.1 ms vs. 1694.3 ms for RTX-only.
- `fec8x1` decoded the most frames, but had worse latency and more freezes than the other FEC profiles.
- The high max latency values show there are still isolated stalls; the average latency improvement is still large.

## Conclusions

1. FlexFEC correction is proven in both directions.
   - Publisher to SFU: SFU logs show the FlexFEC decoder enabled and `packetsRecovered` increasing.
   - SFU to subscriber: subscriber logs show FEC received and recovery payloads accepted.

2. SFU-to-subscriber FEC is clearly useful in this burst-loss setup.
   - It substantially reduced average capture-to-decode latency, especially for long fades.
   - `fec5x1` looks like the best initial downlink default because it matched `fec10x2` latency with lower nominal repair overhead.

3. Publisher-to-SFU FEC is useful but needs more repetitions before choosing a default.
   - `fec8x1` was the best uplink long-fade configuration in this run.
   - `fec5x1` recovered the most short-fade packets but hurt subscriber quality metrics in that case.
   - `fec10x2` was inconsistent on uplink and should not be chosen from this single run.

4. Freeze count alone is not a reliable success metric here.
   - Some FEC cases recovered packets and reduced latency without reducing freezes.
   - Report `packetsRecovered`, accepted recovery payloads, latency, lost packets, NACK/PLI, and decoded frames together.

## Recommended Next Sweep

Run 3 to 5 repetitions per case and compare medians/percentiles. The current single-run results are enough to validate functionality, but burst timing makes per-config ranking noisy.

Recommended next matrix:

```bash
DURATION=120 \
SCENARIOS="uplink downlink" \
FEC_CONFIGS="rtx:off:0:0 fec5x1:on:5:1 fec6x1:on:6:1 fec8x1:on:8:1 fec10x1:on:10:1 fec10x2:on:10:2" \
LOSS_PROFILES="fade-short:35:250:1750:40:10:70 fade-long:70:750:5000:60:20:85 handoff:85:1200:7000:80:30:90" \
scripts/flexfec/sweep-linux.sh
```

Add report fields for:

- FEC bytes/packets sent to compute overhead.
- P50/P95/P99 capture-to-decode latency from all timing windows, not just the last one.
- SFU ingress loss before and after recovery.
- Subscriber RTP jitter and jitter-buffer delay.
- CPU usage for SFU and publisher/subscriber.
