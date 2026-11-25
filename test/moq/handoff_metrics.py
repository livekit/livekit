"""Join decoded watermarks to captured frames and measure receiver-visible continuity."""

import csv
from collections import Counter, deque
from datetime import datetime
import json
import math
import re


def rows(path):
    with path.open(newline="") as source:
        return list(csv.DictReader(source))


def percentile(values, p):
    return (
        round(sorted(values)[math.ceil(len(values) * p) - 1] / 1000, 2)
        if values
        else None
    )


class CaptureClock:
    """Map realtime into the guest monotonic clock only within an unambiguous span."""

    def __init__(self, path):
        self.spans = []
        for row in rows(path):
            mono, wall = int(row["monotonic_us"]), int(row["unix_us"])
            if self.spans:
                start, last = self.spans[-1]
                if abs((wall - last[1]) - (mono - last[0])) <= 1000 and wall > last[1]:
                    self.spans[-1] = (start, (mono, wall))
                    continue
            self.spans.append(((mono, wall), (mono, wall)))

    def elapsed_ms(self, wall_us, origin_us):
        hits = [(a, b) for a, b in self.spans if a[1] <= wall_us <= b[1] and a != b]
        if len(hits) != 1:
            return None
        (m0, w0), (m1, w1) = hits[0]
        mono = m0 + (wall_us - w0) * (m1 - m0) / (w1 - w0)
        return round((mono - origin_us) / 1000, 3)


def probe_evidence(directory, network, initial_ip, standby_ip):
    samples = rows(directory / "probes.csv")
    cut, restored = (network["times"][k] for k in ("cut_us", "restored_us"))
    active = initial_ip if network["control"] else standby_ip
    before = [s for s in samples if s["reply_us"] and int(s["reply_us"]) < cut]
    after = [s for s in samples if int(s["send_us"]) > restored and s["reply_us"]]
    if (
        not before
        or not after
        or any(s["source"] != initial_ip for s in before)
        or any(s["source"] != active for s in after)
    ):
        raise ValueError(
            "default-route echo samples do not bracket the switch on the expected addresses"
        )
    replies = [int(s["reply_us"]) for s in samples if s["reply_us"]]
    return dict(
        sent=len(samples),
        missed=len(samples) - len(replies),
        max_reply_gap_ms=round(
            max(b - a for a, b in zip(replies, replies[1:])) / 1000, 3
        ),
        operation_ms=(restored - cut) / 1000,
    )


def selected_paths(logfile, alternate_ip, cut_unix_us, *, clock=None, cut_us=None):
    """Time the SFU's actual path selection. The server log has millisecond precision."""
    events = {}
    quic = re.compile(
        r"server switching to path \d+ \(" + re.escape(alternate_ip) + r":\d+\)"
    )
    with logfile.open() as lines:
        for line in lines:
            if quic.search(line):
                role = "moq"
            elif "ice reconnected or switched pair\t" in line:
                fields = json.loads(line.rsplit("\t", 1)[1])
                if (
                    fields.get("participant") != "webrtc"
                    or fields.get("newPair", {}).get("remoteAddress") != alternate_ip
                ):
                    continue
                role = "webrtc"
            else:
                continue
            at = datetime.strptime(line.split("\t", 1)[0], "%Y-%m-%dT%H:%M:%S.%f%z")
            us = round(at.timestamp() * 1e6)
            elapsed = (
                clock.elapsed_ms(us, cut_us)
                if clock
                else round((us - cut_unix_us) / 1000, 3)
            )
            # Preserve an untimeable first selection rather than attributing a
            # later port change to the original switch. Log precision is 1 ms.
            if elapsed is None or elapsed >= -1:
                events.setdefault(role, elapsed)
    return events


