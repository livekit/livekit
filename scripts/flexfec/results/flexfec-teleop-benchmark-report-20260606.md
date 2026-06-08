# FlexFEC Tele-Op Benchmark Report - 2026-06-06

## Summary

Ran a tele-op-oriented benchmark suite on `DC-Linux`:

- Main path: publisher/robot uplink over cellular-like loss into the SFU.
- Control path: SFU-to-subscriber downlink over clean/mild landline-like loss.

The result matches the tele-op premise:

- Uplink FEC is useful under moderate to severe cellular fades, but the right block size depends on the fade shape.
- Downlink FEC showed no practical quality benefit under clean/mild landline conditions; it only adds repair traffic.

Best uplink candidates from this single run:

- `fec6x1` for moderate `cell-edge` loss.
- `fec5x1` for severe long handoff where latency matters most.
- `fec8x1` as a conservative general candidate: it behaved well in `cell-edge`, `handoff-long`, and `bursty-uplink`, but was not always the lowest-latency choice.

Avoid `fec10x2` as a default for tele-op uplink. It recovered packets, but it caused clear latency/quality regressions in `bursty-uplink`.

## Validation

Passed locally:

```bash
go test ./pkg/config ./pkg/sfu/flexfec ./pkg/sfu/buffer ./pkg/rtc
bash -n scripts/flexfec/sweep-linux.sh scripts/flexfec/netns.sh scripts/flexfec/netem.sh scripts/flexfec/run-leg-linux.sh scripts/flexfec/run-publisher.sh scripts/flexfec/run-sfu.sh scripts/flexfec/run-subscriber.sh
```

Passed on `DC-Linux` after syncing:

```bash
/home/dc/go1.26/bin/go test ./pkg/config ./pkg/sfu/flexfec ./pkg/sfu/buffer ./pkg/rtc
bash -n scripts/flexfec/sweep-linux.sh scripts/flexfec/netns.sh scripts/flexfec/netem.sh scripts/flexfec/run-leg-linux.sh
```

No launcher failures were found in the copied log artifacts.

## Raw Artifacts

- Uplink cellular sweep: `scripts/flexfec/results/flexfec-teleop-uplink-20260606-135721/`
- Downlink landline control: `scripts/flexfec/results/flexfec-teleop-downlink-control-20260606-151840/`

## Uplink Sweep

Command shape:

```bash
DURATION=120 \
SCENARIOS=uplink \
FEC_CONFIGS="rtx:off:0:0 fec5x1:on:5:1 fec6x1:on:6:1 fec8x1:on:8:1 fec10x1:on:10:1 fec10x2:on:10:2" \
LOSS_PROFILES="clean-control:0:100:2000:20:2:0 urban-5g:3:100:2000:35:8:50 cell-edge:20:250:3000:60:20:75 handoff-short:60:500:8000:80:30:85 handoff-long:85:1200:12000:100:40:90 bursty-uplink:35:300:2500:70:25:85" \
scripts/flexfec/sweep-linux.sh
```

Primary correction signal: SFU `packetsRecovered`.

### `clean-control`

| Config | SFU recovered | Decoded | Freezes | NACK | Lost | PLI | Capture-to-decode avg | Delta vs RTX | Max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `rtx` | 0 | 3645 | 2 | 2 | 0 | 0 | 72.8 ms | 0.0 ms | 73.6 ms |
| `fec5x1` | 0 | 3706 | 2 | 3 | 0 | 0 | 71.0 ms | -1.8 ms | 72.0 ms |
| `fec6x1` | 0 | 3736 | 2 | 2 | 0 | 0 | 71.9 ms | -0.9 ms | 73.0 ms |
| `fec8x1` | 0 | 3676 | 2 | 2 | 0 | 0 | 72.4 ms | -0.4 ms | 73.5 ms |
| `fec10x1` | 498 | 3585 | 3 | 818 | 37 | 1 | 71.9 ms | -0.9 ms | 72.9 ms |
| `fec10x2` | 0 | 3676 | 2 | 5 | 0 | 0 | 71.9 ms | -0.9 ms | 72.7 ms |

Note: `fec10x1` is anomalous here: a no-loss case produced SFU recovery, high NACK, and lost packets. Treat that row as suspect and rerun before drawing conclusions from it.

### `urban-5g`

