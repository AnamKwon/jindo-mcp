package routing

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed config/capability_policy.json
var capabilityPolicyJSON []byte

// CapabilityContext is the caller's explicit description of the work dimension
// that difficulty scoring alone cannot represent. The MCP boundary owns
// collecting these values; routing owns validating and resolving them.
type CapabilityContext struct {
	Domain         string                `json:"domain"`
	Language       string                `json:"language,omitempty"`
	PromptLanguage string                `json:"prompt_language,omitempty"`
	TaskType       string                `json:"task_type"`
	Risk           string                `json:"risk"`
	Oracle         string                `json:"oracle"`
	Signals        CapabilityTaskSignals `json:"signals,omitempty"`
}

// CapabilityTaskSignals are host observations about the concrete task, not
// inputs to a hidden score. They travel with the routing decision so the host
// can explain why analogous benchmark evidence does or does not transfer.
type CapabilityTaskSignals struct {
	Ambiguity         string   `json:"ambiguity,omitempty"`
	ChangeScope       string   `json:"change_scope,omitempty"`
	ContextSize       string   `json:"context_size,omitempty"`
	Reversibility     string   `json:"reversibility,omitempty"`
	RequiredStrengths []string `json:"required_strengths,omitempty"`
	FailureModes      []string `json:"failure_modes,omitempty"`
}

