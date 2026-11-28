package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/catalog"
	"github.com/kirin7002/app_personal_agent/vehicle/tasks"
)

var (
	guardClauseSplitRe = regexp.MustCompile(`[。！？!?;；,，]+`)

	readOnlyQueryMarkers = []string{
		"关了吗", "关没关", "关好没", "关好了吗", "开着吗", "开没开", "打开了吗",
		"正在充电吗", "在充电吗", "设了多少", "设了哪些", "开了没", "权限开了没",
		"状态", "多少度了", "几度了", "现在几度", "多少度", "怎么用", "在哪里设置",
		"在哪设置", "哪儿设置", "怎么设置", "说明书", "手册", "还有什么没做",
		"哪些任务", "待办", "查一下", "查查", "预约记录",
	}

	globalCancelMarkers = []string{
		"算了", "取消", "不用了", "先不管", "先不用", "别动", "不要动",
	}

	clauseNegationMarkers = []string{
		"别关", "不要关", "不用关", "别开", "不要开", "不用开", "别调", "不要调",
		"不用调", "别锁", "不要锁", "别解锁", "不要解锁", "别启动", "不要启动",
		"别加热", "不要加热", "别通风", "不要通风", "别打开", "不要打开", "别关闭", "不要关闭",
	}

	genericControlVerbs = []string{
		"打开", "关闭", "开启", "关上", "开一下", "关一下", "调到", "调高", "调低",
		"调大", "调小", "升高", "降低", "设成", "设置", "启动", "停止", "开始", "结束",
		"解锁", "闭锁", "锁车", "锁上", "升窗", "降窗", "摇上", "摇下", "复位", "放倒",
		"预约", "定时", "导航", "鸣笛", "寻车", "拉上", "拉开",
	}

	relativeUpMarkers = []string{
		"调高", "升高", "提高", "加大", "调大", "增加", "加", "高一点", "大一点", "热一点",
	}

	relativeDownMarkers = []string{
		"调低", "降低", "减小", "调小", "减少", "减", "低一点", "小一点", "冷一点", "凉一点",
	}

	cnSmallNumReplacer = strings.NewReplacer(
		"一", "1", "两", "2", "二", "2", "三", "3", "四", "4", "五", "5",
		"六", "6", "七", "7", "八", "8", "九", "9", "十", "10",
	)

	bareNumberRe = regexp.MustCompile(`(?:\+|-)?(\d+(?:\.\d+)?)`)
)

func isReadOnlyQuery(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, m := range readOnlyQueryMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	if strings.HasSuffix(lower, "吗") || strings.HasSuffix(lower, "吗？") || strings.HasSuffix(lower, "吗?") {
		if strings.Contains(lower, "开着") || strings.Contains(lower, "关着") || strings.Contains(lower, "了吗") || strings.Contains(lower, "几度") || strings.Contains(lower, "充电") {
			return true
		}
	}
	return false
}