def summarize(directory):
    captures = rows(directory / "source.csv")
    decoded = rows(directory / "decoded.csv")
    network = json.loads((directory / "network.json").read_text())
    transition = "route_change" if network.get("requested_outage_s") == 0 else "outage"
    cut, restored = (network["times"][key] for key in ("cut_us", "restored_us"))
    sent = {int(r["id"]): int(r["capture_monotonic_us"]) for r in captures}
    relative = {int(r["id"]): int(r["relative_us"]) for r in captures}
    if list(sent) != list(range(len(captures))) or sorted(sent.values()) != list(
        sent.values()
    ):
        raise ValueError("nonconsecutive IDs or nonmonotonic source clock")
    result = {}
    paired = {}
    for role in ("webrtc", "moq"):
        seen = {}
        count = Counter()
        invalid = reordered = 0
        advances = []
        high = -1
        for row in decoded:
            if row["publisher"] != role:
                continue
            if not row["id"]:
                invalid += 1
                continue
            fid, mark, rx = map(
                int, (row["id"], row["watermark_us"], row["received_monotonic_us"])
            )
            if relative.get(fid) != mark or rx < sent[fid]:
                raise ValueError(
                    f"{role}: decoded watermark does not match capture ledger"
                )
            count[fid] += 1
            if fid in seen:
                continue
            seen[fid] = rx
            if fid <= high:
                reordered += 1
                continue
            high = fid
            advances.append((fid, rx))
        paired[role] = seen
        before = [rx for _, rx in advances if rx < cut]
        if not before:
            raise ValueError(f"{role}: no baseline")
        fresh = [(i, rx) for i, rx in advances if sent[i] >= restored]
        first = fresh[0] if fresh else None
        live = next((rx for fid, rx in fresh if rx - sent[fid] <= 250000), None)
        active = network["initial"] if network["control"] else network["alternate"]
        if first and not network["packets"].get(f"after/{active}/{role}/up"):
            raise ValueError(
                f"{role}: decoded fresh video without publisher traffic on {active}"
            )
        until = first[1] if first else max(sent.values())
        progress = [before[-1]] + [rx for _, rx in advances if cut <= rx <= until]
        if not first:
            progress.append(until)
        last_capture = max(sent.values())
        full_progress = [
            before[-1],
            *(rx for _, rx in advances if cut <= rx <= last_capture),
            last_capture,
        ]
        sustained = None
        window = deque()
        for fid, rx in fresh:
            if rx - sent[fid] > 250000:
                window.clear()
                continue
            if window and rx - window[-1] > 150000:
                window.clear()
            window.append(rx)
            while rx - window[0] > 1000000:
                window.popleft()
            if len(window) >= 24 and rx - window[0] >= 900000:
                sustained = rx
                break
        intervals = {
            "baseline": (cut - 2000000, cut),
            transition: (cut, restored),
            "post_restore": (restored, max(sent.values()) + 1),
        }
        segments = {}
        for kind, (lo, hi) in intervals.items():
            ids = [i for i, tx in sent.items() if lo <= tx < hi]
            delays = [seen[i] - sent[i] for i in ids if i in seen]
            segments[kind] = dict(
                captured=len(ids),
                missing=len(ids) - len(delays),
                source_rejected=sum(
                    captures[i][role + "_accepted"] == "false" for i in ids
                ),
                delayed_over_250ms=sum(d > 250000 for d in delays),
                latency_p50_ms=percentile(delays, 0.5),
                latency_p95_ms=percentile(delays, 0.95),
            )
        bypass = [
            i
            for i, rx in seen.items()
            if cut + 20000 <= sent[i] <= rx < restored - 20000
        ]
        if bypass and not network["control"] and network.get("requested_outage_s") != 0:
            raise ValueError(
                f"{role}: decoded {len(bypass)} frames captured inside the bidirectional blackout"
            )
        result[role] = dict(
            first_new_udp_from_cut_ms=network.get("first_new_udp_from_cut_ms", {}).get(
                role
            ),
            sfu_selected_path_from_cut_ms=network.get(
                "sfu_selected_path_from_cut_ms", {}
            ).get(role),
            first_fresh_id=first[0] if first else None,
            first_under_250ms_after_restore_ms=round((live - restored) / 1000, 2)
            if live is not None
            else None,
            first_fresh_after_restore_ms=round((first[1] - restored) / 1000, 2)
            if first
            else None,
            first_fresh_after_cut_ms=round((first[1] - cut) / 1000, 2)
            if first
            else None,
            sustained_after_restore_ms=round((sustained - restored) / 1000, 2)
            if sustained
            else None,
            longest_progress_gap_to_fresh_ms=round(
                max((b - a for a, b in zip(progress, progress[1:])), default=0) / 1000,
                2,
            ),
            longest_gap_through_capture_end_ms=round(
                max(b - a for a, b in zip(full_progress, full_progress[1:])) / 1000, 2
            ),
            observation_after_restore_ms=round(
                (max(sent.values()) - restored) / 1000, 2
            ),
            missing_entire_run=len(sent) - len(seen),
            invalid_watermarks=invalid,
            duplicates=sum(v - 1 for v in count.values()),
            out_of_order=reordered,
            source_rejections=sum(r[role + "_accepted"] == "false" for r in captures),
            segments=segments,
        )
    with (directory / "frames.csv").open("w", newline="") as dest:
        writer = csv.writer(dest)
        writer.writerow(
            [
                "id",
                "capture_monotonic_us",
                "phase",
                "webrtc_first_decode_us",
                "moq_first_decode_us",
                "webrtc_latency_ms",
                "moq_latency_ms",
            ]
        )
        for fid, tx in sent.items():
            times = [paired[k].get(fid) for k in ("webrtc", "moq")]
            writer.writerow(
                [
                    fid,
                    tx,
                    "before" if tx < cut else transition if tx < restored else "after",
                    *times,
                    *[
                        round((rx - tx) / 1000, 3) if rx is not None else None
                        for rx in times
                    ],
                ]
            )
    (directory / "summary.json").write_text(json.dumps(result, indent=2) + "\n")
    for role, measured in result.items():
        fresh_ms = measured["first_fresh_after_restore_ms"]
        recovery = (
            "no fresh frame" if fresh_ms is None else f"fresh at +{fresh_ms:.1f} ms"
        )
        interval = measured["segments"][transition]
        print(
            f"  {role}: {recovery} from restore, missed {interval['missing']}/{interval['captured']} frames during {transition.replace('_', ' ')}",
            flush=True,
        )
    return result


