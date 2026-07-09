package memory

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// insightsKey is the reserved control key holding the cross-agent insight layer:
// a list of distilled, provenance-tagged learnings that agents accumulate about
// a project (e.g. "build command is `make build`", "auth lives in
// internal/authz"). Like notesKey and digestKey it is NOT a real task entry —
// All(), Read(), and Stats() exclude it — but unlike the flat task log it is a
// curated, deduplicated, relevance-rankable knowledge tier that later agents
// (of any model) read as a bounded brief instead of scanning the whole store.
//
// Its stored form is a bare JSON list (like _notes), each element an insight
// object; see Insight.toMap / insightFromMap for the on-disk shape.
const insightsKey = "_insights"

// Salience tuning. These are package vars (not consts) so an orchestrator or a
// test can adjust the reinforcement dynamics without touching call sites.
var (
	// baseSalience is the importance a freshly recorded insight starts at.
	baseSalience = 0.5
	// reinforceBump is added to salience each time the SAME insight is
	// re-derived by any agent (capped at 1.0). Independent rediscovery is a
	// strong correctness signal, so reinforcement raises how readily the
	// insight is retrieved for future tasks.
	reinforceBump = 0.15
	// maxSalience caps salience so a heavily reinforced insight cannot dominate
	// retrieval unboundedly.
	maxSalience = 1.0
)

// Insight is one distilled cross-agent learning. Provenance (Agent/Model)
// records WHO first contributed it; Hits counts how many times it has been
// re-derived (reinforced); Salience is its current importance weight. Tags are
// optional retrieval keywords in addition to the words in Text.
type Insight struct {
	ID        string   `json:"id"`
	Text      string   `json:"text"`
	Agent     string   `json:"agent"`
	Model     string   `json:"model"`
	Salience  float64  `json:"salience"`
	Hits      int      `json:"hits"`
	Tags      []string `json:"tags,omitempty"`
	CreatedTS float64  `json:"created_ts"`
	UpdatedTS float64  `json:"updated_ts"`
}

// toMap renders an Insight as the generic map that load()/save() round-trips
// through encoding/json (which decodes JSON objects into map[string]any). Keys
// mirror the struct's json tags so a store written here reads back identically.
func (in Insight) toMap() map[string]any {
	m := map[string]any{
		"id":         in.ID,
		"text":       in.Text,
		"agent":      in.Agent,
		"model":      in.Model,
		"salience":   in.Salience,
		"hits":       float64(in.Hits),
		"created_ts": in.CreatedTS,
		"updated_ts": in.UpdatedTS,
	}
	if len(in.Tags) > 0 {
		tags := make([]any, len(in.Tags))
		for i, t := range in.Tags {
			tags[i] = t
		}
		m["tags"] = tags
	}
	return m
}

// insightFromMap parses one stored insight element back into an Insight. A
// malformed element (not a map, missing text) yields ok=false so the caller can
// skip it, mirroring load()'s tolerance of a corrupt store.
func insightFromMap(v any) (Insight, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return Insight{}, false
	}
	text, _ := m["text"].(string)
	if strings.TrimSpace(text) == "" {
		return Insight{}, false
	}
	in := Insight{Text: text}
	in.ID, _ = m["id"].(string)
	in.Agent, _ = m["agent"].(string)
	in.Model, _ = m["model"].(string)
	in.Salience, _ = m["salience"].(float64)
	if h, ok := m["hits"].(float64); ok {
		in.Hits = int(h)
	}
	in.CreatedTS, _ = m["created_ts"].(float64)
	in.UpdatedTS, _ = m["updated_ts"].(float64)
	if raw, ok := m["tags"].([]any); ok {
		for _, t := range raw {
			if s, ok := t.(string); ok {
				in.Tags = append(in.Tags, s)
			}
		}
	}
	return in, true
}

// loadInsights extracts the parsed insight list from an already-loaded store
// map, skipping malformed elements. Order is preserved.
func loadInsights(data map[string]any) []Insight {
	raw, ok := data[insightsKey].([]any)
	if !ok {
		return nil
	}
	out := make([]Insight, 0, len(raw))
	for _, el := range raw {
		if in, ok := insightFromMap(el); ok {
			out = append(out, in)
		}
	}
	return out
}

