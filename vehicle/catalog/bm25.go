package catalog

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/kirin7002/app_personal_agent/contract"
)

const (
	bm25K1           = 1.5
	bm25B            = 0.75
	defaultRecallTop = 4
)

type bm25Doc struct {
	id     int
	tokens []string
	tf     map[string]int
}

type bm25Index struct {
	docs   []bm25Doc
	df     map[string]int
	avgLen float64
}

func buildBM25Index(texts []string) bm25Index {
	docs := make([]bm25Doc, 0, len(texts))
	df := make(map[string]int)
	totalLen := 0
	for i, text := range texts {
		toks := tokenize(text)
		tf := make(map[string]int, len(toks))
		for _, tok := range toks {
			tf[tok]++
		}
		for tok := range tf {
			df[tok]++
		}
		totalLen += len(toks)
		docs = append(docs, bm25Doc{id: i, tokens: toks, tf: tf})
	}
	avgLen := 1.0
	if len(docs) > 0 && totalLen > 0 {
		avgLen = float64(totalLen) / float64(len(docs))
	}
	return bm25Index{docs: docs, df: df, avgLen: avgLen}
}

type scoredDoc struct {
	id    int
	score float64
}

func (idx bm25Index) search(query string) []scoredDoc {
	qTokens := tokenize(query)
	if len(qTokens) == 0 || len(idx.docs) == 0 {
		return nil
	}
	seenQ := make(map[string]bool, len(qTokens))
	uniqQ := make([]string, 0, len(qTokens))
	for _, t := range qTokens {
		if !seenQ[t] {
			seenQ[t] = true
			uniqQ = append(uniqQ, t)
		}
	}

	n := float64(len(idx.docs))
	results := make([]scoredDoc, 0, len(idx.docs))
	for _, doc := range idx.docs {
		if len(doc.tokens) == 0 {
			continue
		}
		score := 0.0
		dl := float64(len(doc.tokens))
		for _, qt := range uniqQ {
			tf := float64(doc.tf[qt])
			if tf == 0 {
				continue
			}
			df := float64(idx.df[qt])
			idf := math.Log(1.0 + (n-df+0.5)/(df+0.5))
			denom := tf + bm25K1*(1.0-bm25B+bm25B*(dl/idx.avgLen))
			score += idf * (tf * (bm25K1 + 1.0)) / denom
		}
		if score > 0 {
			results = append(results, scoredDoc{id: doc.id, score: score})
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].score == results[j].score {
			return results[i].id < results[j].id
		}
		return results[i].score > results[j].score
	})
	return results
}

var cjkStopUnigrams = map[rune]bool{
	'的': true, '了': true, '把': true, '一': true, '下': true, '吗': true,
	'呢': true, '啊': true, '吧': true, '呀': true, '我': true, '你': true,
	'他': true, '她': true, '它': true, '们': true, '帮': true, '给': true,
	'看': true, '那': true, '这': true, '个': true, '在': true, '是': true,
	'有': true, '和': true, '到': true, '要': true, '请': true, '能': true,
	'就': true, '都': true, '很': true, '也': true, '再': true,
}

// tokenize emits lowercased ASCII alphanumeric words plus CJK unigrams (excluding
// generic Chinese stop characters) and bigrams so vehicle utterances match cleanly.
func tokenize(text string) []string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return nil
	}
	var tokens []string
	var asciiBuf strings.Builder
	var cjkRunes []rune

	flushASCII := func() {
		if asciiBuf.Len() > 0 {
			tokens = append(tokens, asciiBuf.String())
			asciiBuf.Reset()
		}
	}
	flushCJK := func() {
		for i, r := range cjkRunes {
			if !cjkStopUnigrams[r] {
				tokens = append(tokens, string(r))
			}
			if i+1 < len(cjkRunes) {
				r2 := cjkRunes[i+1]
				if !cjkStopUnigrams[r] || !cjkStopUnigrams[r2] {
					tokens = append(tokens, string([]rune{r, r2}))
				}
			}
		}
		cjkRunes = cjkRunes[:0]
	}

	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			flushASCII()
			cjkRunes = append(cjkRunes, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			asciiBuf.WriteRune(r)
		default:
			flushASCII()
			flushCJK()
		}
	}
	flushASCII()
	flushCJK()
	return tokens
}

