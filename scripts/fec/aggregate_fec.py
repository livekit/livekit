#!/usr/bin/env python3
"""Aggregate a FlexFEC parameter sweep into a single comparison report.

Reads the cells produced by sweep_fec.sh (a manifest.tsv plus one run directory
per cell) and emits:
  sweep_report.png   - loss/latency/recovery vs loss level, baseline vs each config
  sweep_summary.csv  - one row per cell with the key metrics
  a markdown table on stdout

Usage:
  aggregate_fec.py --sweep <sweep_dir>
"""

import argparse
import csv
import os
import sys

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

# Run lives in plot_fec.py next to this script
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from plot_fec import Run  # noqa: E402


def read_manifest(sweep_dir):
    rows = []
    with open(os.path.join(sweep_dir, "manifest.tsv")) as f:
        for row in csv.DictReader(f, delimiter="\t"):
            rows.append(row)
    return rows


def collect(sweep_dir):
    records = []
    for entry in read_manifest(sweep_dir):
        run_dir = os.path.join(sweep_dir, entry["leaf"])
        if not os.path.isdir(run_dir):
            print(f"warning: missing run dir {run_dir}", file=sys.stderr)
            continue
        run = Run(run_dir)
        if not run.frames:
            print(f"warning: no frames for {entry['leaf']}, skipping", file=sys.stderr)
            continue
        s = run.summary()
        try:
            loss = float(entry["loss"])
        except ValueError:
            loss = 0.0
        expected = s["frames_expected"] or 1
        rec = {
            "leaf": entry["leaf"],
            "mode": entry["mode"],
            "loss": loss,
            "loss_label": entry["loss"],
            "fec_rate": entry["fec_rate"],
            "fec_mask": entry["fec_mask"],
            "config": "baseline"
            if entry["mode"] == "baseline"
            else f"FEC r{entry['fec_rate']}/{entry['fec_mask']}",
            "frame_loss_pct": 100.0 * s["frames_lost"] / expected,
            "recovery_rate_pct": (100.0 * s["fec_recovered"] / s["fec_received"]) if s["fec_received"] else 0.0,
            "debug_drop": run.meta.get("DEBUG_DROP", "0"),
            **s,
        }
        records.append(rec)
    return records


def x_unit(records):
    if records and records[0]["debug_drop"] not in ("0", ""):
        return "injected packet drop (%)"
    return "burst loss fraction"