// storeInsights writes the insight list back into the store map as a bare list
// (like _notes), or deletes the key when the list is empty so an emptied layer
// leaves no stray control key.
func storeInsights(data map[string]any, insights []Insight) {
	if len(insights) == 0 {
		delete(data, insightsKey)
		return
	}
	list := make([]any, len(insights))
	for i, in := range insights {
		list[i] = in.toMap()
	}
	data[insightsKey] = list
}

// normalizeText produces the dedup key for an insight: lowercased, punctuation
// stripped, whitespace collapsed. Two insights whose text differs only in case,
// punctuation, or spacing are treated as the same learning and reinforced
// rather than duplicated.
func normalizeText(s string) string {
	return strings.Join(tokenize(s), " ")
}

// tokenRe splits on any run of non-alphanumeric characters.
var tokenRe = regexp.MustCompile(`[^a-z0-9]+`)

// stopwords are common words dropped from both dedup normalization and
// retrieval scoring so overlap reflects meaningful terms, not filler.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "was": true,
	"with": true, "this": true, "that": true, "from": true, "into": true,
	"use": true, "used": true, "using": true, "add": true, "added": true,
	"should": true, "must": true, "will": true, "can": true, "any": true,
	"all": true, "not": true, "but": true, "its": true, "has": true,
}

// tokenize lowercases s, splits into alphanumeric tokens, and drops stopwords
// and tokens shorter than 3 chars. Deterministic; the shared basis for dedup
// normalization and relevance scoring.
func tokenize(s string) []string {
	parts := tokenRe.Split(strings.ToLower(s), -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) < 3 || stopwords[p] {
			continue
		}
		out = append(out, p)
	}
	return out
}

