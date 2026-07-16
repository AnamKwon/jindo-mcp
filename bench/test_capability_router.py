import importlib.util
import sys
import unittest
from pathlib import Path

HERE = Path(__file__).parent
spec = importlib.util.spec_from_file_location("capability_router", HERE / "capability_router.py")
router = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = router
spec.loader.exec_module(router)


class CapabilityRouterTests(unittest.TestCase):
    def test_measured_go_concurrency_starts_with_observed_small_model(self):
        got = router.route(domain="coding", language="go", task_type="concurrency_fencing",
                           risk="high", oracle="deterministic")
        self.assertEqual(got["evidence_status"], "measured_single_repeat")
        self.assertEqual(got["candidates"][0]["model"], "Gemini 3.5 Flash (Low)")
        self.assertEqual(got["mode"], "cascade")
        self.assertTrue(got["calibration_required"])
        self.assertEqual(got["host_selection"]["owner"], "host")
        self.assertTrue(got["exact_match"])
        self.assertEqual(got["host_selection"]["candidate_order"], "direct_evidence_prior_or_catalog_order_never_execution_order")
        self.assertEqual(len(got["candidate_evidence"]), len(got["candidates"]))
        self.assertGreater(len(got["eligible_models"]), len(got["candidates"]))
        self.assertTrue(got["candidate_evidence"][0]["evidence"]["observed_strengths"])
        self.assertTrue(got["candidate_evidence"][0]["evidence"]["cautions"])

    def test_repeated_python_cell_uses_its_measured_small_model(self):
        got = router.route(domain="coding", language="python", task_type="async_singleflight_cache",
                           risk="high", oracle="deterministic")
        self.assertEqual(got["evidence_status"], "measured_repeated_fixture")
        self.assertEqual(got["candidates"][0]["model"], "Gemini 3.5 Flash (Low)")
        self.assertEqual(got["review"]["pool"][0]["model"], "gpt-5.6-terra")

    def test_korean_sql_uses_paired_prompt_winner(self):
        got = router.route(domain="coding", language="sql", task_type="bitemporal_ledger_report",
                           prompt_language="korean", risk="high", oracle="deterministic")
        self.assertEqual(got["evidence_status"], "measured_repeated_paired_prompt")
        self.assertEqual(got["candidates"][0]["model"], "gpt-5.3-codex-spark")

    def test_general_coding_cells_use_task_specific_reviewed_winners(self):
        for language, task_type, winner in (
            ("go", "api_debugging_retry_semantics", "gpt-5.6-terra"),
            ("python", "numerical_exact_apportionment", "Gemini 3.5 Flash (Low)"),
            ("java", "multifile_transaction_refactor", "Gemini 3.5 Flash (Low)"),
            ("javascript", "security_archive_path_validation", "gpt-5.6-terra"),
        ):
            with self.subTest(language=language, task_type=task_type):
                got = router.route(domain="coding", language=language, task_type=task_type,
                                   risk="high", oracle="deterministic")
                self.assertEqual(got["evidence_status"], "measured_repeated_reviewed_fixture")
                self.assertEqual(got["candidates"][0]["model"], winner)
                self.assertTrue(got["calibration_required"])

    def test_unmeasured_programming_language_is_not_claimed_as_measured(self):
        got = router.route(domain="coding", language="rust", task_type="concurrency_fencing",
                           risk="high", oracle="deterministic",
                           signals={"ambiguity": "high", "required_strengths": ["atomic ordering"]})
        self.assertTrue(got["calibration_required"])
        self.assertFalse(got["exact_match"])
        self.assertEqual(got["evidence_status"], "unmeasured_language_task_or_prompt_cell")
        self.assertEqual(got["mode"], "host_decides")
        self.assertEqual(got["candidates"], [])
        self.assertGreaterEqual(len(got["eligible_models"]), 10)
        self.assertEqual(got["signals"]["ambiguity"], "high")
        self.assertTrue(any(cell["language"] == "rust" for cell in got["analogous_evidence"]))

    def test_unmeasured_prompt_exposes_analogies_without_transferring_winner(self):
        got = router.route(domain="coding", language="sql", task_type="bitemporal_ledger_report",
                           prompt_language="japanese", risk="high", oracle="deterministic")
        self.assertFalse(got["exact_match"])
        self.assertEqual(got["mode"], "host_decides")
        self.assertEqual(got["candidates"], [])
        self.assertGreaterEqual(len(got["analogous_evidence"]), 2)
        self.assertTrue(all(cell["transfer_warning"] for cell in got["analogous_evidence"]))

    def test_expansion_cells_preserve_stability_boundary(self):
        swift = router.route(domain="coding", language="swift", task_type="actor_isolation_atomic_batch",
                             risk="high", oracle="deterministic")
        self.assertEqual(swift["mode"], "cascade")
        self.assertEqual(swift["candidates"][0]["model"], "gpt-5.6-terra")
        for language, task_type in (
            ("c", "memory_safe_incremental_parser"),
            ("shell", "quoting_atomic_file_reconciliation"),
        ):
            with self.subTest(language=language):
                got = router.route(domain="coding", language=language, task_type=task_type,
                                   risk="high", oracle="deterministic")
                self.assertEqual(got["mode"], "parallel_compare")
                self.assertTrue(got["calibration_required"])
                self.assertIn("unstable", got["evidence_status"])

    def test_test_generation_cells_require_parallel_verification(self):
        for language, task_type, first_model in (
            ("python", "contract_mutation_test_generation", "Gemini 3.5 Flash (High)"),
            ("go", "concurrency_fencing_test_generation", "claude-opus-4-8"),
        ):
            with self.subTest(language=language):
                got = router.route(domain="coding", language=language, task_type=task_type,
                                   risk="high", oracle="deterministic")
                self.assertEqual(got["mode"], "parallel_compare")
                self.assertTrue(got["calibration_required"])
                self.assertEqual(got["candidates"][0]["model"], first_model)
                self.assertIn("measured", got["evidence_status"])

    def test_hle_biology_is_provider_diverse_and_requires_calibration(self):
        got = router.route(domain="biology", language=None, task_type="multiple_choice",
                           risk="high", oracle="exact_answer")
        self.assertEqual(got["evidence_status"], "measured_20_items_single_repeat")
        self.assertEqual(got["mode"], "parallel_compare")
        self.assertEqual({c["agent"] for c in got["candidates"]}, {"codex", "claude", "agy"})
        self.assertGreaterEqual(got["review"]["minimum_independent_reviewers"], 2)

    def test_missing_oracle_leaves_mode_to_host_but_requires_stronger_review(self):
        got = router.route(domain="coding", language="python", task_type="debugging",
                           risk="normal", oracle="none")
        self.assertEqual(got["mode"], "host_decides")
        self.assertEqual(got["review"]["acceptance"], "two_reviews_agree_then_human_check")
        self.assertGreaterEqual(got["review"]["minimum_independent_reviewers"], 2)


if __name__ == "__main__":
    unittest.main()