| Config | SFU recovered | Decoded | Freezes | NACK | Lost | PLI | Capture-to-decode avg | Delta vs RTX | Max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `rtx` | 0 | 3736 | 2 | 14 | 0 | 0 | 86.9 ms | 0.0 ms | 87.8 ms |
| `fec5x1` | 0 | 3647 | 2 | 10 | 0 | 0 | 87.8 ms | +0.9 ms | 88.5 ms |
| `fec6x1` | 0 | 3676 | 2 | 11 | 0 | 0 | 88.6 ms | +1.7 ms | 89.5 ms |
| `fec8x1` | 20 | 3646 | 2 | 29 | 0 | 0 | 87.8 ms | +0.9 ms | 94.5 ms |
| `fec10x1` | 101 | 3705 | 2 | 34 | 0 | 0 | 87.8 ms | +0.9 ms | 92.8 ms |
| `fec10x2` | 101 | 3706 | 2 | 47 | 0 | 0 | 88.8 ms | +1.9 ms | 90.0 ms |

Interpretation: RTX/no-FEC is already fine under mild cellular loss. FEC adds little benefit here.

### `cell-edge`

| Config | SFU recovered | Decoded | Freezes | NACK | Lost | PLI | Capture-to-decode avg | Delta vs RTX | Max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `rtx` | 0 | 3608 | 5 | 446 | 0 | 0 | 137.6 ms | 0.0 ms | 138.8 ms |
| `fec5x1` | 145 | 3674 | 5 | 506 | 0 | 0 | 137.1 ms | -0.5 ms | 159.7 ms |
| `fec6x1` | 77 | 3692 | 4 | 443 | 0 | 0 | 134.1 ms | -3.5 ms | 138.8 ms |
| `fec8x1` | 77 | 3734 | 4 | 474 | 0 | 0 | 137.8 ms | +0.2 ms | 147.9 ms |
| `fec10x1` | 0 | 3674 | 4 | 456 | 0 | 0 | 135.0 ms | -2.6 ms | 143.1 ms |
| `fec10x2` | 189 | 3674 | 4 | 568 | 0 | 0 | 137.2 ms | -0.4 ms | 161.5 ms |

Interpretation: `fec6x1` was the best-balanced result: lowest latency, fewer freezes than RTX, and similar NACK load.

### `handoff-short`

| Config | SFU recovered | Decoded | Freezes | NACK | Lost | PLI | Capture-to-decode avg | Delta vs RTX | Max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `rtx` | 0 | 3465 | 15 | 889 | 7 | 2 | 222.5 ms | 0.0 ms | 789.2 ms |
| `fec5x1` | 185 | 3551 | 16 | 880 | 8 | 1 | 201.4 ms | -21.1 ms | 205.6 ms |
| `fec6x1` | 125 | 3500 | 18 | 828 | 2 | 1 | 192.4 ms | -30.1 ms | 927.9 ms |
| `fec8x1` | 206 | 3655 | 16 | 832 | 0 | 0 | 231.6 ms | +9.1 ms | 403.5 ms |
| `fec10x1` | 25 | 3734 | 17 | 866 | 0 | 0 | 242.4 ms | +19.9 ms | 630.5 ms |
| `fec10x2` | 181 | 3617 | 16 | 856 | 2 | 1 | 227.3 ms | +4.8 ms | 571.6 ms |

Interpretation: `fec6x1` had the best average latency, but its max latency and freeze count were worse. `fec5x1` is safer for short handoffs if max-latency stability matters more than the lowest average.

### `handoff-long`

| Config | SFU recovered | Decoded | Freezes | NACK | Lost | PLI | Capture-to-decode avg | Delta vs RTX | Max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `rtx` | 0 | 3373 | 12 | 915 | 3 | 2 | 245.3 ms | 0.0 ms | 261.1 ms |
| `fec5x1` | 454 | 3268 | 14 | 847 | 0 | 0 | 171.2 ms | -74.1 ms | 214.0 ms |
| `fec6x1` | 503 | 3426 | 15 | 954 | 1 | 1 | 219.6 ms | -25.7 ms | 238.5 ms |
| `fec8x1` | 446 | 3549 | 12 | 1105 | 35 | 1 | 202.2 ms | -43.1 ms | 231.6 ms |
| `fec10x1` | 500 | 2963 | 21 | 823 | 1 | 1 | 1120.8 ms | +875.5 ms | 1197.1 ms |
| `fec10x2` | 491 | 3207 | 16 | 922 | 8 | 1 | 223.8 ms | -21.5 ms | 234.6 ms |

