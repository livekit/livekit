"""Measure SFU reachability using the publisher's current default route."""

import csv
import socket
import struct
import sys
import time
from pathlib import Path


def probe(server, directory):
    stop = directory / "stop-probes"
    with (directory / "probes.csv").open("w", buffering=1) as out:
        writer = csv.writer(out)
        writer.writerow(["seq", "send_us", "source", "reply_us", "error"])
        seq = 0
        while not stop.exists():
            seq += 1
            start = time.monotonic_ns() // 1000
            source = ""
            try:
                # A fresh ping socket asks Linux for the currently preferred
                # source. Linux sets the ICMP checksum and socket identifier.
                with socket.socket(
                    socket.AF_INET, socket.SOCK_DGRAM, socket.IPPROTO_ICMP
                ) as s:
                    s.settimeout(0.05)
                    s.connect((server, 0))
                    source = s.getsockname()[0]
                    query = struct.pack("!BBHHHQ", 8, 0, 0, 0, seq % 65536, start)
                    s.send(query)
                    reply = s.recv(64)
                    end = time.monotonic_ns() // 1000
                    if (
                        len(reply) != len(query)
                        or reply[0] != 0
                        or reply[6:] != query[6:]
                    ):
                        raise ValueError("unexpected echo reply")
                writer.writerow([seq, start, source, end, ""])
            except (OSError, ValueError) as exc:
                writer.writerow([seq, start, source, "", str(exc)])
            time.sleep(max(0, (start + 2000 - time.monotonic_ns() // 1000) / 1e6))


if __name__ == "__main__":
    probe(sys.argv[1], Path(sys.argv[2]))
