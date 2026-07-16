import importlib.util
import sys
import unittest
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))
spec = importlib.util.spec_from_file_location("reviewer_run", HERE / "reviewer_run.py")
reviewer = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = reviewer
spec.loader.exec_module(reviewer)


class ReviewerRunnerTests(unittest.TestCase):
    def test_parser_and_duplicate_rejection(self):
        text = '{"reviews":[{"id":"x","verdict":"approved","labels":[]}]}'
        self.assertEqual(reviewer.parse_reviews(text)["x"]["verdict"], "approved")
        duplicate = '{"reviews":[{"id":"x","verdict":"approved","labels":[]},{"id":"x","verdict":"defective","labels":[]}]}'
        self.assertEqual(reviewer.parse_reviews(duplicate), {})

    def test_scoring_penalizes_false_positive_and_missing_defect(self):
        cases = [
            {"id":"bad", "expected":["integer_overflow"]},
            {"id":"good", "expected":[]},
        ]
        reviews = {
            "bad":{"verdict":"approved", "labels":[]},
            "good":{"verdict":"defective", "labels":["integer_overflow"]},
        }
        got = reviewer.score_reviews(cases, reviews)
        self.assertEqual(got["critical_recall"], 0)
        self.assertEqual(got["false_positive_rate"], 1)
        self.assertEqual(got["verdict_accuracy"], 0)


if __name__ == "__main__": unittest.main()
