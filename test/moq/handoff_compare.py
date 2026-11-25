"""Measure two native video publishers across an actual Linux interface switch."""

import argparse
from contextlib import ExitStack, contextmanager
import json
import os
from pathlib import Path
import platform
import secrets
import signal
import subprocess
import sys
import tempfile
import threading
import time

from handoff_metrics import (
    packet_evidence,
    CaptureClock,
    probe_evidence,
    selected_paths,
    summarize,
    write_comparison,
)

SERVER = "10.77.0.1"
SIGNAL, RTC, MOQ = 7880, 7882, 7883
LINKS = {"wifi": 1, "cell": 2, "viewer": 3}


def addr(link, host):
    return f"10.77.{LINKS[link]}.{host}"


def run(*args, check=True):
    result = subprocess.run(args, capture_output=True, text=True)
    if check and result.returncode:
        raise RuntimeError(
            f"{args[0]} failed ({result.returncode}): {result.stderr.strip() or result.stdout.strip()}"
        )
    return result


def timestamp(network, key):
    network["times"][key] = time.monotonic_ns() // 1000
    network["pcap_unix_us"][key] = time.time_ns() // 1000


@contextmanager
def record_clocks(path):
    """The guest wall clock can step while QUIC logs and pcap are being recorded."""
    stop = threading.Event()

    def sample():
        with path.open("w") as f:
            f.write("monotonic_us,unix_us\n")
            while True:
                f.write(f"{time.monotonic_ns() // 1000},{time.time_ns() // 1000}\n")
                if stop.wait(0.005):
                    return

    worker = threading.Thread(target=sample)
    worker.start()
    try:
        yield
    finally:
        stop.set()
        worker.join()


class Topology:
    def __init__(self, stack):
        self.ns = {
            role: "lk-" + secrets.token_hex(5) + "-" + role
            for role in ("pub", "sfu", "view")
        }
        for ns in self.ns.values():
            run("ip", "netns", "add", ns)
            stack.callback(run, "ip", "netns", "del", ns)
            run("ip", "-n", ns, "link", "set", "lo", "up")
        self.ip("sfu", "addr", "add", SERVER + "/32", "dev", "lo")
        for link in LINKS:
            role = self.role(link)
            # Create both ends directly in their private namespaces.
            self.ip(
                role,
                "link",
                "add",
                link,
                "type",
                "veth",
                "peer",
                "name",
                link,
                "netns",
                self.ns["sfu"],
            )
            self.ip("sfu", "addr", "add", addr(link, 1) + "/30", "dev", link)
            self.ip("sfu", "link", "set", link, "up")
            # Discard packets carrying the old interface's source on a different wire.
            self.exec("sfu", "sysctl", "-qw", f"net.ipv4.conf.{link}.rp_filter=1")
        self.enable("viewer")

    @staticmethod
    def role(link):
        return "view" if link == "viewer" else "pub"

    def ip(self, role, *args, check=True):
        return run("ip", "-n", self.ns[role], *args, check=check)

    def cmd(self, role, *args):
        return ["ip", "netns", "exec", self.ns[role], *args]

    def exec(self, role, *args, check=True):
        return run(*self.cmd(role, *args), check=check)

    def configure_link(self, link):
        role = self.role(link)
        self.ip(role, "link", "set", link, "up")
        self.ip(role, "addr", "add", addr(link, 2) + "/30", "dev", link)

    def enable(self, link):
        self.configure_link(link)
        self.ip(
            self.role(link),
            "route",
            "add",
            "default",
            "via",
            addr(link, 1),
            "dev",
            link,
        )

    def prepare_standby(self, link):
        self.configure_link(link)
        self.exec("pub", "sysctl", "-qw", "net.ipv4.ping_group_range=0 0")
        # Only the marked readiness ping can use this route. Both publishers
        # continue using the original default until the measured switch.
        self.exec("pub", "sysctl", "-qw", f"net.ipv4.conf.{link}.rp_filter=2")
        self.ip(
            "pub",
            "route",
            "add",
            "table",
            "77",
            SERVER + "/32",
            "via",
            addr(link, 1),
            "dev",
            link,
            "src",
            addr(link, 2),
        )
        self.ip("pub", "rule", "add", "pref", "90", "fwmark", "77", "lookup", "77")
        # ICE can bind explicitly to a newly enumerated address. Do not let it
        # use that source on the old default route or migrate before the marker.
        self.ip(
            "pub",
            "rule",
            "add",
            "pref",
            "100",
            "from",
            addr(link, 2),
            "ipproto",
            "udp",
            "unreachable",
        )
        self.exec("pub", "ping", "-n", "-m", "77", "-c1", "-W1", SERVER)

    def switch_ready_links(self, old, new):
        # A single replacement keeps a usable default route installed. The
        # following netlink operations retire the old link in the same batch.
        commands = (
            f"route replace default via {addr(new, 1)} dev {new} src {addr(new, 2)}\n"
        )
        if new != old:
            commands = "rule del pref 100\n" + commands
            commands += (
                f"link set dev {old} down\naddr del {addr(old, 2)}/30 dev {old}\n"
            )
        subprocess.run(
            ["ip", "-n", self.ns["pub"], "-batch", "-"],
            input=commands,
            text=True,
            check=True,
        )

    def disable(self, link):
        self.ip("pub", "link", "set", link, "down")
        self.ip("pub", "addr", "del", addr(link, 2) + "/30", "dev", link)
        # Kernel removes the device's routes when the interface goes down.

    def snapshot(self, role="pub"):
        route = self.ip(role, "-j", "route", "get", SERVER, check=False)
        return dict(
            links=json.loads(self.ip(role, "-j", "link", "show").stdout),
            addresses=json.loads(self.ip(role, "-j", "-4", "addr", "show").stdout),
            route=json.loads(route.stdout) if route.returncode == 0 else [],
            ping_ok=self.exec(
                role, "ping", "-n", "-c1", "-W1", SERVER, check=False
            ).returncode
            == 0,
        )

    def check(self, expected, role="pub", standby=None):
        state = self.snapshot(role)
        up = {
            v["ifname"]
            for v in state["links"]
            if v["ifname"] != "lo" and "UP" in v["flags"]
        }
        route = state["route"]
        if expected:
            ok = (
                up == ({expected, standby} if standby else {expected})
                and len(route) == 1
                and route[0]["dev"] == expected
                and route[0]["prefsrc"] == addr(expected, 2)
                and state["ping_ok"]
            )
        else:
            ok = not up and not route and not state["ping_ok"]
        if not ok:
            raise RuntimeError(
                f"unexpected {role} network state, expected {expected}: {state}"
            )
        return state