def packet_evidence(
    directory, network, rtc_port, moq_port, signal_port, network_only, clock=None
):
    """Read classic Ethernet pcap headers. The fixture truncates before any UDP payload."""
    import socket
    import struct

    cut, up = (network["pcap_unix_us"][k] for k in ("cut_us", "restored_us"))
    verified = network["pcap_unix_us"].get("blackout_verified_us")
    ready_switch = network.get("requested_outage_s") == 0
    initial, alternate = network["initial"], network["alternate"]
    labels = {rtc_port: "webrtc", moq_port: "moq", signal_port: "signaling"}
    peers = {"wifi": "10.77.1.2", "cell": "10.77.2.2", "viewer": "10.77.3.2"}
    counters = Counter()
    middle = "route_change" if ready_switch else "outage"
    first_ingress = {}
    for link, peer in peers.items():
        stats = (directory / (link + "-capture.log")).read_text()
        if "0 packets dropped by kernel" not in stats.splitlines():
            raise ValueError(f"{link}: packet capture incomplete; see capture log")
        with (directory / (link + ".pcap")).open("rb") as stream:
            header = stream.read(24)
            orders = {b"\xd4\xc3\xb2\xa1": "<", b"\xa1\xb2\xc3\xd4": ">"}
            order = orders.get(header[:4])
            if not order or struct.unpack_from(order + "I", header, 20)[0] != 1:
                raise ValueError(
                    "expected classic Ethernet pcap with microsecond timestamps"
                )
            while record := stream.read(16):
                sec, us, caplen, size = struct.unpack(order + "IIII", record)
                pkt = stream.read(caplen)
                if len(pkt) != caplen or len(pkt) < 38 or pkt[12:14] != b"\x08\x00":
                    raise ValueError(f"{link}: incomplete or unexpected Ethernet frame")
                t = sec * 1000000 + us
                src, dst = socket.inet_ntoa(pkt[26:30]), socket.inet_ntoa(pkt[30:34])
                if "10.77.0.1" not in (src, dst):
                    continue
                if peer not in (src, dst):
                    raise ValueError(f"{link}: unexpected peer {src} to {dst}")
                inbound = src == peer
                if pkt[23] == 1:
                    role = "ping"
                else:
                    h = 14 + (pkt[14] & 15) * 4
                    sp, dp = struct.unpack_from("!HH", pkt, h)
                    role = labels[dp if inbound else sp]
                phase = "before" if t < cut else middle if t < up else "after"
                if (
                    ready_switch
                    and network["control"]
                    and link == alternate
                    and inbound
                    and role != "ping"
                ):
                    raise ValueError(
                        "control publisher sent transport traffic over the standby uplink"
                    )
                if not network["control"] and link != "viewer" and inbound:
                    if (
                        (not ready_switch and verified < t < up - 50000)
                        or (
                            t < cut
                            and link != initial
                            and not (ready_switch and role == "ping")
                        )
                        or (t > up + 50000 and link != alternate)
                    ):
                        raise ValueError(f"packet on unexpected {link} uplink at {t}")
                    if link == alternate and t >= cut and role in ("moq", "webrtc"):
                        if role not in first_ingress:
                            first_ingress[role] = (
                                clock.elapsed_ms(t, network["times"]["cut_us"])
                                if clock
                                else (t - cut) / 1000
                            )
                counters[f"{phase}/{link}/{role}/{'up' if inbound else 'down'}"] += size
    active = initial if network["control"] else alternate
    if (
        counters[f"after/{active}/ping/up"] == 0
        or counters["after/viewer/ping/up"] == 0
    ):
        raise ValueError(
            "pcap did not observe the restored publisher path and the stable viewer"
        )
    if not network_only:
        for role in ("moq", "webrtc"):
            if counters[f"before/{initial}/{role}/up"] < 10000:
                raise ValueError(f"{role} media traffic missing before the switch")
    if ready_switch:
        if not all(
            counters[f"before/{alternate}/ping/{direction}"]
            for direction in ("up", "down")
        ):
            raise ValueError("standby path was not proved reachable before switching")
        network["first_new_udp_from_cut_ms"] = first_ingress
    return dict(counters)