// KeywordsOf returns up to max distinct significant tokens of s (lowercased,
// stopwords and sub-3-char words dropped), in first-seen order. Exported so an
// orchestrator can derive retrieval tags for an insight from the originating
// task without reimplementing the tokenizer. max<=0 returns all distinct tokens.
func KeywordsOf(s string, max int) []string {
	seen := make(map[string]bool)
	var out []string
	for _, t := range tokenize(s) {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out
}

// tokenSet returns the deduplicated token set of s, for overlap scoring.
func tokenSet(s string) map[string]bool {
	set := make(map[string]bool)
	for _, t := range tokenize(s) {
		set[t] = true
	}
	return set
}

// AddInsight records text as a cross-agent insight contributed by agent/model.
// Under the lock it dedups against the existing layer by normalized text: an
// exact normalized match REINFORCES the prior insight (Hits++, salience bumped,
// UpdatedTS advanced, and any new tags merged) and returns added=false; a novel
// learning is appended with baseSalience and returns added=true. An empty/blank
// text is a no-op (added=false, nil error).
func (m *SharedMemory) AddInsight(text, agent, model string, tags []string) (bool, error) {
	if strings.TrimSpace(text) == "" {
		return false, nil
	}
	if err := m.acquire(); err != nil {
		return false, err
	}
	defer m.release()

	data, err := m.load()
	if err != nil {
		return false, err
	}
	insights := loadInsights(data)

	norm := normalizeText(text)
	now := float64(time.Now().Unix())
	for i := range insights {
		if normalizeText(insights[i].Text) == norm {
			// Reinforce: independent rediscovery raises salience/recency.
			insights[i].Hits++
			insights[i].Salience += reinforceBump
			if insights[i].Salience > maxSalience {
				insights[i].Salience = maxSalience
			}
			insights[i].UpdatedTS = now
			insights[i].Tags = mergeTags(insights[i].Tags, tags)
			storeInsights(data, insights)
			return false, m.save(data)
		}
	}

	insights = append(insights, Insight{
		ID:        nextInsightID(insights),
		Text:      strings.TrimSpace(text),
		Agent:     agent,
		Model:     model,
		Salience:  baseSalience,
		Hits:      1,
		Tags:      dedupTags(tags),
		CreatedTS: now,
		UpdatedTS: now,
	})
	storeInsights(data, insights)
	return true, m.save(data)
}

// Insights returns the full insight layer in stored (insertion) order. Read-only.
func (m *SharedMemory) Insights() ([]Insight, error) {
	if err := m.acquire(); err != nil {
		return nil, err
	}
	defer m.release()

	data, err := m.load()
	if err != nil {
		return nil, err
	}
	return loadInsights(data), nil
}

// RetrieveInsights returns the top-k insights most RELEVANT to task, scored by
// term overlap weighted by salience and reinforcement. It is READ-ONLY (no
// mutation, so it is safe to call on the hot dispatch path and stays
// deterministic for tests). Insights with zero term overlap are excluded, so a
// task unrelated to any learning injects nothing rather than noise. A k<=0 or
// empty layer returns nil.
//
// This is the curated-injection primitive: the orchestrator renders the result
// as a short brief for the next sub-agent instead of dumping the whole store —
// the "bounded shared memory" the headless-agent contract promises.
func (m *SharedMemory) RetrieveInsights(task string, k int) ([]Insight, error) {
	if k <= 0 {
		return nil, nil
	}
	if err := m.acquire(); err != nil {
		return nil, err
	}
	defer m.release()

	data, err := m.load()
	if err != nil {
		return nil, err
	}
	insights := loadInsights(data)
	if len(insights) == 0 {
		return nil, nil
	}

	taskTokens := tokenSet(task)
	if len(taskTokens) == 0 {
		return nil, nil
	}

	type scored struct {
		in    Insight
		score float64
	}
	ranked := make([]scored, 0, len(insights))
	for _, in := range insights {
		overlap := 0
		insTokens := tokenSet(in.Text + " " + strings.Join(in.Tags, " "))
		for t := range insTokens {
			if taskTokens[t] {
				overlap++
			}
		}
		if overlap == 0 {
			continue
		}
		// Relevance drives the score; salience and reinforcement break ties and
		// nudge broadly-relevant, well-established insights ahead of one-off ones.
		score := float64(overlap)*(1.0+in.Salience) + 0.1*float64(in.Hits)
		ranked = append(ranked, scored{in: in, score: score})
	}
	if len(ranked) == 0 {
		return nil, nil
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		if ranked[i].in.Salience != ranked[j].in.Salience {
			return ranked[i].in.Salience > ranked[j].in.Salience
		}
		if ranked[i].in.UpdatedTS != ranked[j].in.UpdatedTS {
			return ranked[i].in.UpdatedTS > ranked[j].in.UpdatedTS
		}
		return ranked[i].in.ID < ranked[j].in.ID
	})
	if len(ranked) > k {
		ranked = ranked[:k]
	}
	out := make([]Insight, len(ranked))
	for i, s := range ranked {
		out[i] = s.in
	}
	return out, nil
}

// nextInsightID returns a fresh "i<n>" id one past the highest existing numeric
// id, so ids stay stable and collision-free across reinforcement rewrites.
func nextInsightID(insights []Insight) string {
	maxN := 0
	for _, in := range insights {
		if strings.HasPrefix(in.ID, "i") {
			if n, err := strconv.Atoi(in.ID[1:]); err == nil && n > maxN {
				maxN = n
			}
		}
	}
	return "i" + strconv.Itoa(maxN+1)
}

// mergeTags appends any new tags not already present (case-insensitive), keeping
// existing order. dedupTags collapses duplicates within a fresh tag list.
func mergeTags(existing, add []string) []string {
	seen := make(map[string]bool, len(existing))
	for _, t := range existing {
		seen[strings.ToLower(t)] = true
	}
	for _, t := range add {
		lt := strings.ToLower(strings.TrimSpace(t))
		if lt == "" || seen[lt] {
			continue
		}
		seen[lt] = true
		existing = append(existing, t)
	}
	return existing
}

func dedupTags(tags []string) []string {
	return mergeTags(nil, tags)
}