@contextmanager
def process(command, logfile, env=None, capture=False):
    with logfile.open("w") as out:
        proc = subprocess.Popen(command, stdout=out, stderr=out, env=env)
        try:
            yield proc
        finally:
            if proc.poll() is None:
                proc.send_signal(signal.SIGINT if capture else signal.SIGTERM)
                try:
                    proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()


def wait_for(condition, seconds, procs, what):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        for name, proc in procs.items():
            if proc.poll() is not None:
                raise RuntimeError(
                    f"{name} exited with {proc.returncode} while waiting for {what}"
                )
        if condition():
            return
        time.sleep(0.05)
    raise TimeoutError(f"timed out waiting for {what}")


def server_config(root):
    crt, key = root / "cert.pem", root / "key.pem"
    run(
        "openssl",
        "req",
        "-x509",
        "-newkey",
        "rsa:2048",
        "-nodes",
        "-days",
        "1",
        "-subj",
        f"/CN={SERVER}",
        "-addext",
        f"subjectAltName=IP:{SERVER}",
        "-addext",
        "basicConstraints=critical,CA:FALSE",
        "-keyout",
        key,
        "-out",
        crt,
    )
    run("openssl", "x509", "-in", crt, "-outform", "der", "-out", root / "cert.der")
    conf = root / "server.yaml"
    conf.write_text(f"""port: {SIGNAL}
bind_addresses: ["{SERVER}"]
rtc:
  udp_port: {RTC}
  tcp_port: 0
  use_ice_lite: false
  node_ip: "{SERVER}"
  ips:
    includes: ["{SERVER}/32"]
  enable_loopback_candidate: true
  stun_servers: ["127.0.0.1:3478"]
webtransport:
  port: {MOQ}
  tls_cert_file: "{crt}"
  tls_key_file: "{key}"
moq:
  enabled: true
  endpoint: "moqt://{SERVER}:{MOQ}/moq"
logging:
  level: info
""")
    return conf