Interpretation: `fec5x1` clearly lowered latency under severe long fades. `fec8x1` decoded the most frames and matched RTX freeze count, but reported more lost packets. `fec10x1` is a bad fit here.

### `bursty-uplink`

| Config | SFU recovered | Decoded | Freezes | NACK | Lost | PLI | Capture-to-decode avg | Delta vs RTX | Max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `rtx` | 0 | 3726 | 20 | 661 | 0 | 0 | 153.5 ms | 0.0 ms | 157.5 ms |
| `fec5x1` | 20 | 3704 | 21 | 650 | 0 | 0 | 154.1 ms | +0.6 ms | 164.3 ms |
| `fec6x1` | 122 | 3668 | 16 | 732 | 8 | 1 | 160.0 ms | +6.5 ms | 169.7 ms |
| `fec8x1` | 143 | 3668 | 17 | 775 | 6 | 1 | 154.0 ms | +0.5 ms | 159.3 ms |
| `fec10x1` | 120 | 3608 | 20 | 736 | 3 | 1 | 157.4 ms | +3.9 ms | 158.9 ms |
| `fec10x2` | 290 | 3199 | 18 | 1091 | 27 | 5 | 254.6 ms | +101.1 ms | 664.0 ms |

Interpretation: `fec8x1` was the best FEC tradeoff here: nearly RTX latency with fewer freezes, but still more NACK and some lost packets. `fec10x2` is too heavy/unstable for this profile.

## Downlink Landline Control

Command shape:

```bash
DURATION=60 \
SCENARIOS=downlink \
FEC_CONFIGS="rtx:off:0:0 fec8x1:on:8:1 fec10x2:on:10:2" \
LOSS_PROFILES="landline-clean:0:100:2000:5:1:0 landline-mild:1:50:2000:10:2:0" \
scripts/flexfec/sweep-linux.sh
```

Primary correction signal: subscriber accepted recovery payloads. Under these landline profiles, accepted payloads prove FEC is active, not that it is needed.

### `landline-clean`

| Config | Subscriber FEC accepted | FEC recv | Decoded | Freezes | NACK | Lost | RTX recovered | Capture-to-decode avg | Delta vs RTX | Max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `rtx` | 0 | 0 | 1906 | 2 | 0 | 0 | 0 | 56.8 ms | 0.0 ms | 57.6 ms |
| `fec8x1` | 267 | 267 | 1907 | 2 | 0 | 0 | 0 | 56.8 ms | 0.0 ms | 57.7 ms |
| `fec10x2` | 426 | 408 | 1816 | 2 | 0 | 0 | 0 | 55.9 ms | -0.9 ms | 56.8 ms |

### `landline-mild`

| Config | Subscriber FEC accepted | FEC recv | Decoded | Freezes | NACK | Lost | RTX recovered | Capture-to-decode avg | Delta vs RTX | Max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `rtx` | 0 | 0 | 1906 | 2 | 0 | 0 | 0 | 62.8 ms | 0.0 ms | 63.7 ms |
| `fec8x1` | 267 | 259 | 1847 | 2 | 0 | 0 | 0 | 62.9 ms | +0.1 ms | 63.6 ms |
| `fec10x2` | 426 | 408 | 1817 | 2 | 0 | 0 | 0 | 62.9 ms | +0.1 ms | 63.7 ms |

Interpretation: downlink FEC provided no visible quality benefit in clean or mild landline conditions. Keep subscriber-side/downlink FEC disabled for the tele-op default unless the operator path is known to be lossy.

## Recommendation

For a tele-op default where only the robot uplink is cellular:

1. Enable FEC on publisher-to-SFU only.
2. Leave SFU-to-subscriber FEC disabled by default.
3. Start field testing with `fec6x1` and `fec8x1`.
4. Add `fec5x1` as an aggressive mode for known handoff/fade zones.
5. Do not default to `fec10x2`; it is too costly under correlated burst loss in this run.

The next benchmark improvement should be repeated runs with median/P95/P99 latency extraction from all timing windows. This run is enough to choose candidates, but not enough to lock a production default from a single sample per case.
