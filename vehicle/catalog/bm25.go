package catalog

import (
	"context"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kirin7002/app_personal_agent/contract"
)

const (
	bm25K1           = 1.2
	bm25B            = 0.25
	defaultRecallTop = 4
	multiRecallTop   = 6
	minRecallScore   = 0.8

	// 6-field BM25 weights (adapted from doc/old/BM25.md).
	weightTitle    = 2.8
	weightSlot     = 2.2
	weightTrigger  = 2.1
	weightName     = 1.2
	weightCategory = 1.1
	weightDesc     = 0.75

	// Domain re-ranking adjustments (validated by offline benchmark gate).
	exactTitleBoost    = 12.0
	exactAliasBoost    = 6.5
	antiExampleFactor  = 0.20
	ungatedToolFactor  = 0.12
	intentMatchBoost   = 1.24
	intentMismatchDrop = 0.78
)

var (
	// Automotive colloquial synonym normalizer applied identically at index & query time.
	automotiveSynonymReplacer = strings.NewReplacer(
		"抬头显示", "hud",
		"风速", "风量",
		"香薰", "香氛",
		"香气", "香氛",
		"起雾", "除雾 除霜",
		"去雾", "除雾 除霜",
		"车门玻璃", "车窗",
		"侧窗玻璃", "车窗",
		"车玻璃", "车窗",
		"电动尾门", "后备箱",
		"尾门", "后备箱",
		"尾箱", "后备箱",
		"行李箱", "后备箱",
		"后备厢", "后备箱",
		"前舱盖", "前备箱",
		"前备厢", "前备箱",
		"前盖", "前备箱",
		"机盖", "前备箱",
		"电滑门", "侧滑门",
		"电动门", "侧滑门",
		"拉门", "侧滑门",
		"倒车镜", "后视镜",
		"反光镜", "后视镜",
		"按喇叭", "鸣笛寻车",
		"双闪寻车", "鸣笛寻车",
		"闪灯鸣笛", "鸣笛寻车",
		"找车", "鸣笛寻车",
		"方向盘热", "方向盘加热",
		"暖方向盘", "方向盘加热",
		"屁股加热", "座椅加热",
		"座垫加热", "座椅加热",
		"拔枪", "充电枪解锁",
		"谷电", "预约充电",
		"续航", "充电 电量",
		"冷风", "空调 制冷",
		"暖风", "空调 制热",
	)

	// Window-specific position nouns preserve "车窗" before pure seat/side position stripping.
	windowPositionReplacer = strings.NewReplacer(
		"左前车窗", "车窗",
		"右前车窗", "车窗",
		"左后车窗", "车窗",
		"右后车窗", "车窗",
		"左前窗", "车窗",
		"右前窗", "车窗",
		"左后窗", "车窗",
		"右后窗", "车窗",
		"主驾窗", "车窗",
		"副驾窗", "车窗",
		"驾驶座窗", "车窗",
	)

	// Seat / side position modifiers stripped before BM25 scoring so position words
	// do not steal entity weight across devices (parsed separately by guard/params).
	positionStripReplacer = strings.NewReplacer(
		"主驾驶", " ",
		"副驾驶", " ",
		"驾驶座", " ",
		"主驾", " ",
		"副驾", " ",
		"左前", " ",
		"右前", " ",
		"左后", " ",
		"右后", " ",
		"前排", " ",
		"左侧", " ",
		"右侧", " ",
		"左边", " ",
		"右边", " ",
	)

	degreeHintRe = regexp.MustCompile(`(\d{1,2}(?:\.\d+)?)\s*(?:度|℃|°c)`)

	// Core vehicle action/entity unigrams preserved alongside 2/3-gram CJK slicing
	// to avoid short-command recall loss (e.g., "关窗", "锁车", "太冷").
	coreVehicleUnigrams = map[rune]bool{
		'开': true, '关': true, '升': true, '降': true,
		'冷': true, '热': true, '暖': true, '锁': true,
		'停': true, '窗': true, '门': true, '灯': true,
		'座': true, '风': true, '度': true, '充': true,
		'雾': true, '霜': true,
	}

	cjkStopUnigrams = map[rune]bool{
		'的': true, '了': true, '把': true, '一': true, '下': true, '吗': true,
		'呢': true, '啊': true, '吧': true, '呀': true, '我': true, '你': true,
		'他': true, '她': true, '它': true, '们': true, '帮': true, '给': true,
		'看': true, '那': true, '这': true, '个': true, '在': true, '是': true,
		'有': true, '和': true, '到': true, '要': true, '请': true, '能': true,
		'就': true, '都': true, '很': true, '也': true, '再': true, '点': true,
		'些': true, '哈': true, '嗯': true, '哦': true,
	}

	clauseSplitRe = regexp.MustCompile(`[。！？!?;；,，]+|同时|顺便|另外|再把|并且|还有|然后把|以及`)

	queryIntentMarkers = []string{
		"吗", "没", "状态", "多少", "几度", "查询", "查一下", "查查", "看看",
		"记录", "上限", "权限", "开了没", "关了没", "开着吗", "关了吗", "设了哪些", "设了多少", "是否",
	}

	controlIntentMarkers = []string{
		"打开", "关闭", "开启", "调到", "调高", "调低", "调大", "调小",
		"设成", "设置", "启动", "停止", "解锁", "闭锁", "锁车", "锁上",
		"升窗", "降窗", "摇上", "摇下", "复位", "放倒", "预约", "定时",
		"导航", "鸣笛", "寻车", "关一下", "开一下", "关上", "拉上", "拉开",
	}
)

