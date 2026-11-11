"""Regression tests for the performance gate, without native execution."""

import copy
import importlib.util
from pathlib import Path
import unittest


SPEC = importlib.util.spec_from_file_location("cache_performance", Path(__file__).parents[1] / "cache-performance.py")
PERF = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PERF)


def report(label, seconds=1.0, transferred=1000):
    cases = []
    for workload in ("source-image", "component-workflow"):
        for scenario in sorted(PERF.SCENARIOS):
            for iteration in range(5):
                network = scenario.startswith("registry-")
                cases.append({
                    "workload": workload, "scenario": scenario, "iteration": iteration,
                    "wallSeconds": seconds, "registryBlobGetBytes": transferred if network else 0,
                    "registryRequests": [{"method": "GET", "path": "/v2/cache/blobs/sha256:blob", "status": 200, "readBytes": 0, "writtenBytes": transferred}] if network else [],
                    "status": "passed", "proofs": {"outputCorrect": True, "executionPolicyCorrect": True},
                })
    return {"schema": 1, "label": label, "samples": 5, "fixtureHash": "a" * 64, "seedHash": "b" * 64,
            "resources": {"gomaxprocs": 4}, "engineSHA": ("c" if label == "baseline" else "d") * 64,
            "stokeSHA": "e" * 64, "proofs": {"correct": True}, "cases": cases}


class EvaluationTests(unittest.TestCase):
    def setUp(self):
        self.baseline = report("baseline")
        self.candidate = report("candidate", seconds=0.80)

    def test_measured_wall_gain_passes(self):
        self.assertEqual(PERF.evaluate(self.baseline, self.candidate)["status"], "pass")

    def test_byte_gain_is_separate_from_wall_gain(self):
        result = PERF.evaluate(self.baseline, report("candidate", transferred=500))
        self.assertEqual(result["status"], "pass")
        self.assertEqual({g["kind"] for g in result["gains"]}, {"registry-blob-bytes"})

    def test_no_improvement_fails(self):
        self.assertEqual(PERF.evaluate(self.baseline, report("candidate"))["status"], "fail")

    def test_regression_overrides_improvement(self):
        for case in self.candidate["cases"]:
            if case["scenario"] == "changed-source":
                case["wallSeconds"] = 1.11
        self.assertEqual(PERF.evaluate(self.baseline, self.candidate)["status"], "fail")

    def test_changed_fixture_resources_seed_or_stoke_rejected(self):
        for field in ("fixtureHash", "seedHash", "resources", "stokeSHA"):
            with self.subTest(field=field):
                candidate = copy.deepcopy(self.candidate)
                candidate[field] = {"gomaxprocs": 8} if field == "resources" else "f" * 64
                with self.assertRaises(ValueError):
                    PERF.evaluate(self.baseline, candidate)

    def test_missing_duplicate_and_undersampled_cases_rejected(self):
        for mutation in (lambda c: c["cases"].pop(), lambda c: c["cases"].append(copy.deepcopy(c["cases"][0])), lambda c: c.update(samples=1)):
            candidate = copy.deepcopy(self.candidate)
            mutation(candidate)
            with self.assertRaises(ValueError):
                PERF.evaluate(self.baseline, candidate)

    def test_false_or_removed_correctness_proof_rejected(self):
        for mutation in (lambda c: c["proofs"].update(correct=False), lambda c: c["cases"][0]["proofs"].clear(), lambda c: c["proofs"].update(extra=True)):
            candidate = copy.deepcopy(self.candidate)
            mutation(candidate)
            with self.assertRaises(ValueError):
                PERF.evaluate(self.baseline, candidate)

    def test_invalid_times_and_inconsistent_bytes_rejected(self):
        for mutation in (lambda c: c["cases"][0].update(wallSeconds=float("nan")), lambda c: c["cases"][0].update(wallSeconds=-1), lambda c: c["cases"][0].update(registryBlobGetBytes=42)):
            candidate = copy.deepcopy(self.candidate)
            mutation(candidate)
            with self.assertRaises(ValueError):
                PERF.evaluate(self.baseline, candidate)

    def test_same_engine_cannot_pass_from_measurement_noise(self):
        self.candidate["engineSHA"] = self.baseline["engineSHA"]
        with self.assertRaises(ValueError):
            PERF.evaluate(self.baseline, self.candidate)


if __name__ == "__main__":
    unittest.main()
