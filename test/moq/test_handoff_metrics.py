"""Regression checks for measurements that distinguish a fresh frame from backlog."""

import csv
import json
from pathlib import Path
import tempfile
import unittest

from handoff_metrics import CaptureClock, probe_evidence, selected_paths, summarize


class HandoffMetricsTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.out = Path(temp.name)
        net = {
            "control": False,
            "initial": "wifi",
            "alternate": "cell",
            "times": {"cut_us": 1_000_000, "restored_us": 2_000_000},
            "packets": {"after/cell/webrtc/up": 10240, "after/cell/moq/up": 10240},
        }
        (self.out / "network.json").write_text(json.dumps(net))
        self.tx = [900000, 1100000, 1600000, 2001000, 2500000]
        self.csv(
            "source.csv",
            [
                "id",
                "relative_us",
                "capture_monotonic_us",
                "webrtc_accepted",
                "moq_accepted",
            ],
            [[i, t - self.tx[0], t, "true", "true"] for i, t in enumerate(self.tx)],
        )

    def csv(self, name, header, rows):
        with (self.out / name).open("w", newline="") as out:
            writer = csv.writer(out)
            writer.writerow(header)
            writer.writerows(rows)

    def decode(self, ids):
        self.csv(
            "decoded.csv",
            ["publisher", "track_sid", "id", "watermark_us", "received_monotonic_us"],
            [
                [role, "track", i, self.tx[i] - self.tx[0], rx]
                for role in ("webrtc", "moq")
                for i, rx in ids
            ],
        )

    def test_late_outage_frame_and_duplicate_do_not_count_as_recovery(self):
        self.decode(
            [
                (0, 910000),
                (2, 2005000),
                (2, 2010000),
                (1, 2020000),
                (3, 2400000),
                (4, 2510000),
            ]
        )
        result = summarize(self.out)["moq"]
        self.assertEqual(result["first_fresh_after_restore_ms"], 400)
        self.assertEqual(result["first_under_250ms_after_restore_ms"], 510)
        self.assertEqual(result["segments"]["outage"]["missing"], 0)
        self.assertEqual(result["duplicates"], 1)
        self.assertEqual(result["out_of_order"], 1)
        self.assertIsNone(result["sustained_after_restore_ms"])

    def test_stream_that_never_resumes_is_censored(self):
        self.decode([(0, 910000)])
        result = summarize(self.out)["webrtc"]
        self.assertIsNone(result["first_fresh_after_restore_ms"])
        self.assertIsNone(result["first_under_250ms_after_restore_ms"])
        self.assertEqual(result["segments"]["post_restore"]["missing"], 2)

    def test_media_bypassing_the_cut_invalidates_trial(self):
        self.decode([(0, 910000), (1, 1110000)])
        with self.assertRaisesRegex(ValueError, "blackout"):
            summarize(self.out)

    def test_frames_from_the_route_replacement_are_measured_as_a_transition(self):
        path = self.out / "network.json"
        network = json.loads(path.read_text())
        network["requested_outage_s"] = 0
        path.write_text(json.dumps(network))
        self.decode([(0, 910000), (1, 1110000), (3, 2010000)])
        result = summarize(self.out)["moq"]
        self.assertEqual(result["segments"]["route_change"]["missing"], 1)
        self.assertNotIn("outage", result["segments"])
        self.assertEqual(result["first_fresh_after_restore_ms"], 10)

    def test_zero_outage_requires_evidence_of_both_routes_and_counts_probe_loss(self):
        net = {"control": False, "times": {"cut_us": 3000, "restored_us": 4000}}
        self.csv(
            "probes.csv",
            ["send_us", "reply_us", "source"],
            [
                [1000, 1010, "10.77.1.2"],
                [3100, "", ""],
                [5000, 5010, "10.77.2.2"],
            ],
        )
        evidence = probe_evidence(self.out, net, "10.77.1.2", "10.77.2.2")
        self.assertEqual(evidence["missed"], 1)
        self.assertEqual(evidence["max_reply_gap_ms"], 4)
        with self.assertRaisesRegex(ValueError, "expected addresses"):
            probe_evidence(self.out, net, "10.77.1.2", "10.77.9.2")

    def test_selection_must_be_on_the_replacement_path(self):
        log = self.out / "server.log"
        log.write_text(
            "1970-01-01T00:00:01.020+0000\tINFO\tserver switching to path 0 (10.77.1.2:5000)\n"
            "1970-01-01T00:00:01.021+0000\tINFO\tserver enqueueing PATH_CHALLENGE for new path 10.77.2.2:5000\n"
            "1970-01-01T00:00:01.042+0000\tINFO\tserver switching to path 1 (10.77.2.2:5000)\n"
            "1970-01-01T00:00:01.080+0000\tINFO\tice reconnected or switched pair\t"
            '{"participant":"webrtc","newPair":{"remoteAddress":"10.77.2.2"}}\n'
        )
        self.assertEqual(
            selected_paths(log, "10.77.2.2", 1000000), {"moq": 42, "webrtc": 80}
        )

    def test_capture_clock_accounts_for_a_forward_vm_clock_step(self):
        self.csv(
            "clocks.csv",
            ["monotonic_us", "unix_us"],
            [
                [1000, 1001000],
                [6000, 1006000],
                [11000, 1161000],
                [16000, 1166000],
            ],
        )
        clock = CaptureClock(self.out / "clocks.csv")
        self.assertEqual(clock.elapsed_ms(1163000, 1000), 12)
        self.assertIsNone(clock.elapsed_ms(1080000, 1000))

    def test_repeated_wall_time_remains_unresolved(self):
        self.csv(
            "clocks.csv",
            ["monotonic_us", "unix_us"],
            [
                [0, 1000000],
                [10000, 1010000],
                [15000, 1005000],
                [25000, 1015000],
            ],
        )
        self.assertIsNone(CaptureClock(self.out / "clocks.csv").elapsed_ms(1008000, 0))
