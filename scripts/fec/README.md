# FlexFEC test harness (publisher → SFU)

Validates that FlexFEC-03 sent by a publisher recovers lost packets at the SFU
under cellular-like loss (continuous base loss plus periodic bursts), the kind
of uplink a robot on LTE/5G sees. The harness runs everything on loopback:

```
publisher (rust-sdks local_video, --test-pattern --flex-fec)
    │  UDP → 127.0.0.1:7882      ← traffic shaper drops packets here
    ▼
livekit-server (enable_flexfec: true, prometheus on :6789)
    │
    ▼
subscriber (rust-sdks local_video, --headless --log-frames)
```

The publisher attaches a wall-clock timestamp and a frame id to every frame
via the packet trailer feature; the subscriber logs them per received frame.
That gives ground truth for end-to-end frame latency and frame loss, while
the SFU's prometheus counters (`livekit_flexfec_packet_total{state=...}`,
`livekit_nack_total`, `livekit_packet_loss_total`) show what FEC did.

## Requirements

- `go`, `cargo`, `curl`, `python3` with `matplotlib` (`pip3 install matplotlib`)
- a `rust-sdks` checkout with the local_video example
  (default `../rust-sdks` relative to this repo, override with `RUST_SDKS_DIR`)
- `sudo` for traffic shaping:
  - **macOS**: dummynet (`dnctl` + `pfctl`). If dummynet is unavailable on
    your macOS build, run with `--no-shaping` and use Network Link Conditioner
    manually, or test on Linux.
  - **Linux**: `tc` with the `netem` qdisc (`iproute2`).

## Usage

```bash
# A/B comparison: baseline (NACK only) vs FlexFEC, 2 minutes each
./run_fec_test.sh --mode ab --duration 120

# single FEC run with a harsher profile
./run_fec_test.sh --mode fec --duration 60 --base-loss 0.05 --burst-loss 0.4

# sanity check without shaping (expect ~0 loss, ~0 recoveries)
./run_fec_test.sh --mode fec --duration 30 --no-shaping

# no sudo available: drop 4% of received packets inside the SFU instead of
# OS shaping (uniform loss only, also useful for CI)
./run_fec_test.sh --mode ab --duration 60 --debug-drop 4
```

Outputs land in `scripts/fec/out/<timestamp>/`:

- `fec_report.png` — stacked time series: per-frame latency with frame-gap
  markers and burst shading, FEC received/recovered/failed rates, NACK and
  packet-loss rates, delivered fps
- `summary.txt` — per-run table (frames lost, latency percentiles, FEC
  counters, NACK totals) and the A/B comparison
- per run (`baseline/`, `fec/`): `server.log`, `publisher.log`,
  `subscriber.log`, `frames.csv`, `prom.tsv`, `events.csv`, `meta.env`

## Loss profile

Defaults simulate a robot uplink over cellular: 2% continuous loss
(Gilbert-Elliott on Linux for realistic correlation, uniform on macOS) with a
3 s burst of 25% loss every 15 s. Tune via `--base-loss`, `--burst-loss`,
`--burst-every`, `--burst-len`. The shaper matches **UDP destined to port
7882** on loopback, which is the publisher→SFU media leg; the subscriber's
upstream RTCP shares that port and is shaped too, which is acceptable for A/B
comparisons since both runs see identical conditions.

The publisher defaults to a fixed 30% FEC protection rate with the bursty
mask (`--fec-rate`, `--fec-mask-type`); pass `--fec-rate 0` to let libwebrtc
adapt the rate to its loss estimate instead.

`--debug-drop PCT` is an unprivileged alternative to OS shaping: the SFU
drops PCT% of received packets before processing (uniform, all SSRCs,
enabled via the `LIVEKIT_DEBUG_RX_DROP_PCT` env var). Use the OS shapers for
the cellular burst profile, this knob for quick checks and CI. Note that with
uniform loss and a fixed protection rate, blocks losing two or more packets
are not FEC-recoverable and fall back to NACK/RTX, so expect partial
recovery; bursty loss with the bursty mask is the scenario FlexFEC targets.

## What to expect

- Sanity run (no shaping): `state="received"` grows, `recovered` ≈ 0, no
  frame gaps.
- FEC run under bursts: `recovered` spikes inside burst windows, the
  subscriber sees few or no frame-id gaps, latency stays near baseline.
- Baseline under the same bursts: frame gaps and latency spikes during
  bursts (NACK/RTX needs a round trip per loss; FEC repairs immediately).
- Publisher bitrate should not collapse in the FEC run — FEC packets carry
  transport-wide CC sequence numbers and the SFU reports them, so the
  publisher's bandwidth estimate stays intact.

## Parameter sweep

`sweep_fec.sh` runs `run_fec_test.sh` across a matrix of loss levels and FEC
configurations (one baseline plus one FEC run per `(rate, mask)` at each loss
level), builds the binaries once, then `aggregate_fec.py` combines all cells
into a single comparison: `sweep_report.png` (frame loss, p99 latency, FEC
recovery rate, and recovered-packet count, each vs loss level, baseline vs
every FEC config), `sweep_summary.csv`, and a markdown table.

```bash
# uniform-loss sweep, no sudo: 4 loss levels x 2 FEC rates x 1 mask
./sweep_fec.sh --loss-mode debug --loss-list "2 5 10 15" --fec-rate-list "20 50"

# cellular-burst sweep: vary burst intensity, compare the two masks at 30%
sudo -v && ./sweep_fec.sh --loss-mode shaped --loss-list "0.15 0.30 0.50" \
    --fec-rate-list "30" --mask-list "random bursty" --duration 90

# re-aggregate an existing sweep without re-running it
python3 aggregate_fec.py --sweep out/sweep_<ts>
```

Runtime ≈ cells × (~20s setup + `--duration`). Each cell is a directory under
the sweep output; `manifest.tsv` maps cell → parameters. In `debug` mode
`--loss-list` is packet-drop percentages; in `shaped` mode it is the burst
loss fraction (base loss and cadence fixed by `--base-loss`/`--burst-every`/
`--burst-len`). Note the caveat above: uniform `debug` loss with a fixed FEC
rate only recovers blocks that lose a single packet, so `shaped` bursts with
the `bursty` mask show FlexFEC at its best.

## Direct script use

```bash
sudo ./shape_macos.sh start --port 7882 --base-loss 0.02 --burst-loss 0.25 \
    --burst-every 15 --burst-len 3 --events /tmp/events.csv
sudo ./shape_macos.sh stop   # force cleanup (also: shape_linux.sh)

./prom_poll.sh 6789 /tmp/prom.tsv

# single run / A/B pair
python3 plot_fec.py --run out/<ts>/baseline --run out/<ts>/fec --out out/<ts>
```