def write_comparison(directory, trials):
    with (directory / "comparison.csv").open("w", newline="") as dest:
        writer = csv.writer(dest)
        writer.writerow(
            [
                "trial",
                "publisher",
                "fresh_after_cut_ms",
                "fresh_after_restore_ms",
                "first_under_250ms_after_restore_ms",
                "sustained_after_restore_ms",
                "longest_gap_ms",
                "baseline_missing",
                "outage_missing",
                "outage_captured",
                "post_missing",
                "post_captured",
                "post_p50_ms",
                "post_p95_ms",
                "first_new_udp_from_cut_ms",
                "sfu_selected_path_from_cut_ms",
                "longest_gap_through_capture_end_ms",
                "route_change_missing",
                "route_change_captured",
            ]
        )
        for label, data in trials.items():
            for role, result in data.items():
                segments = result["segments"]
                after = segments["post_restore"]
                outage = segments.get("outage", {})
                change = segments.get("route_change", {})
                writer.writerow(
                    [
                        label,
                        role,
                        result["first_fresh_after_cut_ms"],
                        result["first_fresh_after_restore_ms"],
                        result["first_under_250ms_after_restore_ms"],
                        result["sustained_after_restore_ms"],
                        result["longest_progress_gap_to_fresh_ms"],
                        segments["baseline"]["missing"],
                        outage.get("missing", 0),
                        outage.get("captured", 0),
                        after["missing"],
                        after["captured"],
                        after["latency_p50_ms"],
                        after["latency_p95_ms"],
                        result["first_new_udp_from_cut_ms"],
                        result["sfu_selected_path_from_cut_ms"],
                        result["longest_gap_through_capture_end_ms"],
                        change.get("missing", 0),
                        change.get("captured", 0),
                    ]
                )


if __name__ == "__main__":
    import argparse
    from pathlib import Path

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "directory",
        type=Path,
        help="benchmark output containing the control and switch trials",
    )
    root = parser.parse_args().directory
    switches = sorted(
        root.glob("switch-*"), key=lambda path: int(path.name.split("-")[-1])
    )
    write_comparison(
        root, {p.name: summarize(p) for p in [root / "control", *switches]}
    )
