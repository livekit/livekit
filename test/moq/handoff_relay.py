"""Local UDP relay for the two-participant native MoQ handoff test."""

import argparse
import selectors
import socket
import time
from contextlib import ExitStack


def udp_socket(stack, address):
    sock = stack.enter_context(socket.socket(socket.AF_INET, socket.SOCK_DGRAM))
    sock.bind(address)
    return sock


def run(listen_port, sfu_port, outage):
    server = ("127.0.0.1", sfu_port)
    with ExitStack() as stack:
        events = stack.enter_context(selectors.DefaultSelector())
        listener = udp_socket(stack, ("127.0.0.1", listen_port))
        events.register(listener, selectors.EVENT_READ)
        upstream = {}
        resume_at = None
        dropped = 0
        while True:
            for key, _ in events.select():
                packet, peer = key.fileobj.recvfrom(65535)
                if key.fileobj is listener:
                    if peer not in upstream:
                        # The SFU sees a different source address and port for each path.
                        upstream[peer] = udp_socket(stack, (peer[0], 0))
                        events.register(upstream[peer], selectors.EVENT_READ, peer)
                        print(f"Observed client path {len(upstream)}", flush=True)
                        if len(upstream) == 3:
                            # Both clients have delivered video and data before the first move.
                            resume_at = time.monotonic() + outage
                    # The version and packet type are visible on a QUIC v1 long header.
                    if (resume_at is not None and len(packet) >= 7
                            and packet[0] >> 4 == 0xC and packet[1:5] == b"\x00\x00\x00\x01"):
                        raise RuntimeError("Unexpected QUIC Initial after the handoff began")
                    outgoing, destination = upstream[peer], server
                else:
                    outgoing, destination = listener, key.data

                if resume_at is not None and time.monotonic() < resume_at:
                    dropped += 1
                    continue
                if dropped:
                    print(f"Path restored after dropping {dropped} datagrams", flush=True)
                    dropped = 0
                outgoing.sendto(packet, destination)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--listen-port", type=int, required=True)
    parser.add_argument("--sfu-port", type=int, required=True)
    parser.add_argument("--outage", type=float, default=2.0, help="outage duration in seconds")
    args = parser.parse_args()
    try:
        run(args.listen_port, args.sfu_port, args.outage)
    except KeyboardInterrupt:
        pass