func splitGuardClauses(text string) []string {
	raw := guardClauseSplitRe.Split(strings.TrimSpace(text), -1)
	out := make([]string, 0, len(raw))
	for _, part := range raw {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 && strings.TrimSpace(text) != "" {
		return []string{strings.TrimSpace(text)}
	}
	return out
}

func clauseHasNegation(clause string) bool {
	lower := strings.ToLower(clause)
	for _, m := range clauseNegationMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func hasGlobalCancel(text string) bool {
	lower := strings.ToLower(text)
	for _, m := range globalCancelMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func clauseMentionsSpec(clause string, spec catalog.OperationSpec) bool {
	lower := strings.ToLower(clause)
	if spec.Guard != nil {
		for _, kw := range spec.Guard.DeviceKeywords {
			if kw != "" && strings.Contains(lower, strings.ToLower(kw)) {
				return true
			}
		}
	}
	for _, al := range spec.Aliases {
		if al != "" && strings.Contains(lower, strings.ToLower(al)) {
			return true
		}
	}
	return spec.Title != "" && strings.Contains(lower, strings.ToLower(spec.Title))
}

// evaluateNegation separates global cancellations and target-scoped negations
// from unrelated negated clauses (e.g. "先别开空调，把左后窗关上" allows window.close
// while still blocking hvac.*). Returns the remaining positive clauses for alignment.
func evaluateNegation(text string, spec catalog.OperationSpec) (string, error) {
	if hasGlobalCancel(text) {
		return "", &contract.BusinessError{
			Code:    contract.ClarificationRequired,
			Message: "检测到否定或取消意图，未执行车控操作",
		}
	}

	clauses := splitGuardClauses(text)
	var positive []string
	var negated []string
	for _, c := range clauses {
		if clauseHasNegation(c) {
			negated = append(negated, c)
		} else {
			positive = append(positive, c)
		}
	}
	if len(negated) == 0 {
		return text, nil
	}
	// Pure negation (no positive clauses at all) is always blocked.
	if len(positive) == 0 {
		return "", &contract.BusinessError{
			Code:    contract.ClarificationRequired,
			Message: "检测到否定或取消意图，未执行车控操作",
		}
	}
	// If any negated clause targets this operation's domain, block.
	for _, nc := range negated {
		if clauseMentionsSpec(nc, spec) {
			return "", &contract.BusinessError{
				Code:    contract.ClarificationRequired,
				Message: "检测到否定或取消意图，未执行车控操作",
			}
		}
	}
	positiveText := strings.Join(positive, "，")
	// Positive remainder must still explicitly reference this operation when another clause was negated.
	if !clauseMentionsSpec(positiveText, spec) && !hasExplicitActionForSpec(spec, positiveText) {
		return "", &contract.BusinessError{
			Code:    contract.ClarificationRequired,
			Message: "检测到否定或取消意图，未执行车控操作",
		}
	}
	return positiveText, nil
}

// evaluateWriteIntent enforces catalog-driven Go-side trusted intent, ambiguity,
// short-reply, and utterance-to-parameter alignment checks before any task write.
func evaluateWriteIntent(
	ctx context.Context,
	cat *catalog.Service,
	store tasks.Store,
	turn contract.TurnContext,
	mut contract.TaskMutation,
	isDispatch bool,
) error {
	text := strings.TrimSpace(turn.FinalTranscript.Text)
	if text == "" {
		return &contract.BusinessError{
			Code:    contract.ClarificationRequired,
			Message: "未获取到有效语音转写，请再说一次",
		}
	}

	// 1. Queries MUST NEVER produce vehicle task writes.
	if isReadOnlyQuery(text) {
		return &contract.BusinessError{
			Code:    contract.ClarificationRequired,
			Message: "当前语句为只读查询，不能执行车控写入",
		}
	}

	// Lookup target operation spec from catalog (unavailable ops fail fast).
	var targetSpec catalog.OperationSpec
	if cat != nil {
		sp, _, err := cat.LookupOperation(turn.Scope.VehicleID, mut.Operation)
		if err != nil {
			return err
		}
		targetSpec = sp
	}

	// 2. Negation / cancellation fence (clause-aware for compound sentences).
	positiveText, err := evaluateNegation(text, targetSpec)
	if err != nil {
		return err
	}

	// A power-only write must not drop a temperature request in the same utterance.
	if cat != nil && mut.Operation == (contract.Operation{Device: "hvac", Tool: "set_power"}) {
		temperature, _, lookupErr := cat.LookupOperation(turn.Scope.VehicleID, contract.Operation{Device: "hvac", Tool: "set_temperature"})
		if lookupErr == nil && hasExplicitActionForSpec(temperature, positiveText) {
			return &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: "原话同时要求调整温度，请使用 hvac.set_climate",
			}
		}
	}

	// Load live candidate records belonging to this turn's snapshot.
	candidates := make([]tasks.TaskRecord, 0, len(turn.Candidates))
	candidateByID := make(map[string]tasks.TaskRecord, len(turn.Candidates))
	for _, cv := range turn.Candidates {
		rec, err := store.GetTask(ctx, turn.Scope, cv.TaskID)
		if err != nil {
			continue
		}
		candidates = append(candidates, rec)
		candidateByID[rec.TaskID] = rec
	}

	// 3. Short reply without an explicit new action verb MUST NOT downgrade to a new task/command.
	if mut.TaskID == "" {
		if !hasExplicitActionForSpec(targetSpec, positiveText) {
			if len(candidates) > 0 {
				return &contract.BusinessError{
					Code:    contract.ClarificationRequired,
					Message: "短回复缺少对应待办 task_id 或明确指令，请确认具体操作",
				}
			}
			return &contract.BusinessError{
				Code:    contract.ClarificationRequired,
				Message: "缺少明确车控动作，请说明要执行的操作",
			}
		}
		return verifyUtteranceAlignment(targetSpec, positiveText, mut, nil, isDispatch)
	}

	// 4. Resuming / partially completing a pending candidate (task_id != "").
	target, exists := candidateByID[mut.TaskID]
	if !exists {
		return &contract.BusinessError{
			Code:    contract.TaskNotEligible,
			Message: "指定的 task_id 不在本轮待回复候选中",
		}
	}

	// Ambiguity check across ALL pending candidates in this turn's snapshot.
	matching := findMatchingCandidates(cat, turn.Scope.VehicleID, positiveText, candidates)
	if len(matching) > 1 {
		return &contract.BusinessError{
			Code:    contract.ClarificationRequired,
			Message: buildAmbiguityMessage(matching),
		}
	}
	if len(matching) == 1 && matching[0].TaskID != target.TaskID {
		return &contract.BusinessError{
			Code:    contract.ClarificationRequired,
			Message: "模型选择的待办任务与用户原话不匹配，请确认目标任务",
		}
	}

	return verifyUtteranceAlignment(targetSpec, positiveText, mut, &target, isDispatch)
}

func containsAny(text string, phrases []string) bool {
	lower := strings.ToLower(text)
	for _, p := range phrases {
		if p != "" && strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func hasExplicitActionForSpec(spec catalog.OperationSpec, text string) bool {
	if spec.Operation.Device == "" {
		return containsAny(text, genericControlVerbs)
	}

	deviceMentioned := clauseMentionsSpec(text, spec)
	if !deviceMentioned {
		// Check if any field's explicit enum aliases carry device context (e.g. "左后窗").
		for _, f := range spec.Fields {
			if f.Extractor != nil && f.Extractor.Kind == "enum_alias" {
				if len(extractEnumMatches(text, f)) > 0 && (f.Name == "position" || f.Name == "operate") {
					// Still requires an explicit verb!
					if spec.Guard != nil && containsAny(text, spec.Guard.PositiveVerbs) {
						return true
					}
				}
			}
		}
		return false
	}

	if spec.Guard != nil {
		if containsAny(text, spec.Guard.PositiveVerbs) || containsAny(text, spec.Guard.RelativeActionKeywords) {
			return true
		}
		// Numeric setting with device mention (e.g., "空调25度", "座椅加热2档") counts as explicit action.
		for _, f := range spec.Fields {
			if f.Extractor != nil && (f.Extractor.Kind == "number" || f.Extractor.Kind == "integer" || f.Extractor.Kind == "relative_tune") {
				if hasFieldExtraction(text, f) {
					return true
				}
			}
		}
		return false
	}
	return containsAny(text, genericControlVerbs)
}

func findMatchingCandidates(cat *catalog.Service, vehicleID, text string, candidates []tasks.TaskRecord) []tasks.TaskRecord {
	var matched []tasks.TaskRecord
	for _, c := range candidates {
		if c.Status != contract.PendingReply {
			continue
		}
		if candidateMatchesUtterance(cat, vehicleID, text, c) {
			matched = append(matched, c)
		}
	}
	return matched
}

func candidateMatchesUtterance(cat *catalog.Service, vehicleID, text string, c tasks.TaskRecord) bool {
	if cat == nil {
		return false
	}
	spec, _, err := cat.LookupOperation(vehicleID, c.Operation)
	if err != nil {
		return false
	}

	fieldByName := make(map[string]catalog.FieldSpec, len(spec.Fields))
	for _, f := range spec.Fields {
		fieldByName[f.Name] = f
	}

	// Check whether utterance provides at least one missing field value.
	suppliesMissing := false
	for _, mf := range c.MissingFields {
		// Handle RequireAnyOf composite missing entries like "power|level".
		for _, part := range strings.Split(mf, "|") {
			if f, ok := fieldByName[strings.TrimSpace(part)]; ok {
				if hasFieldExtraction(text, f) {
					suppliesMissing = true
					break
				}
			}
		}
		if suppliesMissing {
			break
		}
	}
	if !suppliesMissing {
		return false
	}

	// Disambiguate using explicit operation keywords/verbs when user stated them:
	// 1. If conflict verb stated without positive verb -> this candidate does NOT match.
	if spec.Guard != nil {
		hasPos := containsAny(text, spec.Guard.PositiveVerbs)
		hasConflict := containsAny(text, spec.Guard.ConflictVerbs)
		if hasConflict && !hasPos {
			return false
		}
	}

	// 2. If user explicitly named a specific tool/title or anti-example, honor it.
	for _, ae := range spec.AntiExamples {
		if ae != "" && strings.Contains(strings.ToLower(text), strings.ToLower(ae)) {
			return false
		}
	}
	return true
}

func buildAmbiguityMessage(matched []tasks.TaskRecord) string {
	titles := make([]string, 0, len(matched))
	for _, m := range matched {
		titles = append(titles, fmt.Sprintf("%s.%s", m.Operation.Device, m.Operation.Tool))
	}
	return fmt.Sprintf("存在多条匹配的待办任务（%s），请明确您要继续哪一项操作", strings.Join(titles, " / "))
}

func verifyUtteranceAlignment(
	spec catalog.OperationSpec,
	text string,
	mut contract.TaskMutation,
	target *tasks.TaskRecord,
	isDispatch bool,
) error {
	// 1. Check verb conflict against operation's declared positive/conflict verbs.
	if spec.Guard != nil && len(spec.Guard.ConflictVerbs) > 0 {
		hasPos := containsAny(text, spec.Guard.PositiveVerbs)
		hasConflict := containsAny(text, spec.Guard.ConflictVerbs)
		if hasConflict && !hasPos {
			return &contract.BusinessError{
				Code:    contract.ClarificationRequired,
				Message: fmt.Sprintf("用户原话动作与操作 %s.%s 冲突，请确认操作", mut.Operation.Device, mut.Operation.Tool),
			}
		}
	}

	fieldByName := make(map[string]catalog.FieldSpec, len(spec.Fields))
	for _, f := range spec.Fields {
		fieldByName[f.Name] = f
	}

	// Precompute whether any explicit non-power numeric/mode/tune field was extracted
	// (e.g., "把空调调到25度" implicitly implies power="on" without repeating "打开").
	hasImpliedOnSignal := false
	for _, f := range spec.Fields {
		if f.Name == "power" || f.Extractor == nil {
			continue
		}
		if (f.Extractor.Kind == "number" || f.Extractor.Kind == "integer" || f.Extractor.Kind == "relative_tune") && hasFieldExtraction(text, f) {
			hasImpliedOnSignal = true
			break
		}
	}

	// 2. Check every submitted parameter against its declarative extractor.
	for k, rawVal := range mut.Params {
		if rawVal == nil {
			continue
		}
		f, ok := fieldByName[k]
		if !ok || f.Extractor == nil {
			continue
		}
		if alreadyKnownValue(target, k, rawVal) {
			continue
		}
		if err := verifyFieldAlignment(f, text, rawVal, hasImpliedOnSignal); err != nil {
			return err
		}
	}

	// 3. When dispatching, ensure required fields with extractors weren't left empty unless known on target.
	if isDispatch {
		for _, f := range spec.Fields {
			if !f.Required || f.Extractor == nil {
				continue
			}
			_, supplied := mut.Params[f.Name]
			_, known := targetKnown(target, f.Name)
			if !supplied && !known {
				return &contract.BusinessError{
					Code:    contract.InvalidArgument,
					Message: fmt.Sprintf("missing required parameter %s", f.Name),
				}
			}
		}
	}
	return nil
}

func targetKnown(target *tasks.TaskRecord, key string) (any, bool) {
	if target == nil || target.KnownParams == nil {
		return nil, false
	}
	v, ok := target.KnownParams[key]
	return v, ok && v != nil
}

func alreadyKnownValue(target *tasks.TaskRecord, key string, submitted any) bool {
	known, ok := targetKnown(target, key)
	if !ok {
		return false
	}
	if kn, okK := toNumber(known); okK {
		if sn, okS := toNumber(submitted); okS {
			return math.Abs(kn-sn) < 0.05
		}
	}
	return fmt.Sprintf("%v", known) == fmt.Sprintf("%v", submitted)
}

func verifyFieldAlignment(f catalog.FieldSpec, text string, submitted any, hasImpliedOnSignal bool) error {
	ext := f.Extractor
	switch ext.Kind {
	case "enum_alias":
		subStr, ok := submitted.(string)
		if !ok || strings.TrimSpace(subStr) == "" {
			return &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q must be a non-empty string", f.Name),
			}
		}
		extracted := extractEnumMatches(text, f)

		// Special rule for "power" ("on"/"off"): allow implied "on" when user gave an explicit
		// numeric/tune target (e.g. "把空调调到25度"), unless "off" was explicitly stated!
		if f.Name == "power" && subStr == "on" && len(extracted) == 0 && hasImpliedOnSignal {
			return nil
		}

		if len(extracted) == 0 {
			return &contract.BusinessError{
				Code:    contract.ClarificationRequired,
				Message: fmt.Sprintf("原话未指定参数 %s，请先追问用户", f.Name),
			}
		}
		// Support comma-separated multi-target strings (e.g., positionList: "front_left,front_right").
		parts := strings.Split(subStr, ",")
		for _, part := range parts {
			p := strings.TrimSpace(part)
			if !containsStringSlice(extracted, p) {
				return &contract.BusinessError{
					Code:    contract.ClarificationRequired,
					Message: fmt.Sprintf("提交的参数 %s=%q 与用户原话不一致，请确认", f.Name, subStr),
				}
			}
		}
		return nil

	case "number", "integer":
		subNum, ok := toNumber(submitted)
		if !ok {
			if s, okS := submitted.(string); okS {
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
					subNum, ok = parsed, true
				}
			}
		}
		if !ok {
			return &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q must be numeric", f.Name),
			}
		}
		nums := extractNumbers(text, ext)
		for _, n := range nums {
			if math.Abs(n-subNum) < 0.05 {
				return nil
			}
		}
		return &contract.BusinessError{
			Code:    contract.ClarificationRequired,
			Message: fmt.Sprintf("提交的参数 %s=%v 与用户原话不一致，请确认", f.Name, submitted),
		}

	case "relative_tune":
		subStr, ok := submitted.(string)
		if !ok || strings.TrimSpace(subStr) == "" {
			return &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q must be a signed relative string", f.Name),
			}
		}
		expectedDelta, okDelta := extractRelativeDelta(text, ext)
		if !okDelta {
			return &contract.BusinessError{
				Code:    contract.ClarificationRequired,
				Message: fmt.Sprintf("原话未表达相对调节意图，不能提交 %s=%q", f.Name, subStr),
			}
		}
		subDelta, err := strconv.ParseFloat(strings.TrimSpace(subStr), 64)
		if err != nil {
			return &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: fmt.Sprintf("parameter %q=%q is not a valid numeric delta", f.Name, subStr),
			}
		}
		// Sign must match AND magnitude must match when user stated an explicit number.
		if (expectedDelta > 0 && subDelta <= 0) || (expectedDelta < 0 && subDelta >= 0) {
			return &contract.BusinessError{
				Code:    contract.ClarificationRequired,
				Message: fmt.Sprintf("提交的调节方向 %s=%q 与用户原话相反，请确认", f.Name, subStr),
			}
		}
		if math.Abs(math.Abs(expectedDelta)-math.Abs(subDelta)) > 0.05 {
			return &contract.BusinessError{
				Code:    contract.ClarificationRequired,
				Message: fmt.Sprintf("提交的调节幅度 %s=%q 与用户原话不一致，请确认", f.Name, subStr),
			}
		}
		return nil

	default: // "free_text" or unspecified
		return nil
	}
}