def write_csv(records, path):
    cols = [
        "config", "mode", "loss_label", "fec_rate", "fec_mask",
        "frames_received", "frames_expected", "frames_lost", "frame_loss_pct",
        "latency_p50_ms", "latency_p95_ms", "latency_p99_ms",
        "fec_received", "fec_recovery_attempts", "fec_recovered",
        "fec_recovery_failed", "recovery_rate_pct", "nack_total",
    ]
    with open(path, "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(cols)
        for r in sorted(records, key=lambda r: (r["loss"], r["config"])):
            w.writerow([_fmt(r.get(c)) for c in cols])


def _fmt(v):
    if isinstance(v, float):
        return f"{v:.2f}"
    return v


def series_by_config(records):
    """config label -> sorted [(loss, record)]"""
    by_cfg = {}
    for r in records:
        by_cfg.setdefault(r["config"], []).append(r)
    for cfg in by_cfg:
        by_cfg[cfg].sort(key=lambda r: r["loss"])
    return by_cfg


def plot(records, out_path):
    by_cfg = series_by_config(records)
    unit = x_unit(records)
    # baseline first (gray), FEC configs in a color cycle
    order = sorted(by_cfg, key=lambda c: (c != "baseline", c))
    cmap = plt.get_cmap("viridis")
    fec_cfgs = [c for c in order if c != "baseline"]
    colors = {"baseline": "#888888"}
    for i, c in enumerate(fec_cfgs):
        colors[c] = cmap(0.15 + 0.7 * (i / max(1, len(fec_cfgs) - 1)))

    fig, axes = plt.subplots(2, 2, figsize=(15, 11))
    ax_loss, ax_lat, ax_rec, ax_recn = axes.flat

    def line(ax, key, cfgs):
        for cfg in cfgs:
            pts = by_cfg[cfg]
            xs = [r["loss"] for r in pts]
            ys = [r[key] for r in pts]
            ax.plot(xs, ys, marker="o", label=cfg, color=colors[cfg],
                    lw=2 if cfg != "baseline" else 1.5,
                    ls="-" if cfg != "baseline" else "--")

    line(ax_loss, "frame_loss_pct", order)
    ax_loss.set_title("Frame loss vs loss level")
    ax_loss.set_ylabel("frames lost (%)")
    ax_loss.set_xlabel(unit)
    ax_loss.legend(fontsize=8)
    ax_loss.grid(alpha=0.3)

    line(ax_lat, "latency_p99_ms", order)
    ax_lat.set_title("Tail latency (p99) vs loss level")
    ax_lat.set_ylabel("capture→receive p99 (ms)")
    ax_lat.set_xlabel(unit)
    ax_lat.legend(fontsize=8)
    ax_lat.grid(alpha=0.3)

    line(ax_rec, "recovery_rate_pct", fec_cfgs)
    ax_rec.set_title("FEC recovery rate (recovered / FEC packets received)")
    ax_rec.set_ylabel("recovery rate (%)")
    ax_rec.set_xlabel(unit)
    ax_rec.legend(fontsize=8)
    ax_rec.grid(alpha=0.3)

    line(ax_recn, "fec_recovered", fec_cfgs)
    ax_recn.set_title("Packets recovered by FEC")
    ax_recn.set_ylabel("recovered packets")
    ax_recn.set_xlabel(unit)
    ax_recn.legend(fontsize=8)
    ax_recn.grid(alpha=0.3)

    fig.suptitle("FlexFEC parameter sweep — baseline (no FEC) vs FEC configs", fontsize=13)
    fig.tight_layout(rect=(0, 0, 1, 0.97))
    fig.savefig(out_path, dpi=130)


def print_table(records):
    cols = [
        ("config", "config", "{}"),
        ("loss_label", "loss", "{}"),
        ("frames_lost", "lost", "{:.0f}"),
        ("frame_loss_pct", "lost%", "{:.1f}"),
        ("latency_p95_ms", "p95ms", "{:.1f}"),
        ("latency_p99_ms", "p99ms", "{:.1f}"),
        ("fec_received", "fecRx", "{:.0f}"),
        ("fec_recovered", "recov", "{:.0f}"),
        ("recovery_rate_pct", "recov%", "{:.1f}"),
        ("nack_total", "nacks", "{:.0f}"),
    ]
    widths = [max(len(h), 8) for _, h, _ in cols]
    print("| " + " | ".join(h.ljust(w) for (_, h, _), w in zip(cols, widths)) + " |")
    print("|" + "|".join("-" * (w + 2) for w in widths) + "|")
    for r in sorted(records, key=lambda r: (r["loss"], r["config"] != "baseline", r["config"])):
        cells = []
        for (key, _, fmt), w in zip(cols, widths):
            v = r.get(key)
            cells.append((fmt.format(v) if v is not None else "-").ljust(w))
        print("| " + " | ".join(cells) + " |")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sweep", required=True, help="sweep output directory (contains manifest.tsv)")
    args = parser.parse_args()

    records = collect(args.sweep)
    if not records:
        print("no usable cells found", file=sys.stderr)
        sys.exit(1)

    csv_path = os.path.join(args.sweep, "sweep_summary.csv")
    png_path = os.path.join(args.sweep, "sweep_report.png")
    write_csv(records, csv_path)
    plot(records, png_path)

    print(f"report:  {png_path}")
    print(f"summary: {csv_path}")
    print()
    print_table(records)


if __name__ == "__main__":
    main()
