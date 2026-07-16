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
	Domain         string `json:"domain"`
	Language       string `json:"language,omitempty"`
	PromptLanguage string `json:"prompt_language,omitempty"`
	TaskType       string `json:"task_type"`
	Risk           string `json:"risk"`
	Oracle         string `json:"oracle"`
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
	EvidenceStatus      string                        `json:"evidence_status"`
	CalibrationRequired bool                          `json:"calibration_required"`
	Reason              string                        `json:"reason"`
	Candidates          []CapabilityCandidate         `json:"candidates"`
	CandidateEvidence   []CapabilityCandidateEvidence `json:"candidate_evidence"`
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

type capabilityFallback struct {
	Candidates []CapabilityCandidate `json:"candidates"`
}

type capabilityPolicy struct {
	MeasuredRoutes []capabilityRoute                  `json:"measured_routes"`
	Fallbacks      map[string]capabilityFallback      `json:"fallbacks"`
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

// RouteCapability resolves an explicit capability cell. It never guesses that
// an unmeasured language or subject inherits a measured winner: such cells
// return parallel_compare with provider-diverse candidates.
func RouteCapability(ctx CapabilityContext) (CapabilityDecision, error) {
	if err := validateCapabilityContext(ctx); err != nil {
		return CapabilityDecision{}, err
	}

	var candidates []CapabilityCandidate
	var requiredOracle []string
	evidenceStatus := ""
	reason := ""
	calibrationRequired := false
	mode := "cascade"

	var matched *capabilityRoute
	for i := range loadedCapabilityPolicy.MeasuredRoutes {
		route := &loadedCapabilityPolicy.MeasuredRoutes[i]
		if route.Domain == ctx.Domain && route.Language == ctx.Language && route.TaskType == ctx.TaskType &&
			(route.PromptLanguage == "" || route.PromptLanguage == ctx.PromptLanguage) {
			if matched == nil || route.PromptLanguage != "" {
				matched = route
			}
		}
	}
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
		switch {
		case ctx.Domain == "coding" && ctx.TaskType == "mechanical":
			candidates = loadedCapabilityPolicy.Fallbacks["coding_mechanical"].Candidates
			evidenceStatus = "tier_prior_only"
			calibrationRequired = ctx.Language != "go"
			reason = "mechanical cascade is a tier prior, not language-specific evidence"
		case ctx.Domain == "coding":
			candidates = loadedCapabilityPolicy.Fallbacks["coding_unmeasured"].Candidates
			evidenceStatus = "unmeasured_language_or_task_type"
			calibrationRequired = true
			reason = "no exact language and task-type calibration cell; provider-diverse comparison required when risk is high or the oracle is absent"
			if ctx.Risk == "high" || ctx.Oracle == "none" {
				mode = "parallel_compare"
			}
		default:
			candidates = loadedCapabilityPolicy.Fallbacks["noncoding_unmeasured"].Candidates
			evidenceStatus = "unmeasured_domain"
			calibrationRequired = true
			mode = "parallel_compare"
			reason = "no local HLE-like subject evidence; candidates are provider-diverse and not a subject-strength ranking"
		}
	}
	if ctx.Oracle == "none" {
		mode = "parallel_compare"
	}

	candidates = availableCapabilityCandidates(candidates)
	if len(candidates) == 0 {
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
		Context: ctx, Mode: mode, EvidenceStatus: evidenceStatus,
		CalibrationRequired: calibrationRequired, Reason: reason,
		Candidates: candidates, CandidateEvidence: candidateEvidence(candidates), RequiredOracle: requiredOracle,
		Review: CapabilityReview{
			Status:                      loadedCapabilityPolicy.ReviewPolicy.Status,
			MinimumIndependentReviewers: minimumReviewers,
			Pool:                        reviewPool, ExcludeAnswerAuthor: true, Acceptance: acceptance,
		},
		HostSelection: loadedCapabilityPolicy.HostSelection,
	}, nil
}
