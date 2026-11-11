#!/usr/bin/env python3
"""Bounded unit tests for the Nix example scripts; no Nix builds or evaluation."""

import importlib.util
import json
from pathlib import Path
import tempfile
import sys
import unittest


sys.dont_write_bytecode = True


def module(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    loaded = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(loaded)
    return loaded


generator = module("nix_workflow", "nix-workflow.py")
runner = module("run_nix_check", "run-nix-check.py")


class NixExampleTests(unittest.TestCase):
    def test_inventory_controls_every_native_matrix_variant(self):
        discovery = {"system": "aarch64-linux", "checks": ["build", "new-check"]}
        workflow = generator.make_workflow(discovery)
        task = workflow["tasks"]["nix-checks"]
        self.assertEqual(task["platform"], "linux/arm64")
        self.assertEqual(task["strategy"]["matrix"]["include"], [
            {"check": "build", "system": "aarch64-linux"},
            {"check": "new-check", "system": "aarch64-linux"},
        ])
        self.assertEqual(task["strategy"]["max-parallel"], 1)
        self.assertFalse(task["cache"])
        self.assertNotIn("allow-host", task["host"])
        self.assertIn("reports", task["host"]["outputs"])
        self.assertTrue(all(item["destination"].startswith("source/")
                            for item in task["host"]["inputs"]))
        self.assertEqual(task["steps"][0]["run"][-2:], ["--flake", "path:source"])
        self.assertNotIn(".", [item["path"] for item in task["host"]["inputs"]])

    def test_regeneration_preserves_handwritten_and_edited_files(self):
        data = generator.render(generator.make_workflow({"system": "x86_64-linux", "checks": ["build"]}))
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "workflow.yaml"
            target.write_text("handwritten\n")
            with self.assertRaises(ValueError):
                generator.write_generated(target, data)
            self.assertEqual(target.read_text(), "handwritten\n")
            target.unlink()
            generator.write_generated(target, data)
            generator.write_generated(target, data)
            self.assertEqual(target.read_text(), data)
            target.write_text(data + "# manual edit\n")
            with self.assertRaises(ValueError):
                generator.write_generated(target, data)

    def activity(self, tracker, action, identity, kind=None, target=None, machine=""):
        event = {"action": action, "id": identity}
        if kind is not None:
            fields = [target]
            if kind == 105:
                fields += [machine, 1, 1]
            elif kind == 108:
                fields += ["https://cache.example.invalid"]
            event.update({"type": kind, "fields": fields})
        tracker.observe("@nix " + json.dumps(event))

    def test_exact_target_builder_is_fresh_only_after_success(self):
        tracker = runner.Activities("/nix/store/check.drv", ["/nix/store/output"])
        self.activity(tracker, "start", 1, 105, "/nix/store/dependency.drv")
        self.activity(tracker, "stop", 1)
        self.assertEqual(tracker.classify(0, {})["realization"], "unknown")
        self.activity(tracker, "start", 2, 105, "/nix/store/check.drv")
        self.assertIsNone(tracker.classify(0, {})["freshSuccessfulCheckBuilder"])
        self.activity(tracker, "stop", 2)
        self.assertTrue(tracker.classify(0, {})["freshSuccessfulCheckBuilder"])
        self.assertEqual(tracker.classify(1, {})["realization"], "failed")

    def test_substitution_is_not_a_fresh_check(self):
        tracker = runner.Activities("/nix/store/check.drv", ["/nix/store/output"])
        self.activity(tracker, "start", 1, 108, "/nix/store/dependency-output")
        self.activity(tracker, "stop", 1)
        self.assertEqual(tracker.classify(0, {})["realization"], "unknown")
        self.activity(tracker, "start", 2, 108, "/nix/store/output")
        self.activity(tracker, "stop", 2)
        result = tracker.classify(0, {})
        self.assertEqual(result["realization"], "substituted")
        self.assertFalse(result["freshSuccessfulCheckBuilder"])

    def test_malformed_evidence_prevents_positive_builder_claim(self):
        for bad in ["malformed", json.dumps({"action": "start", "id": True, "type": 105, "fields": []}),
                    json.dumps({"action": "start", "id": 9, "type": 105, "fields": ["/nix/store/check.drv"]}),
                    json.dumps({"action": "stop", "id": True}),
                    json.dumps({"action": "start", "id": 9, "type": 108, "fields": ["/nix/store/output", False]})]:
            with self.subTest(record=bad):
                tracker = runner.Activities("/nix/store/check.drv", ["/nix/store/output"])
                self.activity(tracker, "start", 1, 105, "/nix/store/check.drv")
                self.activity(tracker, "stop", 1)
                tracker.observe("@nix " + bad)
                self.assertEqual(tracker.classify(0, {})["realization"], "unknown")
                self.assertIsNone(tracker.classify(0, {})["freshSuccessfulCheckBuilder"])

    def test_remote_hook_alone_does_not_prove_fresh_builder(self):
        tracker = runner.Activities("/nix/store/check.drv", ["/nix/store/output"])
        self.activity(tracker, "start", 1, 105, "/nix/store/check.drv", machine="ssh://builder")
        self.activity(tracker, "stop", 1)
        result = tracker.classify(0, {})
        self.assertEqual(result["realization"], "unknown")
        self.assertTrue(result["targetRemoteHookActivityObserved"])
        self.assertFalse(result["targetBuilderActivityObserved"])
        self.activity(tracker, "start", 2, 105, "/nix/store/check.drv")
        self.activity(tracker, "stop", 2)
        self.assertTrue(tracker.classify(0, {})["freshSuccessfulCheckBuilder"])

    def test_preexisting_validity_and_unknown_protocol_are_distinct(self):
        tracker = runner.Activities("/nix/store/check.drv", ["/nix/store/output"])
        before = {"/nix/store/output": True}
        self.assertEqual(tracker.classify(0, before)["realization"], "unknown")
        self.activity(tracker, "start", 1, 102, "/nix/store/output")
        result = tracker.classify(0, before)
        self.assertEqual(result["realization"], "already-valid")
        self.assertFalse(result["freshSuccessfulCheckBuilder"])
        tracker.observe("@nix malformed")
        self.assertIsNone(tracker.classify(0, before)["freshSuccessfulCheckBuilder"])


if __name__ == "__main__":
    unittest.main()
