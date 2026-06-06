# Local FlexFEC-03 test harness

Scripts to validate end-to-end FlexFEC-03 between the rust-sdks `local_video`
examples and a locally-built SFU, under simulated packet loss.

FlexFEC is terminated at the SFU (it can't be passed through because the SFU
rewrites SSRC/sequence numbers per subscriber), so the two directions are
independent and tested separately:

| Direction | What's exercised                                   | Loss to inject |
| --------- | -------------------------------------------------- | -------------- |
| Downlink  | SFU **generates** a repair stream per subscriber   | `down`         |
| Uplink    | SFU **receives + decodes** a publisher repair flow | `up`           |

## Prerequisites

- Go toolchain (to build this SFU).
- A `rust-sdks5` checkout with the FlexFEC changes, next to this repo
  (`../rust-sdks5`) or pointed at via `RUST_SDKS_DIR`. Its `webrtc-sys` must be
  built (the C++ field trials that enable FlexFEC-03 live there).
- A camera (the publisher captures real video).
- `sudo` for traffic shaping. macOS uses `dnctl`/`pfctl` (dummynet); Linux uses
  `tc netem`.

All processes run on one machine, so media flows over loopback. The SFU is pinned
to UDP `7882` (see `flexfec-local.yaml`) so shaping can target it by port.

## Files

| File                 | Purpose                                              |
| -------------------- | ---------------------------------------------------- |
| `flexfec-local.yaml` | SFU config: flexfec on (pub+sub), fixed loopback port |
| `run-sfu.sh`         | Build + run the SFU (`--dev`, devkey/secret)         |
| `netem.sh`           | Start/stop/inspect packet-loss shaping on udp/7882   |
| `run-publisher.sh`   | Run the publisher with `--flex-fec`                  |
| `run-subscriber.sh`  | Run the subscriber, surface `Video FEC ...` logs     |

## Workflow

Use four terminals.

**1 — SFU**

```bash
scripts/flexfec/run-sfu.sh
```

Connects on `ws://127.0.0.1:7880` (API key `devkey` / secret `secret`).

**2 — Subscriber**

```bash
scripts/flexfec/run-subscriber.sh
# or only show FEC lines on the console:
FEC_ONLY=1 scripts/flexfec/run-subscriber.sh
```

**3 — Publisher** (grant camera permission on first run)

```bash
scripts/flexfec/run-publisher.sh
```

**4 — Packet loss** (needs sudo)

```bash
# Uplink test: drop 8% of publisher -> SFU packets
sudo scripts/flexfec/netem.sh start 8 up

# Downlink test: drop 8% of SFU -> subscriber packets (+ optional delay_ms, see A/B below)
sudo scripts/flexfec/netem.sh start 8 down
sudo scripts/flexfec/netem.sh start 8 down 150   # 8% loss + 150ms one-way delay

sudo scripts/flexfec/netem.sh status
sudo scripts/flexfec/netem.sh stop   # always clean up when done
```

## What success looks like

**Downlink (`down`)** — the subscriber recovers SFU-generated repair packets.
In the subscriber log (`logs/subscriber.log`):

```
Video FEC active: fec_ssrc=..., received_total=..., discarded_total=..., accepted_recovery_payload_total=...
Video FEC recovery payload accepted: +N packets (accepted_total=...)
```

`accepted_recovery_payload_total` should climb while loss is applied.

**Uplink (`up`)** — the SFU receives + decodes the publisher's repair stream.
In the SFU log:

```
flexfec decoder enabled   {"fecSSRC": ..., "mediaSSRC": ...}
flexfec recovery stats    {"fecPacketsReceived": ..., "packetsRecovered": ..., "packetsRecoveredDelta": ...}
```

`packetsRecovered` increasing confirms the SFU rebuilt lost uplink packets.

To also see FlexFEC SDP negotiation (`flexfec ... from sdp`, codec registration),
set `logging.level: debug` in `flexfec-local.yaml`.

### Subscriber-side A/B (isolating FEC from RTX)

On loopback RTT~=0, so NACK/RTX recovers nearly all loss before FlexFEC matters and
its benefit is invisible. To make FEC the primary recovery path, add one-way delay so
retransmissions arrive too late, and compare FEC on vs off under the same conditions:

```bash
# A) FEC ON (flexfec.subscriber: true). With SFU + pub + sub running:
sudo scripts/flexfec/netem.sh start 8 down 150     # 8% loss + 150ms delay, downlink
# ...observe subscriber for ~20s, then...
sudo scripts/flexfec/netem.sh stop

# B) FEC OFF: set flexfec.subscriber: false in flexfec-local.yaml, restart the SFU,
#    reconnect the subscriber, then apply the SAME shaping and observe.
```

The subscriber logs a `Video quality:` line every ~3s:

```
Video quality: frames_decoded=..., frames_dropped=..., freezes=N (Xs), nack=..., pli=...,
               packets_lost=..., rtx_recovered=..., fec_recv=..., fec_discarded=...
```

With FEC ON you expect fewer `freezes`/`frames_dropped`, lower net `packets_lost`, and a
meaningful `fec_recv`; with FEC OFF the same loss leans entirely on `rtx_recovered` and
(because of the delay) typically shows more freezes and dropped frames.

## Notes & caveats

- **Loopback shaping (macOS):** `netem.sh` loads its dummynet rules into a dedicated
  `flexfec-loss` pf anchor and re-applies `/etc/pf.conf` non-destructively. `stop`
  flushes the pipes and restores pf to its prior enabled/disabled state. If pf is set
  to `skip on lo0` the script warns and shaping won't apply.
- **Direction isolation is approximate.** Both clients share the SFU UDP port, so
  `up`/`down` are distinguished by source vs. destination port. The publisher is the
  dominant sender to the SFU and the subscriber the dominant receiver, so this is good
  enough to isolate each path in practice (some RTCP is also affected).
- **`dnctl -q flush` clears all dummynet pipes**, not just ours — fine on a dev box.
- **Send-side BWE:** if the SFU uses pacer-based send-side BWE, subscriber FEC overhead
  isn't accounted for by congestion control (logged as a warning by the SFU). The
  interceptor-based send-side BWE path TWCC-tags FEC packets correctly.
- Increase loss gradually (e.g. 3% → 10%). Beyond what a single FlexFEC repair packet
  can cover, bursts become unrecoverable (expected).