func hasFieldExtraction(text string, f catalog.FieldSpec) bool {
	if f.Extractor == nil {
		return false
	}
	switch f.Extractor.Kind {
	case "enum_alias":
		return len(extractEnumMatches(text, f)) > 0
	case "number", "integer":
		return len(extractNumbers(text, f.Extractor)) > 0
	case "relative_tune":
		_, ok := extractRelativeDelta(text, f.Extractor)
		return ok
	default:
		return strings.TrimSpace(text) != ""
	}
}

func extractEnumMatches(text string, f catalog.FieldSpec) []string {
	lower := strings.ToLower(text)
	aliases := f.Extractor.ValueAliases
	if len(aliases) == 0 {
		aliases = f.EnumLabels
	}
	var out []string
	seen := make(map[string]bool)
	for val, words := range aliases {
		for _, w := range words {
			if w != "" && strings.Contains(lower, strings.ToLower(w)) {
				if !seen[val] {
					seen[val] = true
					out = append(out, val)
				}
				break
			}
		}
	}
	return out
}

func extractNumbers(text string, ext *catalog.FieldExtractorSpec) []float64 {
	normalized := cnSmallNumReplacer.Replace(strings.ToLower(text))
	var re *regexp.Regexp
	if ext.UnitPattern != "" && !ext.AllowBareNumber {
		re = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*` + ext.UnitPattern)
	} else if ext.UnitPattern != "" {
		re = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*` + ext.UnitPattern)
	} else {
		re = bareNumberRe
	}
	matches := re.FindAllStringSubmatch(normalized, -1)
	var out []float64
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		if ext.Min != nil && v < *ext.Min {
			continue
		}
		if ext.Max != nil && v > *ext.Max {
			continue
		}
		out = append(out, v)
	}
	return out
}

func extractRelativeDelta(text string, ext *catalog.FieldExtractorSpec) (float64, bool) {
	normalized := cnSmallNumReplacer.Replace(strings.ToLower(text))
	up := containsAny(normalized, relativeUpMarkers)
	down := containsAny(normalized, relativeDownMarkers)
	if !up && !down {
		return 0, false
	}
	if up && down {
		return 0, false
	}
	sign := 1.0
	if down {
		sign = -1.0
	}
	// Look for explicit delta number (e.g. "升高3度" -> 3, "调大一档" -> 1); default magnitude 1 when "高一点"/"低一点".
	var mag float64 = 1.0
	re := bareNumberRe
	if ext != nil && ext.UnitPattern != "" {
		re = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*` + ext.UnitPattern)
	}
	if m := re.FindStringSubmatch(normalized); len(m) >= 2 {
		if parsed, err := strconv.ParseFloat(m[1], 64); err == nil && parsed > 0 {
			mag = parsed
		}
	}
	return sign * mag, true
}

func containsStringSlice(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}

func toNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