func specCorpusText(spec OperationSpec) string {
	var b strings.Builder
	b.WriteString(spec.Operation.Device)
	b.WriteByte(' ')
	b.WriteString(spec.Operation.Tool)
	b.WriteByte(' ')
	b.WriteString(spec.Description)
	for _, a := range spec.Aliases {
		b.WriteByte(' ')
		b.WriteString(a)
	}
	for _, u := range spec.Utterances {
		b.WriteByte(' ')
		b.WriteString(u)
	}
	for _, f := range spec.Fields {
		b.WriteByte(' ')
		b.WriteString(f.Name)
	}
	return b.String()
}

// FindCapabilities implements contract.CapabilityFinder for first-turn BM25 pre-screening.
// Recall results are filtered strictly to the target vehicle's enabled capabilities and
// never restrict subsequent search_vehicle_capabilities calls.
func (s *Service) FindCapabilities(_ context.Context, scope contract.Scope, text string) ([]contract.Capability, error) {
	if !scope.Valid() {
		return nil, &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "invalid scope"}
	}
	s.mu.RLock()
	profile, ok := s.profiles[scope.VehicleID]
	if !ok {
		profile = DefaultProfile(scope.VehicleID)
	}
	specs := s.enabledSpecsLocked(profile)
	s.mu.RUnlock()

	if strings.TrimSpace(text) == "" || len(specs) == 0 {
		return []contract.Capability{}, nil
	}
	corpus := make([]string, len(specs))
	for i, sp := range specs {
		corpus[i] = specCorpusText(sp)
	}
	hits := buildBM25Index(corpus).search(text)
	if len(hits) == 0 {
		return []contract.Capability{}, nil
	}
	limit := min(len(hits), defaultRecallTop)
	out := make([]contract.Capability, 0, limit)
	for _, h := range hits[:limit] {
		sp := specs[h.id]
		out = append(out, contract.Capability{
			Operation:    sp.Operation,
			MCPTool:      sp.MCPTool,
			ParamsSchema: sp.ParamsSchemaFor(profile),
		})
	}
	return out, nil
}

// SearchCapabilities serves the search_vehicle_capabilities MCP tool across the full
// enabled capability map for the vehicle (including when first-turn recall returned empty).
func (s *Service) SearchCapabilities(ctx context.Context, scope contract.Scope, input contract.SearchCapabilitiesInput) (contract.SearchCapabilitiesResult, error) {
	if !scope.Valid() {
		return contract.SearchCapabilitiesResult{}, &contract.BusinessError{
			Code:    contract.TurnContextInvalid,
			Message: "invalid scope",
		}
	}
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return contract.SearchCapabilitiesResult{}, &contract.BusinessError{
			Code:    contract.InvalidArgument,
			Message: "query is required",
		}
	}
	caps, err := s.FindCapabilities(ctx, scope, query)
	if err != nil {
		return contract.SearchCapabilitiesResult{}, err
	}
	if len(caps) > 0 {
		return contract.SearchCapabilitiesResult{Capabilities: caps}, nil
	}
	// Broad fallback: if query mentions broad category words like "全部" or "all",
	// return all enabled vehicle capabilities so the agent can inspect available tools.
	lower := strings.ToLower(query)
	if strings.Contains(lower, "all") || strings.Contains(query, "全部") || strings.Contains(query, "能力") || strings.Contains(query, "功能") {
		s.mu.RLock()
		profile, ok := s.profiles[scope.VehicleID]
		if !ok {
			profile = DefaultProfile(scope.VehicleID)
		}
		specs := s.enabledSpecsLocked(profile)
		s.mu.RUnlock()
		all := make([]contract.Capability, 0, len(specs))
		for _, sp := range specs {
			all = append(all, contract.Capability{
				Operation:    sp.Operation,
				MCPTool:      sp.MCPTool,
				ParamsSchema: sp.ParamsSchemaFor(profile),
			})
		}
		return contract.SearchCapabilitiesResult{Capabilities: all}, nil
	}
	return contract.SearchCapabilitiesResult{Capabilities: []contract.Capability{}}, nil
}
