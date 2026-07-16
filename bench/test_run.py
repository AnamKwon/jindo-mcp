import importlib.util
import sys
import unittest
from unittest import mock
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))
spec = importlib.util.spec_from_file_location("bench_run", HERE / "run.py")
bench = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = bench
spec.loader.exec_module(bench)


class HarnessTests(unittest.TestCase):
    def test_commands_disable_external_customization(self):
        cwd = Path("/tmp/work")
        codex = bench.candidate_command({"agent":"codex","model":"m"}, "p", cwd).argv
        self.assertIn("--ignore-user-config", codex)
        self.assertIn("--ephemeral", codex)
        self.assertNotIn("-a", codex)
        claude = bench.candidate_command({"agent":"claude","model":"m","effort":"high"}, "p", cwd).argv
        self.assertIn("--safe-mode", claude)
        self.assertIn("--disable-slash-commands", claude)
        self.assertIn('{"mcpServers":{}}', claude)
        agy = bench.candidate_command({"agent":"agy","model":"Gemini X"}, "p", cwd).argv
        self.assertIn("--sandbox", agy)
        self.assertIn(str(cwd), agy)

    def test_review_tiebreak_favors_fewer_critical_findings(self):
        rows = [
            {"task":"t","model":"a","attempts":1,"passes":1,"pass_rate":1,"review_score":9.8,"critical_findings":2,"review_count":2,"critical_per_review":1,"median_secs":1},
            {"task":"t","model":"b","attempts":1,"passes":1,"pass_rate":1,"review_score":9.0,"critical_findings":0,"review_count":3,"critical_per_review":0,"median_secs":2},
        ]
        old = bench.TASKS
        try:
            bench.TASKS = [{"id":"t","difficulty":"hard"}]
            self.assertEqual(bench.proposal(rows)["tiers"]["hard"]["winner"], "b")
        finally:
            bench.TASKS = old

    def test_review_json_parser(self):
        got = bench.parse_review('noise {"correctness":9,"invariants":8,"maintainability":7,"test_quality":6,"critical_findings":0,"rationale":"ok"}')
        self.assertEqual(got["score"], 7.5)

    def test_verify_timeout_is_a_recorded_failure(self):
        task = {"hidden_files": {}, "verify": [["go", "test", "./..."]]}
        with mock.patch.object(bench, "run", side_effect=bench.subprocess.TimeoutExpired(["go"], 1)):
            passed, checks = bench.verify(Path("/tmp"), task)
        self.assertFalse(passed)
        self.assertTrue(checks[0]["timeout"])


if __name__ == "__main__":
    unittest.main()