def switch_with_ready_standby(topo, net, out, procs, offset):
    start, end = net["initial"], net["alternate"]
    target = start if net["control"] else end
    topo.prepare_standby(end)
    net["states"]["ready"] = topo.check(start, standby=end)
    command = topo.cmd(
        "pub",
        sys.executable,
        Path(__file__).with_name("handoff_probe.py"),
        SERVER,
        str(out),
    )
    with process(command, out / "probe.log") as witness:
        trace = out / "probes.csv"
        wait_for(
            lambda: trace.exists() and trace.stat().st_size > 512,
            5,
            {**procs, "route witness": witness},
            "default-route reachability samples",
        )
        # Stagger cuts across the native SDK's 500 ms route monitor cycle.
        net["pre_switch_offset_s"] = offset
        time.sleep(offset)
        timestamp(net, "cut_us")
        topo.switch_ready_links(start, target)
        timestamp(net, "restored_us")
        net["states"]["restored"] = topo.check(
            target, standby=end if net["control"] else None
        )
        # Continue probing past the route change before collecting the ledger.
        time.sleep(0.05)
        (out / "stop-probes").touch()
        if witness.wait(timeout=2):
            raise RuntimeError("route witness failed")
    net["reachability"] = probe_evidence(out, net, addr(start, 2), addr(end, 2))