type precomputedEntry struct {
	spec         OperationSpec
	weights      map[string]float64
	gatedWeights map[string]float64
	normTitle    string
	normNames    []string
	normAliases  []string
	antiExamples []string
	gateKeywords []string
}

type precomputedBM25 struct {
	entries []precomputedEntry
	byOp    map[contract.Operation]int
}

func normalizeAutomotiveText(text string, stripPositions bool) string {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return ""
	}
	if degreeHintRe.MatchString(s) {
		s = s + " 温度"
	}
	s = automotiveSynonymReplacer.Replace(s)
	if stripPositions {
		s = windowPositionReplacer.Replace(s)
		s = positionStripReplacer.Replace(s)
	}
	return strings.TrimSpace(s)
}

// stripAntiExampleClauses removes negative/counter-example clauses from descriptions
// so counter-examples do not accidentally attract BM25 hits.
func stripAntiExampleClauses(desc string) string {
	for _, marker := range []string{"不负责", "非本工具", "不是本工具", "请用", "默认由本地"} {
		if idx := strings.Index(desc, marker); idx >= 0 {
			desc = desc[:idx]
		}
	}
	return strings.TrimSpace(desc)
}

// tokenize emits lowercased ASCII words (splitting snake_case identifiers too),
// core vehicle CJK unigrams, and CJK 2-gram + 3-gram sliding windows.
func tokenize(text string) []string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return nil
	}
	var tokens []string
	var asciiBuf strings.Builder
	var cjkRunes []rune

	flushASCII := func() {
		if asciiBuf.Len() == 0 {
			return
		}
		word := asciiBuf.String()
		asciiBuf.Reset()
		tokens = append(tokens, word)
		if strings.ContainsRune(word, '_') {
			for _, part := range strings.Split(word, "_") {
				if part != "" && part != word {
					tokens = append(tokens, part)
				}
			}
		}
	}

	flushCJK := func() {
		n := len(cjkRunes)
		for i, r := range cjkRunes {
			if coreVehicleUnigrams[r] {
				tokens = append(tokens, string(r))
			}
			if i+1 < n {
				r2 := cjkRunes[i+1]
				if !cjkStopUnigrams[r] || !cjkStopUnigrams[r2] {
					tokens = append(tokens, string([]rune{r, r2}))
				}
			}
			if i+2 < n {
				r2, r3 := cjkRunes[i+1], cjkRunes[i+2]
				if !cjkStopUnigrams[r] || !cjkStopUnigrams[r2] || !cjkStopUnigrams[r3] {
					tokens = append(tokens, string([]rune{r, r2, r3}))
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
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
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

func countTermFreq(tokens []string) map[string]int {
	tf := make(map[string]int, len(tokens))
	for _, t := range tokens {
		tf[t]++
	}
	return tf
}

func collectFieldTokens(spec OperationSpec) (titleToks, slotToks, triggerToks, nameToks, catToks, descToks, gatedToks []string) {
	titleToks = tokenize(normalizeAutomotiveText(spec.Title, true))

	hasGate := len(spec.GateKeywords) > 0
	var slotBuf strings.Builder
	var gatedBuf strings.Builder
	for _, f := range spec.Fields {
		if f.isSensitive() {
			continue
		}
		slotBuf.WriteString(f.Name)
		slotBuf.WriteByte(' ')
		for _, e := range f.Enum {
			if hasGate {
				gatedBuf.WriteString(e)
				gatedBuf.WriteByte(' ')
			} else {
				slotBuf.WriteString(e)
				slotBuf.WriteByte(' ')
			}
		}
		for _, labels := range f.EnumLabels {
			for _, lbl := range labels {
				if hasGate {
					gatedBuf.WriteString(lbl)
					gatedBuf.WriteByte(' ')
				} else {
					slotBuf.WriteString(lbl)
					slotBuf.WriteByte(' ')
				}
			}
		}
	}
	for _, gt := range spec.GatedTerms {
		gatedBuf.WriteString(gt)
		gatedBuf.WriteByte(' ')
	}
	slotToks = tokenize(normalizeAutomotiveText(slotBuf.String(), true))
	gatedToks = tokenize(normalizeAutomotiveText(gatedBuf.String(), true))

	var trigBuf strings.Builder
	for _, a := range spec.Aliases {
		trigBuf.WriteString(a)
		trigBuf.WriteByte(' ')
	}
	for _, u := range spec.Utterances {
		trigBuf.WriteString(u)
		trigBuf.WriteByte(' ')
	}
	triggerToks = tokenize(normalizeAutomotiveText(trigBuf.String(), true))

	var nameBuf strings.Builder
	nameBuf.WriteString(spec.Operation.Device)
	nameBuf.WriteByte(' ')
	nameBuf.WriteString(spec.Operation.Tool)
	if spec.DownstreamTool != "" && spec.DownstreamTool != spec.Operation.Tool {
		nameBuf.WriteByte(' ')
		nameBuf.WriteString(spec.DownstreamTool)
	}
	nameToks = tokenize(nameBuf.String())

	catToks = tokenize(normalizeAutomotiveText(spec.Category, true))
	descToks = tokenize(normalizeAutomotiveText(stripAntiExampleClauses(spec.Description), true))
	return
}

// buildPrecomputedBM25 computes sparse 6-field weighted BM25 tables at startup/load time
// so runtime recall is O(query_terms * enabled_specs) table lookup (< 1 ms).
func buildPrecomputedBM25(specs []OperationSpec) *precomputedBM25 {
	n := len(specs)
	idx := &precomputedBM25{
		entries: make([]precomputedEntry, n),
		byOp:    make(map[contract.Operation]int, n),
	}
	if n == 0 {
		return idx
	}

	type fieldCorpus struct {
		weight float64
		tfs    []map[string]int
		lens   []int
		df     map[string]int
		avgLen float64
	}
	fields := []fieldCorpus{
		{weight: weightTitle, tfs: make([]map[string]int, n), lens: make([]int, n), df: make(map[string]int)},
		{weight: weightSlot, tfs: make([]map[string]int, n), lens: make([]int, n), df: make(map[string]int)},
		{weight: weightTrigger, tfs: make([]map[string]int, n), lens: make([]int, n), df: make(map[string]int)},
		{weight: weightName, tfs: make([]map[string]int, n), lens: make([]int, n), df: make(map[string]int)},
		{weight: weightCategory, tfs: make([]map[string]int, n), lens: make([]int, n), df: make(map[string]int)},
		{weight: weightDesc, tfs: make([]map[string]int, n), lens: make([]int, n), df: make(map[string]int)},
	}
	gatedTFs := make([]map[string]int, n)
	gatedLens := make([]int, n)
	gatedDF := make(map[string]int)
	totalGatedLen := 0

	for i, sp := range specs {
		idx.byOp[sp.Operation] = i
		t0, t1, t2, t3, t4, t5, tg := collectFieldTokens(sp)
		allToks := [][]string{t0, t1, t2, t3, t4, t5}
		for fIdx, toks := range allToks {
			tf := countTermFreq(toks)
			fields[fIdx].tfs[i] = tf
			fields[fIdx].lens[i] = len(toks)
			for term := range tf {
				fields[fIdx].df[term]++
			}
		}
		gtf := countTermFreq(tg)
		gatedTFs[i] = gtf
		gatedLens[i] = len(tg)
		totalGatedLen += len(tg)
		for term := range gtf {
			gatedDF[term]++
		}
	}

	for fIdx := range fields {
		total := 0
		for _, l := range fields[fIdx].lens {
			total += l
		}
		if total > 0 {
			fields[fIdx].avgLen = float64(total) / float64(n)
		} else {
			fields[fIdx].avgLen = 1.0
		}
	}
	avgGatedLen := 1.0
	if totalGatedLen > 0 {
		avgGatedLen = float64(totalGatedLen) / float64(n)
	}

	nf := float64(n)
	for i, sp := range specs {
		weights := make(map[string]float64)
		for fIdx := range fields {
			fc := &fields[fIdx]
			dl := float64(fc.lens[i])
			if dl == 0 {
				continue
			}
			for term, count := range fc.tfs[i] {
				tf := float64(count)
				df := float64(fc.df[term])
				idf := math.Log(1.0 + (nf-df+0.5)/(df+0.5))
				denom := tf + bm25K1*(1.0-bm25B+bm25B*(dl/fc.avgLen))
				score := fc.weight * idf * (tf * (bm25K1 + 1.0)) / denom
				weights[term] += score
			}
		}

		gated := make(map[string]float64)
		if gatedLens[i] > 0 {
			dl := float64(gatedLens[i])
			for term, count := range gatedTFs[i] {
				tf := float64(count)
				df := float64(gatedDF[term])
				idf := math.Log(1.0 + (nf-df+0.5)/(df+0.5))
				denom := tf + bm25K1*(1.0-bm25B+bm25B*(dl/avgGatedLen))
				gated[term] = weightSlot * idf * (tf * (bm25K1 + 1.0)) / denom
			}
		}

		normNames := []string{strings.ToLower(sp.Operation.Tool)}
		if sp.DownstreamTool != "" && strings.ToLower(sp.DownstreamTool) != normNames[0] {
			normNames = append(normNames, strings.ToLower(sp.DownstreamTool))
		}
		normAliases := make([]string, 0, len(sp.Aliases))
		for _, a := range sp.Aliases {
			na := normalizeAutomotiveText(a, true)
			if utf8.RuneCountInString(na) >= 2 {
				normAliases = append(normAliases, na)
			}
		}
		anti := make([]string, 0, len(sp.AntiExamples))
		for _, ae := range sp.AntiExamples {
			if na := strings.ToLower(strings.TrimSpace(ae)); na != "" {
				anti = append(anti, na)
			}
		}
		gates := make([]string, 0, len(sp.GateKeywords))
		for _, gk := range sp.GateKeywords {
			if ng := strings.ToLower(strings.TrimSpace(gk)); ng != "" {
				gates = append(gates, ng)
			}
		}

		idx.entries[i] = precomputedEntry{
			spec:         sp,
			weights:      weights,
			gatedWeights: gated,
			normTitle:    normalizeAutomotiveText(sp.Title, true),
			normNames:    normNames,
			normAliases:  normAliases,
			antiExamples: anti,
			gateKeywords: gates,
		}
	}
	return idx
}

func splitRecallClauses(text string) []string {
	parts := clauseSplitRe.Split(strings.TrimSpace(text), -1)
	clauses := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			clauses = append(clauses, trimmed)
		}
	}
	if len(clauses) == 0 && strings.TrimSpace(text) != "" {
		return []string{strings.TrimSpace(text)}
	}
	return clauses
}

func detectClauseIntent(clause string) string {
	lower := strings.ToLower(strings.TrimSpace(clause))
	isQuery := false
	for _, m := range queryIntentMarkers {
		if strings.Contains(lower, m) {
			isQuery = true
			break
		}
	}
	isControl := false
	for _, m := range controlIntentMarkers {
		if strings.Contains(lower, m) {
			isControl = true
			break
		}
	}
	if isQuery && !isControl {
		return "query"
	}
	if isControl && !isQuery {
		return "control"
	}
	if isQuery && (strings.HasSuffix(lower, "吗") || strings.HasSuffix(lower, "没") || strings.Contains(lower, "多少") || strings.Contains(lower, "几度") || strings.Contains(lower, "记录") || strings.Contains(lower, "上限") || strings.Contains(lower, "权限")) {
		return "query"
	}
	if isControl {
		return "control"
	}
	return "neutral"
}

func (idx *precomputedBM25) scoreClause(clause string, enabled map[contract.Operation]bool) map[contract.Operation]float64 {
	rawLower := strings.ToLower(strings.TrimSpace(clause))
	norm := normalizeAutomotiveText(clause, true)
	qTokens := tokenize(norm)
	if len(qTokens) == 0 {
		// Fallback for pure position-only queries like "左后".
		qTokens = tokenize(normalizeAutomotiveText(clause, false))
	}
	if len(qTokens) == 0 {
		return nil
	}

	uniqQ := make([]string, 0, len(qTokens))
	seen := make(map[string]bool, len(qTokens))
	for _, t := range qTokens {
		if !seen[t] {
			seen[t] = true
			uniqQ = append(uniqQ, t)
		}
	}

	intent := detectClauseIntent(clause)
	scores := make(map[contract.Operation]float64, len(idx.entries))

	for i := range idx.entries {
		entry := &idx.entries[i]
		if !enabled[entry.spec.Operation] {
			continue
		}

		// Check large-enum gate keywords.
		gateOpen := true
		if len(entry.gateKeywords) > 0 {
			gateOpen = false
			for _, gk := range entry.gateKeywords {
				if strings.Contains(rawLower, gk) || strings.Contains(norm, gk) {
					gateOpen = true
					break
				}
			}
		}

		score := 0.0
		for _, qt := range uniqQ {
			if w, ok := entry.weights[qt]; ok {
				score += w
			}
			if gateOpen {
				if gw, ok := entry.gatedWeights[qt]; ok {
					score += gw
				}
			}
		}

		// 1. Exact title / name / alias match boosts.
		if gateOpen {
			if entry.normTitle != "" && utf8.RuneCountInString(entry.normTitle) >= 2 && strings.Contains(norm, entry.normTitle) {
				score += exactTitleBoost
			}
			for _, nm := range entry.normNames {
				if nm != "" && strings.Contains(rawLower, nm) {
					score += exactTitleBoost
					break
				}
			}
			for _, al := range entry.normAliases {
				if strings.Contains(norm, al) {
					score += exactAliasBoost
					break
				}
			}
		}

		if score <= 0 {
			continue
		}

		// 2. Large-enum ungated penalty.
		if !gateOpen {
			score *= ungatedToolFactor
		}

		// 3. Anti-example suppression.
		for _, ae := range entry.antiExamples {
			if strings.Contains(rawLower, ae) || strings.Contains(norm, ae) {
				score *= antiExampleFactor
				break
			}
		}

		// 4. Control vs Query intent alignment.
		if intent == "query" {
			if entry.spec.MCPTool == contract.GetVehicleStatus {
				score *= intentMatchBoost
			} else {
				score *= intentMismatchDrop
			}
		} else if intent == "control" {
			if entry.spec.MCPTool == contract.DispatchVehicleCommand {
				score *= intentMatchBoost
			} else {
				score *= intentMismatchDrop
			}
		}

		if score >= minRecallScore {
			scores[entry.spec.Operation] = score
		}
	}
	return scores
}

type scoredOp struct {
	op    contract.Operation
	score float64
}

// FindCapabilities implements contract.CapabilityFinder using precomputed 6-field
// weighted BM25 + vehicle re-ranking + recall-only multi-intent clause union.
// Recall results are filtered strictly to the target vehicle's enabled capabilities
// and never restrict subsequent search_vehicle_capabilities calls.
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
	cache := s.bm25Cache
	s.mu.RUnlock()

	if strings.TrimSpace(text) == "" || len(specs) == 0 {
		return []contract.Capability{}, nil
	}
	if cache == nil {
		cache = buildPrecomputedBM25(specs)
	}

	enabled := make(map[contract.Operation]bool, len(specs))
	specByOp := make(map[contract.Operation]OperationSpec, len(specs))
	for _, sp := range specs {
		enabled[sp.Operation] = true
		specByOp[sp.Operation] = sp
	}

	clauses := splitRecallClauses(text)
	merged := make(map[contract.Operation]float64, len(specs))
	// Guarantee each clause's top candidate is preserved in multi-intent utterances.
	clauseTops := make([]scoredOp, 0, len(clauses))
	for _, clause := range clauses {
		cScores := cache.scoreClause(clause, enabled)
		var bestOp contract.Operation
		bestScore := 0.0
		for op, sc := range cScores {
			if sc > merged[op] {
				merged[op] = sc
			}
			if sc > bestScore {
				bestScore = sc
				bestOp = op
			}
		}
		if bestScore > 0 {
			clauseTops = append(clauseTops, scoredOp{op: bestOp, score: bestScore})
		}
	}

	if len(merged) == 0 {
		return []contract.Capability{}, nil
	}

	hits := make([]scoredOp, 0, len(merged))
	for op, sc := range merged {
		hits = append(hits, scoredOp{op: op, score: sc})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score == hits[j].score {
			if hits[i].op.Device != hits[j].op.Device {
				return hits[i].op.Device < hits[j].op.Device
			}
			return hits[i].op.Tool < hits[j].op.Tool
		}
		return hits[i].score > hits[j].score
	})

	limit := defaultRecallTop
	if len(clauses) > 1 {
		limit = multiRecallTop
	}
	if len(hits) < limit {
		limit = len(hits)
	}

	chosen := make([]contract.Operation, 0, limit)
	inChosen := make(map[contract.Operation]bool, limit)
	for _, h := range hits[:limit] {
		chosen = append(chosen, h.op)
		inChosen[h.op] = true
	}
	// If a multi-intent clause's #1 match fell outside top-K, include it cleanly.
	if len(clauses) > 1 {
		for _, ct := range clauseTops {
			if !inChosen[ct.op] {
				chosen = append(chosen, ct.op)
				inChosen[ct.op] = true
			}
		}
	}

	out := make([]contract.Capability, 0, len(chosen))
	for _, op := range chosen {
		sp := specByOp[op]
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

// Generic single-field BM25 index retained for ad-hoc manual text search (manual.go).
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

type scoredDoc struct {
	id    int
	score float64
}

func buildBM25Index(texts []string) bm25Index {
	docs := make([]bm25Doc, 0, len(texts))
	df := make(map[string]int)
	totalLen := 0
	for i, text := range texts {
		toks := tokenize(normalizeAutomotiveText(text, false))
		tf := countTermFreq(toks)
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

func (idx bm25Index) search(query string) []scoredDoc {
	qTokens := tokenize(normalizeAutomotiveText(query, false))
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
