import argparse
import importlib.util
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import delegation_hard_tasks as hard_tasks
spec = importlib.util.spec_from_file_location("delegation", HERE / "delegation_run.py")
delegation = importlib.util.module_from_spec(spec)
spec.loader.exec_module(delegation)

# Synthetic candidates -- tests must not touch base_run.inventory()/agy_models(),
# which shell out to the real codex/claude/agy CLIs.
CODEX_CANDIDATE = {"key": "codex:gpt-5.6-luna", "agent": "codex", "model": "gpt-5.6-luna", "effort": "high"}
CLAUDE_CANDIDATE = {"key": "claude:claude-sonnet-5", "agent": "claude", "model": "claude-sonnet-5", "effort": "high"}
AGY_CANDIDATE = {"key": "agy:Gemini 3.5 Flash (Low)", "agent": "agy", "model": "Gemini 3.5 Flash (Low)", "effort": "low"}


class DelegationRunTests(unittest.TestCase):
    def test_build_conditions_default_and_explicit(self):
        self.assertEqual(delegation.build_conditions(None), list(delegation.CONDITIONS))
        self.assertEqual(
            delegation.build_conditions("large_direct,small_direct"),
            ["large_direct", "small_direct"],
        )
        with self.assertRaises(ValueError):
            delegation.build_conditions("large_direct,invalid_condition")

    def test_parse_token_usage(self):
        sample = "input tokens: 12, output tokens: 34, tokens used: 46"
        got = delegation.parse_token_usage(sample)
        self.assertEqual(got["input_tokens"], 12)
        self.assertEqual(got["output_tokens"], 34)
        self.assertEqual(got["total_tokens"], 46)
        self.assertEqual(got["coverage"], "exact")
        self.assertFalse(got["parse_error"])
        self.assertNotIn("raw", got)

        sample_total_only = "Run complete. 1,234 tokens used."
        got = delegation.parse_token_usage(sample_total_only)
        self.assertEqual(got["input_tokens"], None)
        self.assertEqual(got["output_tokens"], None)
        self.assertEqual(got["total_tokens"], 1234)
        self.assertEqual(got["coverage"], "total_only")

    def test_parse_token_usage_prefers_trailing_summary_over_earlier_progress_lines(self):
        # A CLI may print running/partial usage during the session; only the
        # last (trailing) occurrence is the authoritative total.
        sample = (
            "input tokens: 10, output tokens: 20\n"
            "...working...\n"
            "input tokens: 999, output tokens: 888\n"
            "Final summary -- input tokens: 100, output tokens: 200, tokens used: 300"
        )
        got = delegation.parse_token_usage(sample)
        self.assertEqual(got["input_tokens"], 100)
        self.assertEqual(got["output_tokens"], 200)
        self.assertEqual(got["total_tokens"], 300)
        self.assertEqual(got["coverage"], "exact")

    def test_parse_token_usage_prefers_latest_position_across_regex_formats(self):
        sample = "tokens used: 5\n...\nRun complete. 300 tokens used."
        got = delegation.parse_token_usage(sample)
        self.assertEqual(got["total_tokens"], 300)
        self.assertEqual(got["coverage"], "total_only")

    def test_parse_token_usage_no_match_reports_none_coverage(self):
        got = delegation.parse_token_usage("no usage information printed")
        self.assertEqual(got["coverage"], "none")
        self.assertFalse(got["parse_error"])

    def test_estimate_cost(self):
        price = {"input_per_million": 1.0, "output_per_million": 2.0}
        cost, coverage = delegation.estimate_cost(
            {"input_tokens": 10, "output_tokens": 20, "total_tokens": 30, "coverage": "exact"},
            price,
        )
        self.assertEqual(cost, 0.00005)
        self.assertEqual(coverage, "exact")

        cost, coverage = delegation.estimate_cost(
            {"input_tokens": None, "output_tokens": None, "total_tokens": 50, "coverage": "total_only"},
            {"input_per_million": 4.0, "output_per_million": 4.0},
        )
        self.assertEqual(cost, 0.0002)
        self.assertEqual(coverage, "uniform_price_with_total")

        cost, coverage = delegation.estimate_cost(
            {"input_tokens": None, "output_tokens": 8, "total_tokens": 50, "coverage": "partial"},
            {"input_per_million": 4.0, "output_per_million": 5.0},
        )
        self.assertIsNone(cost)
        self.assertEqual(coverage, "missing_input_or_output_breakdown")

    def test_planner_command_is_read_only_independent(self):
        cmd = delegation.planner_command(CODEX_CANDIDATE, "fix this", Path("/tmp/repo"))
        self.assertIn("read-only", cmd.argv)
        self.assertNotIn("workspace-write", cmd.argv)

        cmd = delegation.planner_command(CLAUDE_CANDIDATE, "fix this", Path("/tmp/repo"))
        self.assertIn("--permission-mode", cmd.argv)
        mode_index = cmd.argv.index("--permission-mode")
        self.assertEqual(cmd.argv[mode_index + 1], "plan")
        tools_index = cmd.argv.index("--tools")
        self.assertEqual(cmd.argv[tools_index + 1], "Bash,Read,Glob,Grep")

    def test_planner_command_agy_has_no_contradictory_edit_anchor(self):
        cmd = delegation.planner_command(AGY_CANDIDATE, "fix this", Path("/tmp/repo"))
        self.assertIn("--mode", cmd.argv)
        mode_index = cmd.argv.index("--mode")
        self.assertEqual(cmd.argv[mode_index + 1], "plan")

        prompt_index = cmd.argv.index("-p") + 1
        prompt_text = cmd.argv[prompt_index]
        self.assertNotIn("edit files there", prompt_text)
        self.assertIn("Do not edit", prompt_text)

    def test_planner_failure_reason(self):
        ok_stage = {
            "exit": 0,
            "timed_out": False,
            "output": "a concrete plan",
            "tokens": {"parse_error": False},
        }
        self.assertIsNone(delegation.planner_failure_reason(ok_stage))

        self.assertEqual(
            delegation.planner_failure_reason({**ok_stage, "exit": 1}),
            "planner_exit_nonzero",
        )
        self.assertEqual(
            delegation.planner_failure_reason({**ok_stage, "timed_out": True}),
            "planner_timeout",
        )
        self.assertEqual(
            delegation.planner_failure_reason({**ok_stage, "output": "   "}),
            "planner_empty_output",
        )
        self.assertEqual(
            delegation.planner_failure_reason({**ok_stage, "tokens": {"parse_error": True}}),
            "planner_token_parse_error",
        )

    def test_run_condition_marks_invalid_and_skips_implementation_when_planner_unavailable(self):
        task = {
            "id": "t1",
            "task_type": "unit",
            "difficulty": "easy",
            "language": "go",
            "prompt": "do the thing",
            "verify": [],
        }
        large = {"key": "codex:large", "agent": "codex", "model": "large-model", "effort": "high"}
        small = {"key": "codex:small", "agent": "codex", "model": "small-model", "effort": "high"}
        args = argparse.Namespace(skip_review=True)

        failing_planner_result = {
            "model": large["key"],
            "exit": 1,
            "timed_out": False,
            "secs": 0.42,
            "output": "",
            "error": "boom",
            "tokens": {"input_tokens": None, "output_tokens": None, "total_tokens": None,
                       "coverage": "none", "parse_error": False},
            "cost_usd": None,
            "cost_coverage": "missing_usage_or_price",
        }

        with tempfile.TemporaryDirectory() as tmp:
            with patch.object(delegation.base_run, "setup_repo", return_value=Path(tmp)), \
                 patch.object(delegation, "run_stage", return_value=failing_planner_result) as mock_run_stage, \
                 patch.object(delegation.base_run, "verify") as mock_verify, \
                 patch.object(delegation.base_run, "review_solution") as mock_review:
                row = delegation.run_condition(
                    task, "large_plan_small", large, small,
                    run_id="run-1", repeat=0, reviewers=[], all_candidates={},
                    args=args, price_table=delegation.DEFAULT_PRICE_TABLE,
                )

        self.assertTrue(row["invalid"])
        self.assertEqual(row["invalid_reason"], "planner_exit_nonzero")
        self.assertIsNone(row["implementation"])
        self.assertFalse(row["passed"])
        mock_run_stage.assert_called_once()  # only the planner stage ran
        mock_verify.assert_not_called()
        mock_review.assert_not_called()

    def test_run_condition_proceeds_when_planner_succeeds(self):
        task = {
            "id": "t1",
            "task_type": "unit",
            "difficulty": "easy",
            "language": "go",
            "prompt": "do the thing",
            "verify": [],
        }
        large = {"key": "codex:large", "agent": "codex", "model": "large-model", "effort": "high"}
        small = {"key": "codex:small", "agent": "codex", "model": "small-model", "effort": "high"}
        args = argparse.Namespace(skip_review=True)

        planner_result = {
            "model": large["key"], "exit": 0, "timed_out": False, "secs": 1.0,
            "output": "a concrete plan", "error": None,
            "tokens": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2,
                       "coverage": "exact", "parse_error": False},
            "cost_usd": None, "cost_coverage": "missing_usage_or_price",
        }
        impl_result = {
            "model": small["key"], "exit": 0, "timed_out": False, "secs": 1.0,
            "output": "done", "error": None,
            "tokens": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2,
                       "coverage": "exact", "parse_error": False},
            "cost_usd": None, "cost_coverage": "missing_usage_or_price",
        }

        with tempfile.TemporaryDirectory() as tmp:
            with patch.object(delegation.base_run, "setup_repo", return_value=Path(tmp)), \
                 patch.object(delegation, "run_stage", side_effect=[planner_result, impl_result]), \
                 patch.object(delegation.base_run, "verify", return_value=(True, [])):
                row = delegation.run_condition(
                    task, "large_plan_small", large, small,
                    run_id="run-2", repeat=0, reviewers=[], all_candidates={},
                    args=args, price_table=delegation.DEFAULT_PRICE_TABLE,
                )

        self.assertFalse(row["invalid"])
        self.assertIsNone(row["invalid_reason"])
        self.assertIsNotNone(row["implementation"])
        self.assertTrue(row["passed"])

    def test_summarize_excludes_invalid_rows_from_quality_and_cost(self):
        rows = [
            {
                "condition": "large_plan_small", "passed": True, "invalid": False,
                "invalid_reason": None, "total_cost_usd": 1.0, "reviews": [],
                "implementation": {"secs": 1.0}, "planner": {"secs": 1.0}, "secs": 2.0,
            },
            {
                "condition": "large_plan_small", "passed": False, "invalid": True,
                "invalid_reason": "planner_timeout", "total_cost_usd": None, "reviews": [],
                "implementation": None, "planner": {"secs": 0.5}, "secs": 0.5,
            },
        ]
        summary = delegation.summarize(rows)
        cond = summary["conditions"][0]
        self.assertEqual(cond["attempts"], 1)
        self.assertEqual(cond["passes"], 1)
        self.assertEqual(cond["pass_rate"], 1.0)
        self.assertEqual(cond["invalid_count"], 1)
        self.assertEqual(cond["invalid_reasons"], ["planner_timeout"])
        self.assertEqual(cond["median_cost_usd"], 1.0)

    def test_aggregate_task_rows_excludes_invalid_from_pass_rate(self):
        rows = [
            {"task": "t1", "condition": "large_plan_small", "passed": True, "invalid": False},
            {"task": "t1", "condition": "large_plan_small", "passed": False, "invalid": True},
        ]
        task_rows = delegation.aggregate_task_rows(rows)
        self.assertEqual(len(task_rows), 1)
        self.assertEqual(task_rows[0]["attempts"], 1)
        self.assertEqual(task_rows[0]["passes"], 1)
        self.assertEqual(task_rows[0]["pass_rate"], 1.0)
        self.assertEqual(task_rows[0]["invalid_count"], 1)

    def test_delegation_tasks_include_hard_fixtures(self):
        ids = [task["id"] for task in delegation.TASKS]
        self.assertEqual(len(ids), len(delegation.base_run.TASKS) + len(hard_tasks.TASKS))
        self.assertIn("atomic_config_migration_v2", ids)
        self.assertIn("javascript_secure_archive_plan_v3", ids)

    def test_selected_includes_hard_fixture_task_ids(self):
        selected_task = delegation.selected(delegation.TASKS, "atomic_config_migration_v2,javascript_secure_archive_plan_v3", "id")
        self.assertEqual([task["id"] for task in selected_task], ["atomic_config_migration_v2", "javascript_secure_archive_plan_v3"] )


if __name__ == "__main__":
    unittest.main()
