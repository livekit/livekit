#!/usr/bin/env python3
"""Render time-series plots and a summary for FlexFEC test harness runs.

Inputs are run directories produced by run_fec_test.sh, each containing:
  frames.csv  - per received frame: recv_wall_us,frame_id,user_timestamp_us,width,height
  prom.tsv    - 1s prometheus samples: wall_us<TAB>metric{labels}<TAB>value
  events.csv  - shaper events: wall_us,event (burst_on / burst_off / ...)
  meta.env    - run parameters incl. T0_US/T1_US measurement window

Usage:
  plot_fec.py --run <dir> [--run <dir2>] --out <dir>

With two runs the first is treated as the baseline and the second as the FEC
run, both are overlaid and a comparison table is printed.
"""

import argparse
import csv
import os
import re
import statistics
import sys

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

FLEXFEC_METRIC = "livekit_flexfec_packet_total"
METRIC_RE = re.compile(r"^(?P<name>[a-zA-Z0-9_]+)(?:\{(?P<labels>.*)\})?$")


class Run:
    def __init__(self, path):
        self.path = path
        self.name = os.path.basename(os.path.normpath(path))
        self.meta = self._read_meta()
        self.t0 = int(self.meta.get("T0_US", 0))
        self.t1 = int(self.meta.get("T1_US", 1 << 62))
        self.frames = self._read_frames()
        self.prom = self._read_prom()
        self.bursts = self._read_bursts()

    def _read_meta(self):
        meta = {}
        path = os.path.join(self.path, "meta.env")
        if os.path.exists(path):
            with open(path) as f:
                for line in f:
                    if "=" in line:
                        k, v = line.strip().split("=", 1)
                        meta[k] = v
        return meta

    def _in_window(self, ts):
        return self.t0 <= ts <= self.t1

    def rel(self, ts):
        return (ts - self.t0) / 1e6

    def _read_frames(self):
        frames = []
        path = os.path.join(self.path, "frames.csv")
        if not os.path.exists(path):
            return frames
        with open(path) as f:
            for row in csv.DictReader(f):
                try:
                    recv = int(row["recv_wall_us"])
                except (KeyError, ValueError):
                    continue
                if not self._in_window(recv):
                    continue
                frames.append(
                    {
                        "recv": recv,
                        "frame_id": int(row["frame_id"]) if row.get("frame_id") else None,
                        "user_ts": int(row["user_timestamp_us"]) if row.get("user_timestamp_us") else None,
                    }
                )
        frames.sort(key=lambda fr: fr["recv"])
        return frames

    def _read_prom(self):
        """metric family -> label key -> [(wall_us, value)]"""
        series = {}
        path = os.path.join(self.path, "prom.tsv")
        if not os.path.exists(path):
            return series
        with open(path) as f:
            for line in f:
                parts = line.rstrip("\n").split("\t")
                if len(parts) != 3:
                    continue
                ts, metric, value = parts
                m = METRIC_RE.match(metric)
                if not m:
                    continue
                try:
                    ts, value = int(ts), float(value)
                except ValueError:
                    continue
                family = m.group("name")
                labels = m.group("labels") or ""
                series.setdefault(family, {}).setdefault(labels, []).append((ts, value))
        return series

    def _read_bursts(self):
        """[(start_rel_s, end_rel_s)] of shaper burst windows"""
        bursts, start = [], None
        path = os.path.join(self.path, "events.csv")
        if not os.path.exists(path):
            return bursts
        with open(path) as f:
            for line in f:
                parts = line.strip().split(",", 1)
                if len(parts) != 2:
                    continue
                ts, event = int(parts[0]), parts[1]
                if event == "burst_on":
                    start = self.rel(ts)
                elif event == "burst_off" and start is not None:
                    bursts.append((start, self.rel(ts)))
                    start = None
        if start is not None:
            bursts.append((start, self.rel(self.t1 if self.t1 < (1 << 62) else start)))
        return bursts

    # ---------- derived series ----------

    def latency_series(self):
        xs, ys = [], []
        for fr in self.frames:
            if fr["user_ts"]:
                xs.append(self.rel(fr["recv"]))
                ys.append((fr["recv"] - fr["user_ts"]) / 1e3)
        return xs, ys

    def frame_gaps(self):
        """[(rel_s, missing_count)] where frame ids skipped between consecutive frames"""
        gaps, prev = [], None
        for fr in self.frames:
            fid = fr["frame_id"]
            if fid is None:
                continue
            if prev is not None and fid > prev + 1:
                gaps.append((self.rel(fr["recv"]), fid - prev - 1))
            prev = fid
        return gaps

    def fps_series(self):
        buckets = {}
        for fr in self.frames:
            buckets[int(self.rel(fr["recv"]))] = buckets.get(int(self.rel(fr["recv"])), 0) + 1
        xs = sorted(buckets)
        return xs, [buckets[x] for x in xs]

    def counter_rate(self, family, label_filter=None):
        """summed per-second rate across label sets of a counter family"""
        fam = self.prom.get(family, {})
        per_ts = {}
        for labels, samples in fam.items():
            if label_filter and label_filter not in labels:
                continue
            samples = [s for s in samples if self._in_window(s[0])]
            for (t_a, v_a), (t_b, v_b) in zip(samples, samples[1:]):
                dt = (t_b - t_a) / 1e6
                if dt <= 0:
                    continue
                key = int(self.rel(t_b))
                per_ts[key] = per_ts.get(key, 0.0) + max(0.0, v_b - v_a) / dt
        xs = sorted(per_ts)
        return xs, [per_ts[x] for x in xs]

    def counter_total(self, family, label_filter=None):
        """delta of a summed counter family over the measurement window"""
        total = 0.0
        for labels, samples in self.prom.get(family, {}).items():
            if label_filter and label_filter not in labels:
                continue
            samples = [s for s in samples if self._in_window(s[0])]
            if len(samples) >= 2:
                total += samples[-1][1] - samples[0][1]
        return total

    def flexfec_state_rate(self, state):
        return self.counter_rate(FLEXFEC_METRIC, f'state="{state}"')

    def flexfec_state_total(self, state):
        return self.counter_total(FLEXFEC_METRIC, f'state="{state}"')

    # ---------- summary ----------

    def summary(self):
        _, lat = self.latency_series()
        gaps = self.frame_gaps()
        frame_ids = [fr["frame_id"] for fr in self.frames if fr["frame_id"] is not None]
        expected = (max(frame_ids) - min(frame_ids) + 1) if frame_ids else 0
        lost = sum(n for _, n in gaps)

        def pct(p):
            if not lat:
                return float("nan")
            data = sorted(lat)
            return data[min(len(data) - 1, int(len(data) * p))]

        return {
            "frames_received": len(self.frames),
            "frames_expected": expected,
            "frames_lost": lost,
            "gap_events": len(gaps),
            "latency_mean_ms": statistics.fmean(lat) if lat else float("nan"),
            "latency_p50_ms": pct(0.50),
            "latency_p95_ms": pct(0.95),
            "latency_p99_ms": pct(0.99),
            "fec_received": self.flexfec_state_total("received"),
            "fec_recovery_attempts": self.flexfec_state_total("recovery_attempt"),
            "fec_recovered": self.flexfec_state_total("recovered"),
            "fec_recovery_failed": self.flexfec_state_total("recovery_failed"),
            "fec_unused": self.flexfec_state_total("unused"),
            "fec_invalid": self.flexfec_state_total("invalid"),
            "nack_total": self.counter_total("livekit_nack_total"),
            "packet_loss_total": self.counter_total("livekit_packet_loss_total"),
        }


