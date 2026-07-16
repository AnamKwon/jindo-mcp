import importlib.util
import sys
import unittest
from pathlib import Path


HERE = Path(__file__).parent
spec = importlib.util.spec_from_file_location("adaptive_run", HERE / "adaptive_run.py")
adaptive = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = adaptive
spec.loader.exec_module(adaptive)


class AdaptiveCampaignTests(unittest.TestCase):
    def test_screen_selection_keeps_provider_diversity_before_extra_fast_models(self):
        rows = [
            {"task": "t", "model": "codex:fast", "passed": True, "exit": 0, "secs": 1},
            {"task": "t", "model": "codex:second", "passed": True, "exit": 0, "secs": 2},
            {"task": "t", "model": "claude:slow", "passed": True, "exit": 0, "secs": 9},
            {"task": "t", "model": "agy:failed", "passed": False, "exit": 0, "secs": 1},
        ]
        self.assertEqual(
            adaptive.select_finalists(rows, "t", 3),
            ["codex:fast", "claude:slow", "codex:second"],
        )

    def test_stability_requires_every_repeat_to_pass_without_transport_failure(self):
        rows = [
            {"task": "t", "model": "codex:a", "repeat": 0, "passed": True, "exit": 0},
            {"task": "t", "model": "codex:a", "repeat": 1, "passed": True, "exit": 0},
            {"task": "t", "model": "agy:b", "repeat": 0, "passed": True, "exit": 0},
            {"task": "t", "model": "agy:b", "repeat": 1, "passed": True, "exit": -1},
        ]
        self.assertEqual(adaptive.stable_finalists(rows, "t", ["codex:a", "agy:b"], 2), ["codex:a"])

    def test_promotion_requires_a_fresh_pass_and_clean_review(self):
        rows = [
            {"model": "a", "passed": True, "exit": 0, "reviews": [{"critical_findings": 0, "score": 8}]},
            {"model": "b", "passed": True, "exit": 0, "reviews": [{"critical_findings": 1, "score": 9}]},
            {"model": "c", "passed": False, "exit": 0, "reviews": []},
        ]
        got = adaptive.reviewed_outcomes(rows)
        self.assertEqual([row["promotion_eligible"] for row in got], [True, False, False])


if __name__ == "__main__":
    unittest.main()