def trial(args, out, control, start, end, trial_number):
    out.mkdir()
    net = dict(
        control=control,
        initial=start,
        alternate=end,
        requested_outage_s=args.outage,
        times={},
        pcap_unix_us={},
        states={},
    )
    state = net["states"]
    summary = None
    with ExitStack() as stack:
        topo = Topology(stack)
        topo.enable(start)
        state["before"] = topo.check(start)
        state["viewer_before"] = topo.check("viewer", "view")
        with tempfile.TemporaryDirectory() as td, ExitStack() as processes:
            tmp = Path(td)
            procs = {}
            if args.outage == 0 or args.trace_quic:
                processes.enter_context(record_clocks(out / "clocks.csv"))
            for link in LINKS:
                pk = processes.enter_context(
                    process(
                        topo.cmd(
                            "sfu",
                            "tcpdump",
                            "-n",
                            "--immediate-mode",
                            "-U",
                            "-s",
                            "42",
                            "-y",
                            "EN10MB",
                            "-i",
                            link,
                            "-w",
                            str(out / (link + ".pcap")),
                            f"ip and (icmp or udp port {MOQ} or udp port {RTC} or tcp port {SIGNAL})",
                        ),
                        out / (link + "-capture.log"),
                        capture=True,
                    )
                )
                procs[link + " capture"] = pk
            wait_for(
                lambda: all(
                    "listening on" in (out / (link + "-capture.log")).read_text()
                    for link in LINKS
                ),
                10,
                procs,
                "packet captures",
            )
            if not args.network_only:
                conf = server_config(tmp)
                sfu = processes.enter_context(
                    process(
                        topo.cmd("sfu", args.server_bin, "--dev", "--config", conf),
                        out / "server.log",
                        dict(os.environ, QUIC_GO_LOG_LEVEL="DEBUG")
                        if args.trace_quic
                        else None,
                    )
                )
                procs["sfu"] = sfu

                def started():
                    return (
                        topo.exec(
                            "view",
                            sys.executable,
                            "-c",
                            f"import socket; socket.create_connection(('{SERVER}',{SIGNAL}), .3).close()",
                            check=False,
                        ).returncode
                        == 0
                    )

                wait_for(started, 20, procs, "SFU startup")
                env = dict(
                    os.environ,
                    LK_HANDOFF_OUTPUT=str(out),
                    LK_HANDOFF_ROOM="pair-" + secrets.token_hex(8),
                    LK_HANDOFF_URL=f"ws://{SERVER}:{SIGNAL}",
                    LIVEKIT_MOQ_TEST_CA_DER=str(tmp / "cert.der"),
                )
                for role, name, ready in [
                    ("view", "paired_viewer", "viewer-ready"),
                    ("pub", "paired_publishers", "video-ready"),
                ]:
                    proc = processes.enter_context(
                        process(
                            topo.cmd(
                                role,
                                args.probe_bin,
                                "--ignored",
                                "--exact",
                                name,
                                "--nocapture",
                            ),
                            out / (role + ".log"),
                            env,
                        )
                    )
                    procs[role] = proc
                    wait_for((out / ready).exists, 80, procs, ready)
            # Verify both wires from inside the publisher namespace. The controller and
            # frame generator communicate out of band via the shared filesystem.
            try:
                if args.outage == 0:
                    offset = max(0, trial_number - 1) * 0.5 / args.trials
                    switch_with_ready_standby(topo, net, out, procs, offset)
                else:
                    timestamp(net, "cut_us")
                    deadline = time.monotonic() + args.outage
                    if not control:
                        topo.disable(start)
                    state["blackout"] = topo.check(start if control else None)
                    timestamp(net, "blackout_verified_us")
                    state["viewer_during"] = topo.check("viewer", "view")
                    time.sleep(max(0, deadline - time.monotonic()))
                    if not control:
                        topo.enable(end)
                    timestamp(net, "restored_us")
                    state["restored"] = topo.check(start if control else end)
                if not args.network_only:
                    stop_at = net["times"]["restored_us"] / 1e6 + args.observe
                    wait_for(
                        lambda: time.monotonic() >= stop_at,
                        args.observe + 5,
                        procs,
                        "post-switch recording",
                    )
                    (out / "stop-capture").touch()
                    time.sleep(2)
                    (out / "stop-viewer").touch()
                    for role in ("view", "pub"):
                        if procs[role].wait(timeout=30):
                            raise RuntimeError(
                                f"{role} benchmark failed; see {out / (role + '.log')}"
                            )
                state["viewer_after"] = topo.check("viewer", "view")
            finally:
                (out / "network.json").write_text(json.dumps(net, indent=2) + "\n")
        clock = (
            CaptureClock(out / "clocks.csv") if (out / "clocks.csv").exists() else None
        )
        net["packets"] = packet_evidence(
            out, net, RTC, MOQ, SIGNAL, args.network_only, clock
        )
        if args.trace_quic and not args.network_only and not control:
            net["sfu_selected_path_from_cut_ms"] = selected_paths(
                out / "server.log",
                addr(end, 2),
                net["pcap_unix_us"]["cut_us"],
                clock=clock,
                cut_us=net["times"]["cut_us"],
            )
        (out / "network.json").write_text(json.dumps(net, indent=2) + "\n")
        if not args.network_only:
            summary = summarize(out)
    return summary


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--server-bin", type=Path, help="Linux livekit-server executable")
    p.add_argument(
        "--probe-bin", type=Path, help="Linux moq_handoff_benchmark test executable"
    )
    p.add_argument("--output", type=Path, required=True, help="new output directory")
    p.add_argument(
        "--trials",
        default=3,
        type=int,
        help="switch trials in addition to one no-switch control",
    )
    p.add_argument(
        "--outage",
        default=2.0,
        type=float,
        help="seconds offline; 0 prepares a reachable standby before atomically changing the default route",
    )
    p.add_argument(
        "--observe",
        default=25,
        type=int,
        help="seconds of source video after restoration",
    )
    p.add_argument(
        "--direction",
        choices=["wifi-cell", "cell-wifi", "alternate"],
        default="wifi-cell",
    )
    p.add_argument(
        "--trace-quic",
        action="store_true",
        help="enable SFU QUIC debug logging to time path validation",
    )
    p.add_argument(
        "--network-only",
        action="store_true",
        help="exercise the links and packet captures without media binaries",
    )
    a = p.parse_args()
    if platform.system() != "Linux" or os.geteuid() != 0:
        p.error(
            "run as root inside a Linux test machine or VM with iproute2, ping and tcpdump"
        )
    if (
        not (a.outage == 0 or 0.1 <= a.outage <= 90)
        or not 2 <= a.observe <= 180
        or a.trials < 1
    ):
        p.error("outage must be 0 or 0.1–90 s, observe 2–180 s, and trials positive")
    if not a.network_only:
        for key in ("probe_bin", "server_bin"):
            val = getattr(a, key)
            if val is None or not val.is_file() or not os.access(val, os.X_OK):
                p.error(f"provide an executable --{key.replace('_', '-')}")
            setattr(a, key, str(val.resolve()))
    a.output = a.output.resolve()
    a.output.mkdir(parents=True, exist_ok=False)
    results = {}
    for i in range(a.trials + 1):
        start, end = (
            ("cell", "wifi")
            if a.direction == "cell-wifi"
            or (a.direction == "alternate" and i and i % 2 == 0)
            else ("wifi", "cell")
        )
        label = f"switch-{i}" if i else "control"
        action = f"{start} to {end}" if i else f"{start} stays up"
        print(f"Running {label} ({action})", flush=True)
        results[label] = trial(a, a.output / label, i == 0, start, end, i)
        if not a.network_only:
            write_comparison(a.output, results)


if __name__ == "__main__":
    main()