def shade_bursts(ax, bursts):
    for start, end in bursts:
        ax.axvspan(start, end, color="red", alpha=0.08, lw=0)


def plot(runs, out_dir):
    colors = {"baseline": "#888888", "fec": "#1f77b4"}
    fig, axes = plt.subplots(4, 1, figsize=(14, 16), sharex=True)
    ax_lat, ax_fec, ax_loss, ax_fps = axes
    bursts = runs[-1].bursts

    for run in runs:
        color = colors.get(run.name, None)
        xs, ys = run.latency_series()
        ax_lat.plot(xs, ys, ".", markersize=2.5, label=f"{run.name} latency", color=color, alpha=0.7)
        for x, n in run.frame_gaps():
            ax_lat.axvline(x, color=color or "red", alpha=0.5, lw=min(0.5 + n * 0.3, 3))
    shade_bursts(ax_lat, bursts)
    ax_lat.set_ylabel("capture→receive latency (ms)")
    ax_lat.set_title("Frame latency (vertical lines: frame-id gaps = lost frames, red shading: loss bursts)")
    ax_lat.legend(loc="upper right", fontsize=8)
    ax_lat.grid(alpha=0.3)

    fec_runs = [r for r in runs if r.flexfec_state_total("received") > 0] or runs[-1:]
    for run in fec_runs:
        for state, style in [
            ("received", dict(color="#1f77b4", lw=1)),
            ("recovered", dict(color="#2ca02c", lw=1.8)),
            ("recovery_failed", dict(color="#d62728", lw=1.2)),
            ("unused", dict(color="#9467bd", lw=0.8, alpha=0.6)),
        ]:
            xs, ys = run.flexfec_state_rate(state)
            ax_fec.plot(xs, ys, label=f"{run.name} {state}/s", **style)
    shade_bursts(ax_fec, bursts)
    ax_fec.set_ylabel("FEC packets/s")
    ax_fec.set_title("FlexFEC at the SFU")
    ax_fec.legend(loc="upper right", fontsize=8)
    ax_fec.grid(alpha=0.3)

    for run in runs:
        color = colors.get(run.name, None)
        xs, ys = run.counter_rate("livekit_nack_total")
        ax_loss.plot(xs, ys, label=f"{run.name} nack/s", color=color, lw=1.2)
        xs, ys = run.counter_rate("livekit_packet_loss_total")
        ax_loss.plot(xs, ys, label=f"{run.name} packet_loss/s", color=color, lw=1.2, ls="--", alpha=0.7)
    shade_bursts(ax_loss, bursts)
    ax_loss.set_ylabel("packets/s")
    ax_loss.set_title("NACK and reported packet loss")
    ax_loss.legend(loc="upper right", fontsize=8)
    ax_loss.grid(alpha=0.3)

    for run in runs:
        color = colors.get(run.name, None)
        xs, ys = run.fps_series()
        ax_fps.plot(xs, ys, label=f"{run.name} fps", color=color, lw=1.2)
    shade_bursts(ax_fps, bursts)
    ax_fps.set_ylabel("frames/s")
    ax_fps.set_xlabel("seconds since measurement start")
    ax_fps.set_title("Delivered frame rate at subscriber")
    ax_fps.legend(loc="lower right", fontsize=8)
    ax_fps.grid(alpha=0.3)

    meta = runs[-1].meta
    if meta.get("DEBUG_DROP", "0") not in ("0", ""):
        profile = "uniform {}% loss injected at SFU receive".format(meta["DEBUG_DROP"])
    elif meta.get("SHAPING") == "0":
        profile = "no loss"
    else:
        profile = "base loss {} / burst {} for {}s every {}s".format(
            meta.get("BASE_LOSS", "?"),
            meta.get("BURST_LOSS", "?"),
            meta.get("BURST_LEN", "?"),
            meta.get("BURST_EVERY", "?"),
        )
    fig.suptitle(
        "FlexFEC publisher→SFU recovery — {}, codec {}".format(profile, meta.get("CODEC", "?")),
        fontsize=12,
    )
    fig.tight_layout(rect=(0, 0, 1, 0.985))
    out_path = os.path.join(out_dir, "fec_report.png")
    fig.savefig(out_path, dpi=130)
    return out_path