type CapabilityCandidate struct {
	Agent  string `json:"agent"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

type CapabilityModelEvidence struct {
	ObservedStrengths  []string `json:"observed_strengths"`
	Cautions           []string `json:"cautions"`
	OperationalProfile string   `json:"operational_profile"`
}

type CapabilityCandidateEvidence struct {
	Candidate CapabilityCandidate     `json:"candidate"`
	Evidence  CapabilityModelEvidence `json:"evidence"`
}

type CapabilityHostSelection struct {
	Owner                  string   `json:"owner"`
	CandidateOrder         string   `json:"candidate_order"`
	RequiredConsiderations []string `json:"required_considerations"`
	SingleModelWhen        []string `json:"single_model_when"`
	MultiModelWhen         []string `json:"multi_model_when"`
	SelectionRecord        []string `json:"selection_record"`
	EvidenceBoundaries     []string `json:"evidence_boundaries"`
	UnmeasuredWorkflow     []string `json:"unmeasured_workflow"`
}

// CapabilityEvidenceCell is a measured route that resembles the requested
// work. It is deliberately descriptive: SharedDimensions exposes why it was
// retrieved and TransferWarning prevents analogy from becoming an automatic
// winner transfer.
type CapabilityEvidenceCell struct {
	Domain           string                `json:"domain"`
	Language         string                `json:"language,omitempty"`
	PromptLanguage   string                `json:"prompt_language,omitempty"`
	TaskType         string                `json:"task_type"`
	SharedDimensions []string              `json:"shared_dimensions"`
	EvidenceStatus   string                `json:"evidence_status"`
	Reason           string                `json:"reason"`
	Candidates       []CapabilityCandidate `json:"candidates"`
	RequiredOracle   []string              `json:"required_oracle,omitempty"`
	TransferWarning  string                `json:"transfer_warning"`
}

type CapabilityReview struct {
	Status                      string                `json:"status"`
	MinimumIndependentReviewers int                   `json:"minimum_independent_reviewers"`
	Pool                        []CapabilityCandidate `json:"pool"`
	ExcludeAnswerAuthor         bool                  `json:"exclude_answer_author"`
	Acceptance                  string                `json:"acceptance"`
}

type CapabilityDecision struct {
	Context             CapabilityContext             `json:"context"`
	Mode                string                        `json:"mode"`
	ExactMatch          bool                          `json:"exact_match"`
	EvidenceStatus      string                        `json:"evidence_status"`
	EvidenceGap         string                        `json:"evidence_gap,omitempty"`
	CalibrationRequired bool                          `json:"calibration_required"`
	Reason              string                        `json:"reason"`
	Candidates          []CapabilityCandidate         `json:"candidates"`
	CandidateEvidence   []CapabilityCandidateEvidence `json:"candidate_evidence"`
	EligibleModels      []CapabilityCandidateEvidence `json:"eligible_models"`
	AnalogousEvidence   []CapabilityEvidenceCell      `json:"analogous_evidence,omitempty"`
	RequiredOracle      []string                      `json:"required_oracle,omitempty"`
	Review              CapabilityReview              `json:"review"`
	HostSelection       CapabilityHostSelection       `json:"host_selection"`
}

type capabilityRoute struct {
	Domain              string                `json:"domain"`
	Language            string                `json:"language"`
	TaskType            string                `json:"task_type"`
	PromptLanguage      string                `json:"prompt_language"`
	Mode                string                `json:"mode"`
	EvidenceStatus      string                `json:"evidence_status"`
	CalibrationRequired bool                  `json:"calibration_required"`
	Reason              string                `json:"reason"`
	Candidates          []CapabilityCandidate `json:"candidates"`
	RequiredOracle      []string              `json:"required_oracle"`
}

type capabilityPolicy struct {
	MeasuredRoutes []capabilityRoute                  `json:"measured_routes"`
	ModelCatalog   []CapabilityCandidate              `json:"model_catalog"`
	HostSelection  CapabilityHostSelection            `json:"host_selection"`
	ModelEvidence  map[string]CapabilityModelEvidence `json:"model_evidence"`
	ReviewPolicy   struct {
		Status string                `json:"status"`
		Pool   []CapabilityCandidate `json:"pool"`
	} `json:"review_policy"`
	CalibrationBacklog struct {
		NoncodingDomains []string `json:"noncoding_domains"`
	} `json:"calibration_backlog"`
}

var loadedCapabilityPolicy capabilityPolicy

func init() {
	if err := json.Unmarshal(capabilityPolicyJSON, &loadedCapabilityPolicy); err != nil {
		panic(fmt.Sprintf("routing: cannot parse embedded capability_policy.json: %v", err))
	}
}

func validateCapabilityContext(ctx CapabilityContext) error {
	if ctx.Domain == "" {
		return fmt.Errorf("capability domain is required")
	}
	if ctx.TaskType == "" {
		return fmt.Errorf("capability task_type is required")
	}
	if ctx.Domain == "coding" && ctx.Language == "" {
		return fmt.Errorf("capability language is required for domain coding")
	}
	switch ctx.Risk {
	case "low", "normal", "high":
	default:
		return fmt.Errorf("capability risk must be one of low, normal, high")
	}
	switch ctx.Oracle {
	case "deterministic", "exact_answer", "judge", "none":
	default:
		return fmt.Errorf("capability oracle must be one of deterministic, exact_answer, judge, none")
	}
	switch ctx.PromptLanguage {
	case "", "english", "korean", "japanese", "multilingual_mixed":
	default:
		return fmt.Errorf("capability prompt_language must be one of english, korean, japanese, multilingual_mixed")
	}
	if ctx.Domain != "coding" {
		known := false
		for _, domain := range loadedCapabilityPolicy.CalibrationBacklog.NoncodingDomains {
			if domain == ctx.Domain {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("unsupported capability domain %q", ctx.Domain)
		}
	}
	return nil
}

func availableCapabilityCandidates(in []CapabilityCandidate) []CapabilityCandidate {
	out := make([]CapabilityCandidate, 0, len(in))
	for _, candidate := range in {
		if agentUsable(candidate.Agent) {
			out = append(out, candidate)
		}
	}
	return out
}

func candidateEvidence(candidates []CapabilityCandidate) []CapabilityCandidateEvidence {
	out := make([]CapabilityCandidateEvidence, 0, len(candidates))
	for _, candidate := range candidates {
		evidence, ok := loadedCapabilityPolicy.ModelEvidence[candidate.Model]
		if !ok {
			evidence = CapabilityModelEvidence{
				Cautions:           []string{"no model-specific local benchmark summary is available"},
				OperationalProfile: "unknown",
			}
		}
		out = append(out, CapabilityCandidateEvidence{Candidate: candidate, Evidence: evidence})
	}
	return out
}

// CapabilityModelCatalog returns a copy of the models known to the capability
// evidence policy. Inventory scanners use it only to distinguish a newly seen
// model from an already assessed one; catalog order is not a routing rank.
func CapabilityModelCatalog() []CapabilityCandidate {
	return append([]CapabilityCandidate(nil), loadedCapabilityPolicy.ModelCatalog...)
}

func exactCapabilityRoute(ctx CapabilityContext) *capabilityRoute {
	for i := range loadedCapabilityPolicy.MeasuredRoutes {
		route := &loadedCapabilityPolicy.MeasuredRoutes[i]
		if route.Domain != ctx.Domain || route.Language != ctx.Language || route.TaskType != ctx.TaskType {
			continue
		}
		// Natural-language effects are measured dimensions. A generic/English
		// cell must not silently become Japanese or mixed-language evidence.
		promptMatches := route.PromptLanguage == ctx.PromptLanguage
		if route.PromptLanguage == "" && (ctx.PromptLanguage == "" || ctx.PromptLanguage == "english") {
			promptMatches = true
		}
		if promptMatches {
			return route
		}
	}
	return nil
}

func analogousCapabilityEvidence(ctx CapabilityContext) []CapabilityEvidenceCell {
	out := make([]CapabilityEvidenceCell, 0)
	for _, route := range loadedCapabilityPolicy.MeasuredRoutes {
		promptMatches := route.PromptLanguage == ctx.PromptLanguage ||
			(route.PromptLanguage == "" && (ctx.PromptLanguage == "" || ctx.PromptLanguage == "english"))
		if route.Domain != ctx.Domain || (route.Language == ctx.Language && route.TaskType == ctx.TaskType && promptMatches) {
			continue
		}
		shared := []string{"domain"}
		if route.Language != "" && route.Language == ctx.Language {
			shared = append(shared, "language")
		}
		if route.TaskType == ctx.TaskType {
			shared = append(shared, "task_type")
		}
		if route.PromptLanguage != "" && route.PromptLanguage == ctx.PromptLanguage {
			shared = append(shared, "prompt_language")
		}
		// Coding analogies need at least one dimension beyond the broad domain;
		// subject routes may transfer from another reasoning form in that subject.
		if ctx.Domain == "coding" && len(shared) == 1 {
			continue
		}
		reason := route.Reason
		if reason == "" {
			reason = "local direct-CLI calibration in the displayed capability cell"
		}
		status := route.EvidenceStatus
		if status == "" {
			status = "measured_single_repeat"
		}
		out = append(out, CapabilityEvidenceCell{
			Domain: route.Domain, Language: route.Language, PromptLanguage: route.PromptLanguage,
			TaskType: route.TaskType, SharedDimensions: shared, EvidenceStatus: status,
			Reason: reason, Candidates: availableCapabilityCandidates(route.Candidates),
			RequiredOracle:  route.RequiredOracle,
			TransferWarning: "analogous evidence is context for host judgment, not permission to transfer its winner",
		})
	}
	return out
}

// RouteCapability retrieves evidence for an explicit capability cell. It does
// not score the task or choose a model: exact candidates are a measured prior,
// while an unmeasured cell exposes the full available policy catalog plus
// analogous evidence for the host to interpret and verify against the real task.
func RouteCapability(ctx CapabilityContext) (CapabilityDecision, error) {
	if err := validateCapabilityContext(ctx); err != nil {
		return CapabilityDecision{}, err
	}

	eligibleCandidates := availableCapabilityCandidates(loadedCapabilityPolicy.ModelCatalog)
	if len(eligibleCandidates) == 0 {
		return CapabilityDecision{}, fmt.Errorf("no installed agent is available for capability route")
	}

	var candidates []CapabilityCandidate
	var requiredOracle []string
	evidenceStatus := ""
	evidenceGap := ""
	reason := ""
	calibrationRequired := true
	mode := "host_decides"

	matched := exactCapabilityRoute(ctx)
	if matched != nil {
		route := matched
		candidates = route.Candidates
		requiredOracle = route.RequiredOracle
		evidenceStatus = route.EvidenceStatus
		if evidenceStatus == "" {
			evidenceStatus = "measured_single_repeat"
		}
		calibrationRequired = route.CalibrationRequired
		if route.Mode != "" {
			mode = route.Mode
		}
		reason = route.Reason
		if reason == "" {
			reason = "exact domain-language-task cell from local direct-CLI calibration"
		}
	}

	if evidenceStatus == "" {
		if ctx.Domain == "coding" {
			evidenceStatus = "unmeasured_language_task_or_prompt_cell"
			evidenceGap = "no exact programming-language, task-type, and prompt-language benchmark cell"
		} else {
			evidenceStatus = "unmeasured_subject_reasoning_or_prompt_cell"
			evidenceGap = "no exact subject, reasoning-form, and prompt-language benchmark cell"
		}
		reason = "the host must choose from the available catalog using the concrete task signals, analogous evidence, and a task-local verification probe"
	}

	candidates = availableCapabilityCandidates(candidates)
	if matched != nil && len(candidates) == 0 {
		return CapabilityDecision{}, fmt.Errorf("no installed agent is available for capability route")
	}
	reviewPool := availableCapabilityCandidates(loadedCapabilityPolicy.ReviewPolicy.Pool)
	minimumReviewers := 1
	if ctx.Risk == "high" || ctx.Oracle == "none" || mode == "parallel_compare" {
		minimumReviewers = 2
	}
	acceptance := "objective_oracle_and_review_agree"
	if ctx.Oracle == "none" {
		acceptance = "two_reviews_agree_then_human_check"
	}

	return CapabilityDecision{
		Context: ctx, Mode: mode, ExactMatch: matched != nil, EvidenceStatus: evidenceStatus, EvidenceGap: evidenceGap,
		CalibrationRequired: calibrationRequired, Reason: reason,
		Candidates: candidates, CandidateEvidence: candidateEvidence(candidates),
		EligibleModels: candidateEvidence(eligibleCandidates), AnalogousEvidence: analogousCapabilityEvidence(ctx),
		RequiredOracle: requiredOracle,
		Review: CapabilityReview{
			Status:                      loadedCapabilityPolicy.ReviewPolicy.Status,
			MinimumIndependentReviewers: minimumReviewers,
			Pool:                        reviewPool, ExcludeAnswerAuthor: true, Acceptance: acceptance,
		},
		HostSelection: loadedCapabilityPolicy.HostSelection,
	}, nil
}
