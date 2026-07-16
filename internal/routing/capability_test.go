package routing

import "testing"

func TestCapabilityPolicyHasEvidenceForEveryCandidateModel(t *testing.T) {
	seen := map[string]bool{}
	for _, route := range loadedCapabilityPolicy.MeasuredRoutes {
		for _, candidate := range route.Candidates {
			seen[candidate.Model] = true
		}
	}
	for _, fallback := range loadedCapabilityPolicy.Fallbacks {
		for _, candidate := range fallback.Candidates {
			seen[candidate.Model] = true
		}
	}
	for model := range seen {
		evidence, ok := loadedCapabilityPolicy.ModelEvidence[model]
		if !ok || len(evidence.ObservedStrengths) == 0 || len(evidence.Cautions) == 0 || evidence.OperationalProfile == "" {
			t.Fatalf("model evidence for %q = %+v", model, evidence)
		}
	}
}

func TestRouteCapabilityMeasuredGoConcurrencyUsesObservedSmallModel(t *testing.T) {
	got, err := RouteCapability(CapabilityContext{
		Domain: "coding", Language: "go", TaskType: "concurrency_fencing",
		Risk: "high", Oracle: "deterministic",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.EvidenceStatus != "measured_single_repeat" || !got.CalibrationRequired {
		t.Fatalf("evidence = %q calibration=%v", got.EvidenceStatus, got.CalibrationRequired)
	}
	if got.Mode != "cascade" || len(got.Candidates) == 0 || got.Candidates[0].Model != "Gemini 3.5 Flash (Low)" {
		t.Fatalf("route = %+v", got)
	}
	if got.HostSelection.Owner != "host" || got.HostSelection.CandidateOrder != "benchmark_prior_not_execution_order" || len(got.CandidateEvidence) != len(got.Candidates) {
		t.Fatalf("decision support = selection:%+v evidence:%+v", got.HostSelection, got.CandidateEvidence)
	}
	if len(got.CandidateEvidence[0].Evidence.ObservedStrengths) == 0 || len(got.CandidateEvidence[0].Evidence.Cautions) == 0 {
		t.Fatalf("candidate evidence = %+v", got.CandidateEvidence[0])
	}
}

func TestRouteCapabilityMeasuredRustStoreUsesRepeatedSmallModel(t *testing.T) {
	got, err := RouteCapability(CapabilityContext{
		Domain: "coding", Language: "rust", TaskType: "optimistic_atomic_store",
		Risk: "high", Oracle: "deterministic",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "cascade" || !got.CalibrationRequired || got.EvidenceStatus != "measured_repeated_fixture" {
		t.Fatalf("route = %+v", got)
	}
	if len(got.Candidates) == 0 || got.Candidates[0].Model != "Gemini 3.5 Flash (Low)" {
		t.Fatalf("candidates = %+v", got.Candidates)
	}
}

func TestRouteCapabilityMeasuredPythonCacheUsesRepeatedSmallModel(t *testing.T) {
	got, err := RouteCapability(CapabilityContext{
		Domain: "coding", Language: "python", TaskType: "async_singleflight_cache",
		Risk: "high", Oracle: "deterministic",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.EvidenceStatus != "measured_repeated_fixture" || len(got.Candidates) == 0 || got.Candidates[0].Model != "Gemini 3.5 Flash (Low)" {
		t.Fatalf("route = %+v", got)
	}
	if len(got.Review.Pool) == 0 || got.Review.Pool[0].Model != "gpt-5.6-terra" {
		t.Fatalf("review pool = %+v", got.Review.Pool)
	}
}

func TestRouteCapabilityDiverseCodeCellsKeepTaskSpecificWinners(t *testing.T) {
	for _, tc := range []struct {
		language, taskType, model string
	}{
		{"javascript", "bounded_keyed_scheduler", "gpt-5.6-terra"},
		{"java", "deterministic_dependency_planner", "gpt-5.3-codex-spark"},
		{"sql", "bitemporal_ledger_report", "Gemini 3.5 Flash (Low)"},
		{"cpp", "raii_reentrant_observer_registry", "claude-opus-4-8"},
	} {
		got, err := RouteCapability(CapabilityContext{
			Domain: "coding", Language: tc.language, TaskType: tc.taskType,
			Risk: "high", Oracle: "deterministic",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.EvidenceStatus != "measured_repeated_fixture" || len(got.Candidates) == 0 || got.Candidates[0].Model != tc.model {
			t.Fatalf("%s/%s route = %+v", tc.language, tc.taskType, got)
		}
	}
}

func TestRouteCapabilityGeneralCodingCellsUseReviewedWinners(t *testing.T) {
	for _, tc := range []struct {
		language, taskType, model string
	}{
		{"go", "api_debugging_retry_semantics", "gpt-5.6-terra"},
		{"python", "numerical_exact_apportionment", "Gemini 3.5 Flash (Low)"},
		{"java", "multifile_transaction_refactor", "Gemini 3.5 Flash (Low)"},
		{"javascript", "security_archive_path_validation", "gpt-5.6-terra"},
	} {
		got, err := RouteCapability(CapabilityContext{
			Domain: "coding", Language: tc.language, TaskType: tc.taskType,
			Risk: "high", Oracle: "deterministic",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.EvidenceStatus != "measured_repeated_reviewed_fixture" || !got.CalibrationRequired || len(got.Candidates) == 0 || got.Candidates[0].Model != tc.model {
			t.Fatalf("%s/%s route = %+v", tc.language, tc.taskType, got)
		}
	}
}

func TestRouteCapabilityExpansionCellsPreserveStabilityBoundary(t *testing.T) {
	swift, err := RouteCapability(CapabilityContext{
		Domain: "coding", Language: "swift", TaskType: "actor_isolation_atomic_batch",
		Risk: "high", Oracle: "deterministic",
	})
	if err != nil {
		t.Fatal(err)
	}
	if swift.Mode != "cascade" || len(swift.Candidates) == 0 || swift.Candidates[0].Model != "gpt-5.6-terra" {
		t.Fatalf("swift route = %+v", swift)
	}
	for _, tc := range []struct{ language, taskType string }{
		{"c", "memory_safe_incremental_parser"},
		{"shell", "quoting_atomic_file_reconciliation"},
	} {
		got, err := RouteCapability(CapabilityContext{
			Domain: "coding", Language: tc.language, TaskType: tc.taskType,
			Risk: "high", Oracle: "deterministic",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != "parallel_compare" || !got.CalibrationRequired {
			t.Fatalf("%s route = %+v", tc.language, got)
		}
	}
}

func TestRouteCapabilityTestGenerationCellsRequireParallelVerification(t *testing.T) {
	for _, tc := range []struct {
		language, taskType, model string
	}{
		{"python", "contract_mutation_test_generation", "Gemini 3.5 Flash (High)"},
		{"go", "concurrency_fencing_test_generation", "claude-opus-4-8"},
	} {
		got, err := RouteCapability(CapabilityContext{
			Domain: "coding", Language: tc.language, TaskType: tc.taskType,
			Risk: "high", Oracle: "deterministic",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != "parallel_compare" || !got.CalibrationRequired || len(got.Candidates) == 0 || got.Candidates[0].Model != tc.model {
			t.Fatalf("%s/%s route = %+v", tc.language, tc.taskType, got)
		}
	}
}

func TestRouteCapabilityKoreanSQLOverridesGenericPromptRoute(t *testing.T) {
	english, err := RouteCapability(CapabilityContext{
		Domain: "coding", Language: "sql", PromptLanguage: "english",
		TaskType: "bitemporal_ledger_report", Risk: "high", Oracle: "deterministic",
	})
	if err != nil {
		t.Fatal(err)
	}
	korean, err := RouteCapability(CapabilityContext{
		Domain: "coding", Language: "sql", PromptLanguage: "korean",
		TaskType: "bitemporal_ledger_report", Risk: "high", Oracle: "deterministic",
	})
	if err != nil {
		t.Fatal(err)
	}
	if english.Candidates[0].Model != "Gemini 3.5 Flash (Low)" || korean.Candidates[0].Model != "gpt-5.3-codex-spark" {
		t.Fatalf("english=%+v korean=%+v", english.Candidates, korean.Candidates)
	}
	if korean.EvidenceStatus != "measured_repeated_paired_prompt" {
		t.Fatalf("korean evidence = %q", korean.EvidenceStatus)
	}
}

func TestRouteCapabilityUnmeasuredRustShapeStillRequiresParallelComparison(t *testing.T) {
	got, err := RouteCapability(CapabilityContext{
		Domain: "coding", Language: "rust", TaskType: "concurrency_fencing",
		Risk: "high", Oracle: "deterministic",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "parallel_compare" || got.EvidenceStatus != "unmeasured_language_or_task_type" {
		t.Fatalf("route = %+v", got)
	}
}

func TestRouteCapabilityBiologyIsNeverSilentlySingleModel(t *testing.T) {
	got, err := RouteCapability(CapabilityContext{
		Domain: "biology", TaskType: "multiple_choice", Risk: "high", Oracle: "exact_answer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "parallel_compare" || got.EvidenceStatus != "measured_20_items_single_repeat" || !got.CalibrationRequired {
		t.Fatalf("route = %+v", got)
	}
	if len(got.Candidates) == 0 || got.Candidates[0].Model != "Gemini 3.5 Flash (Medium)" {
		t.Fatalf("candidates = %+v", got.Candidates)
	}
	if got.Review.Status != "measured_20_cases_3_repeats" || got.Review.MinimumIndependentReviewers != 2 {
		t.Fatalf("review = %+v", got.Review)
	}
}

func TestRouteCapabilityRejectsIncompleteContext(t *testing.T) {
	for _, tc := range []CapabilityContext{
		{TaskType: "short_answer", Risk: "high", Oracle: "exact_answer"},
		{Domain: "coding", TaskType: "debugging", Risk: "normal", Oracle: "deterministic"},
		{Domain: "biology", Risk: "high", Oracle: "exact_answer"},
		{Domain: "biology", TaskType: "short_answer", Risk: "danger", Oracle: "exact_answer"},
	} {
		if _, err := RouteCapability(tc); err == nil {
			t.Fatalf("RouteCapability(%+v) returned nil error", tc)
		}
	}
}

func TestRouteCapabilityFiltersUnavailableAgents(t *testing.T) {
	saved := AgentAvailable
	AgentAvailable = func(name string) bool { return name != "agy" }
	t.Cleanup(func() { AgentAvailable = saved })

	got, err := RouteCapability(CapabilityContext{
		Domain: "coding", Language: "go", TaskType: "concurrency_fencing",
		Risk: "high", Oracle: "deterministic",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Candidates) == 0 || got.Candidates[0].Agent != "codex" {
		t.Fatalf("candidates = %+v", got.Candidates)
	}
}
