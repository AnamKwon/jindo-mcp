import importlib.util
import sys
import unittest
from pathlib import Path

HERE = Path(__file__).parent
spec = importlib.util.spec_from_file_location("benchmark_matrix", HERE / "matrix.py")
matrix_mod = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = matrix_mod
spec.loader.exec_module(matrix_mod)


class BenchmarkMatrixTests(unittest.TestCase):
    def test_matrix_is_valid(self):
        self.assertEqual(matrix_mod.validate(matrix_mod.load_matrix()), [])

    def test_matrix_covers_languages_hle_multilingual_and_review(self):
        matrix = matrix_mod.load_matrix()
        campaigns = {c["id"]: c for c in matrix["campaigns"]}
        languages = {c["language"] for c in campaigns["coding_language_semantics"]["cells"]}
        self.assertTrue({"go", "python", "typescript_javascript", "rust", "cpp", "java_kotlin", "sql", "shell"} <= languages)
        self.assertTrue({"mathematics", "biology", "physics", "chemistry"} <= set(campaigns["hle_subject_reasoning"]["domains"]))
        self.assertIn("korean", campaigns["prompt_language_transfer"]["prompt_languages"])
        self.assertIn("false_positive_rate", campaigns["reviewer_defect_detection"]["required_metrics"])


if __name__ == "__main__":
    unittest.main()
