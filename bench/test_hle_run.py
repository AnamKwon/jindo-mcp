import importlib.util
import sys
import unittest
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))
spec = importlib.util.spec_from_file_location("hle_run", HERE / "hle_run.py")
hle = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = hle
spec.loader.exec_module(hle)


class HLERunnerTests(unittest.TestCase):
    def test_parse_answers_ignores_noise_and_rejects_duplicates(self):
        got = hle.parse_answers('noise {"answers":[{"id":"1","answer":"B"},{"id":"2","answer":"False"}]} tail')
        self.assertEqual(got, {"1": "B", "2": "False"})
        self.assertEqual(hle.parse_answers('{"answers":[{"id":"1","answer":"A"},{"id":"1","answer":"B"}]}'), {})

    def test_normalize_only_allows_small_format_variations(self):
        self.assertEqual(hle.normalize_answer("b."), "B")
        self.assertEqual(hle.normalize_answer(" false "), "False")

    def test_score_batch_penalizes_missing_answers(self):
        items = [
            {"id": "1", "domain": "biology", "answer": "B"},
            {"id": "2", "domain": "physics", "answer": "True"},
        ]
        scored = hle.score_batch(items, {"1": "B"})
        self.assertTrue(scored[0]["correct"])
        self.assertFalse(scored[1]["correct"])

    def test_summary_is_per_model_and_domain(self):
        rows = [{"model": "m", "items": [
            {"domain": "biology", "correct": True},
            {"domain": "biology", "correct": False},
        ]}]
        self.assertEqual(hle.summarize(rows)[0]["accuracy"], 0.5)


if __name__ == "__main__":
    unittest.main()
