# Video handoff with a ready replacement uplink

The controller configured a **0 ms network outage**. The standby uplink had a verified route to the SFU before the switch. The controller then replaced the default route and brought the old interface down. Both publishers kept capturing the same 30 fps VP8 video, and a third participant decoded the two tracks on a stable MoQ downlink.

## Measured timings

These are milliseconds from issuing the switch command. Each cell gives the **fastest / median / slowest** observation from ten independent rooms, five in each direction. The SFU timed its selected QUIC path or WebRTC ICE candidate pair. Frame IDs and capture timestamps identify the first frame captured after the kernel acknowledged the switch.

| Milestone | MoQ | WebRTC |
| --- | ---: | ---: |
| First UDP packet from the replacement address | 2.2 / 21.2 / 31.0 | 724.6 / 947.8 / 1,197.9 |
| SFU selected the replacement path | 2.1 / 21.0 / 31.1 | 724.3 / 947.6 / 1,198.1 |
| Viewer decoded the first fresh frame | 14.3 / 26.0 / 36.3 | 791.4 / 1,011.6 / 1,602.6 |
| Longest gap between advancing frames through capture end | 40.1 / 41.3 / 43.7 | 798.5 / 1,018.7 / 1,604.1 |

The SFU selection timestamp was available in 10/10 MoQ and 10/10 WebRTC trials. An incoming UDP packet can carry a probe or acknowledgment. The viewer's frame measurement includes encoding, transport, forwarding, decoding, and the wait for the next capture. The no-switch control delivered a fresh frame in 36.3 ms for MoQ and 36.6 ms for WebRTC after its marker.

Of 2,406 IDs captured from the switch command through the end of observation, the viewer missed **0 via MoQ** and **119 via WebRTC** after a two-second drain. Each switched room recorded eight seconds of post-switch capture.

## Evidence of network continuity

The publisher's route witness received **1,465 of 1,465 echo replies** around the ten switches. Its source address changed to match the replacement interface. It sampled about every 2 ms, with a maximum observed gap of 2.78 ms between replies. The kernel command batch took 1.0–3.1 ms including acknowledgment. A usable route was installed throughout that batch. Sampling limits the precision of any packet-loss claim.

Each room started on a fresh connection. Both links run inside one Linux VM at local latency. The figures describe the first switch in that room with its second uplink already reachable. Cellular attachment, radio conditions, and repeated handoffs of the same long-lived connection require separate measurements.

## Reproduce and audit

```sh
./test/moq/run_handoff_vm.sh /tmp/lk-ready-handoff \
  --outage 0 --trials 10 --direction alternate --observe 8 --trace-quic
```

Both executables matched the hashes from the [two-second outage experiment](../README.md). The [comparison CSV](comparison.csv) and [traces](traces.zip) include the calibrated series. The archive contains packet headers, reachability probes, route snapshots, SFU path events, room events, and per-frame ledgers. The controller sampled wall time against monotonic time every 5 ms to account for guest clock adjustments. Server logs have millisecond resolution.

An earlier ten-switch series independently measured frames on the shared monotonic clock. Across both series, the fastest fresh frame reached the viewer after 10.79 ms for MoQ and 664.03 ms for WebRTC. The earlier series exposed a wall-clock step, so only its monotonic frame measurements are reported. [Earlier frame timings](first-series-frames.csv) · [Harness guide](../../../HANDOFF.md)