def print_summary(runs):
    summaries = [(run.name, run.summary()) for run in runs]
    keys = [
        ("frames_received", "{:.0f}"),
        ("frames_expected", "{:.0f}"),
        ("frames_lost", "{:.0f}"),
        ("gap_events", "{:.0f}"),
        ("latency_mean_ms", "{:.1f}"),
        ("latency_p50_ms", "{:.1f}"),
        ("latency_p95_ms", "{:.1f}"),
        ("latency_p99_ms", "{:.1f}"),
        ("fec_received", "{:.0f}"),
        ("fec_recovery_attempts", "{:.0f}"),
        ("fec_recovered", "{:.0f}"),
        ("fec_recovery_failed", "{:.0f}"),
        ("fec_unused", "{:.0f}"),
        ("fec_invalid", "{:.0f}"),
        ("nack_total", "{:.0f}"),
        ("packet_loss_total", "{:.0f}"),
    ]

    name_w = 24
    header = "metric".ljust(name_w) + "".join(name.rjust(16) for name, _ in summaries)
    print(header)
    print("-" * len(header))
    for key, fmt in keys:
        row = key.ljust(name_w)
        for _, summary in summaries:
            row += fmt.format(summary[key]).rjust(16)
        print(row)

    if len(summaries) == 2:
        base, fec = summaries[0][1], summaries[1][1]
        print()
        if fec["frames_lost"] < base["frames_lost"]:
            print(
                "frames lost reduced {} -> {} with FlexFEC".format(
                    int(base["frames_lost"]), int(fec["frames_lost"])
                )
            )
        if fec["fec_recovered"] > 0:
            print("SFU recovered {} packets via FlexFEC".format(int(fec["fec_recovered"])))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", action="append", required=True, help="run directory (repeatable)")
    parser.add_argument("--out", required=True, help="output directory for the report")
    args = parser.parse_args()

    runs = [Run(path) for path in args.run]
    for run in runs:
        if not run.frames:
            print(f"warning: no frames recorded for {run.path}", file=sys.stderr)

    out_path = plot(runs, args.out)
    print(f"report: {out_path}")
    print()
    print_summary(runs)


if __name__ == "__main__":
    main()
