package mcp

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/tasks"
)

var (
	tempNumberRe = regexp.MustCompile(`(\d{1,2}(?:\.\d+)?)\s*(?:度|℃|°c|c)?`)

	readOnlyQueryMarkers = []string{
		"关了吗", "关没关", "关好没", "关好了吗", "开着吗", "开没开", "打开了吗",
		"状态", "多少度了", "几度了", "现在几度", "怎么用", "在哪里设置", "在哪设置",
		"哪儿设置", "怎么设置", "说明书", "手册", "还有什么没做", "哪些任务", "待办",
	}

	negationMarkers = []string{
		"别关", "不要关", "不用关", "别开", "不要开", "不用开", "别调", "不要调",
		"不用调", "算了", "取消", "不用了", "先不管", "先不用", "别动", "不要动",
	}

	positionAliases = map[string][]string{
		"front_left":  {"左前窗", "左前", "主驾窗", "驾驶座窗", "左前车窗"},
		"front_right": {"右前窗", "右前", "副驾窗", "右前车窗"},
		"rear_left":   {"左后窗", "左后", "左后车窗"},
		"rear_right":  {"右后窗", "右后", "右后车窗"},
	}
)

type utteranceSignals struct {
	Raw          string
	IsQuery      bool
	IsNegation   bool
	WindowClose  bool
	WindowOpen   bool
	WindowMention bool
	Positions    []string
	HVACMention  bool
	HVACPower    string // "on", "off", or ""
	TempC        *float64
	HasAction    bool
}

func analyzeUtterance(raw string) utteranceSignals {
	text := strings.TrimSpace(raw)
	lower := strings.ToLower(text)
	sig := utteranceSignals{Raw: text}

	for _, m := range readOnlyQueryMarkers {
		if strings.Contains(lower, m) {
			sig.IsQuery = true
			break
		}
	}
	if strings.HasSuffix(text, "吗") || strings.HasSuffix(text, "吗？") || strings.HasSuffix(text, "吗?") {
		// Distinguish polite commands ("把车窗关一下好吗" is rare; status questions end with 关了吗/开着吗/开着没).
		if strings.Contains(text, "开着") || strings.Contains(text, "关着") || strings.Contains(text, "了吗") || strings.Contains(text, "几度") {
			sig.IsQuery = true
		}
	}

	for _, m := range negationMarkers {
		if strings.Contains(lower, m) {
			sig.IsNegation = true
			break
		}
	}

	for pos, words := range positionAliases {
		for _, w := range words {
			if strings.Contains(text, w) {
				sig.Positions = append(sig.Positions, pos)
				break
			}
		}
	}

	if strings.Contains(text, "车窗") || strings.Contains(text, "窗户") || len(sig.Positions) > 0 {
		sig.WindowMention = true
	}
	if strings.Contains(text, "关") || strings.Contains(text, "升") || strings.Contains(text, "摇上") {
		if sig.WindowMention && !strings.Contains(text, "空调") {
			sig.WindowClose = true
		}
	}
	if strings.Contains(text, "开") || strings.Contains(text, "降") || strings.Contains(text, "摇下") {
		if sig.WindowMention && !strings.Contains(text, "空调") {
			sig.WindowOpen = true
		}
	}
	// Handle explicit verb + position like "关左后窗" / "开左后窗" even without "车窗".
	if strings.Contains(text, "空调") || strings.Contains(text, "冷风") || strings.Contains(text, "暖风") || strings.Contains(text, "温度") || strings.Contains(text, "度") {
		sig.HVACMention = true
	}
	if strings.Contains(text, "打开空调") || strings.Contains(text, "开空调") || strings.Contains(text, "开启空调") || strings.Contains(text, "空调开") || text == "打开" || text == "开启" {
		sig.HVACPower = "on"
	}
	if strings.Contains(text, "关闭空调") || strings.Contains(text, "关空调") || strings.Contains(text, "空调关") || text == "关闭" {
		sig.HVACPower = "off"
	}

	if m := tempNumberRe.FindStringSubmatch(lower); len(m) == 2 {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v >= 10 && v <= 40 {
			sig.TempC = &v
		}
	}

	sig.HasAction = sig.WindowClose || sig.WindowOpen ||
		(strings.Contains(text, "空调") && (sig.HVACPower != "" || strings.Contains(text, "调") || strings.Contains(text, "设") || sig.TempC != nil)) ||
		(strings.Contains(text, "温度") && (strings.Contains(text, "调") || strings.Contains(text, "设") || sig.TempC != nil))

	return sig
}

// evaluateWriteIntent enforces Go-side trusted intent, ambiguity, short-reply,
// and utterance-to-parameter alignment checks before allowing any task write.
func evaluateWriteIntent(
	ctx context.Context,
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
	sig := analyzeUtterance(text)

	// 1. Queries and negations MUST NEVER produce vehicle task writes.
	if sig.IsQuery {
		return &contract.BusinessError{
			Code:    contract.ClarificationRequired,
			Message: "当前语句为只读查询，不能执行车控写入",
		}
	}
	if sig.IsNegation {
		return &contract.BusinessError{
			Code:    contract.ClarificationRequired,
			Message: "检测到否定或取消意图，未执行车控操作",
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

	// 2. Short reply without an explicit new action verb MUST NOT downgrade to a new task/command.
	if mut.TaskID == "" {
		if !sig.HasAction {
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
		return verifyUtteranceAlignment(sig, mut, nil, isDispatch)
	}

	// 3. Resuming / partially completing a pending candidate (task_id != "").
	target, exists := candidateByID[mut.TaskID]
	if !exists {
		return &contract.BusinessError{
			Code:    contract.TaskNotEligible,
			Message: "指定的 task_id 不在本轮待回复候选中",
		}
	}

	// Check ambiguity across ALL pending candidates in this turn's snapshot:
	// if multiple candidates reasonably match this utterance and the user did not
	// explicitly disambiguate the operation, refuse the write and ask for clarification.
	matching := findMatchingCandidates(sig, candidates)
	if len(matching) > 1 {
		return &contract.BusinessError{
			Code:    contract.ClarificationRequired,
			Message: buildAmbiguityMessage(sig, matching),
		}
	}
	if len(matching) == 1 && matching[0].TaskID != target.TaskID {
		return &contract.BusinessError{
			Code:    contract.ClarificationRequired,
			Message: "模型选择的待办任务与用户原话不匹配，请确认目标任务",
		}
	}

	return verifyUtteranceAlignment(sig, mut, &target, isDispatch)
}

func findMatchingCandidates(sig utteranceSignals, candidates []tasks.TaskRecord) []tasks.TaskRecord {
	var matched []tasks.TaskRecord
	for _, c := range candidates {
		if c.Status != contract.PendingReply {
			continue
		}
		if candidateMatchesUtterance(sig, c) {
			matched = append(matched, c)
		}
	}
	return matched
}

func candidateMatchesUtterance(sig utteranceSignals, c tasks.TaskRecord) bool {
	missingSet := make(map[string]bool, len(c.MissingFields))
	for _, f := range c.MissingFields {
		missingSet[f] = true
	}

	switch c.Operation.Device {
	case "window":
		if !missingSet["position"] || len(sig.Positions) == 0 {
			return false
		}
		// If user explicitly stated open vs close, filter by the stated verb.
		if sig.WindowClose && !sig.WindowOpen {
			return c.Operation.Tool == "close"
		}
		if sig.WindowOpen && !sig.WindowClose {
			return c.Operation.Tool == "open"
		}
		return true

	case "hvac":
		if missingSet["temperature_c"] && sig.TempC != nil {
			return true
		}
		if missingSet["power"] && sig.HVACPower != "" {
			return true
		}
		return false
	default:
		return false
	}
}

func buildAmbiguityMessage(sig utteranceSignals, matched []tasks.TaskRecord) string {
	if len(sig.Positions) == 1 {
		posCN := positionLabelCN(sig.Positions[0])
		hasOpen, hasClose := false, false
		for _, m := range matched {
			if m.Operation.Device == "window" && m.Operation.Tool == "open" {
				hasOpen = true
			}
			if m.Operation.Device == "window" && m.Operation.Tool == "close" {
				hasClose = true
			}
		}
		if hasOpen && hasClose {
			return fmt.Sprintf("请确认要打开还是关闭%s", posCN)
		}
	}
	return "存在多条匹配的待办任务，请明确您要继续哪一项操作"
}

func positionLabelCN(pos string) string {
	switch pos {
	case "front_left":
		return "左前窗"
	case "front_right":
		return "右前窗"
	case "rear_left":
		return "左后窗"
	case "rear_right":
		return "右后窗"
	default:
		return "车窗"
	}
}

func verifyUtteranceAlignment(sig utteranceSignals, mut contract.TaskMutation, target *tasks.TaskRecord, isDispatch bool) error {
	switch mut.Operation.Device {
	case "window":
		if mut.Operation.Tool == "close" && sig.WindowOpen && !sig.WindowClose {
			return &contract.BusinessError{
				Code:    contract.ClarificationRequired,
				Message: "用户原话为开窗而非关窗，请确认操作",
			}
		}
		if mut.Operation.Tool == "open" && sig.WindowClose && !sig.WindowOpen {
			return &contract.BusinessError{
				Code:    contract.ClarificationRequired,
				Message: "用户原话为关窗而非开窗，请确认操作",
			}
		}
		if pos, ok := mut.Params["position"].(string); ok && pos != "" {
			// If user didn't mention any position (and it's not already known on target), refuse hallucinated position!
			alreadyKnown := target != nil && target.KnownParams["position"] == pos
			if !alreadyKnown {
				if len(sig.Positions) == 0 {
					return &contract.BusinessError{
						Code:    contract.ClarificationRequired,
						Message: "原话未指定车窗位置，请先追问用户",
					}
				}
				found := false
				for _, p := range sig.Positions {
					if p == pos {
						found = true
						break
					}
				}
				if !found {
					return &contract.BusinessError{
						Code:    contract.ClarificationRequired,
						Message: "提交的车窗位置与用户原话不一致，请确认",
					}
				}
			}
		} else if isDispatch && (target == nil || target.KnownParams["position"] == nil) {
			return &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: "missing required parameter position",
			}
		}

	case "hvac":
		if rawTemp, ok := mut.Params["temperature_c"]; ok && rawTemp != nil {
			submitted, okNum := toNumber(rawTemp)
			if !okNum {
				return &contract.BusinessError{
					Code:    contract.InvalidArgument,
					Message: "temperature_c must be numeric",
				}
			}
			alreadyKnown := false
			if target != nil {
				if kt, okKT := toNumber(target.KnownParams["temperature_c"]); okKT && math.Abs(kt-submitted) < 0.05 {
					alreadyKnown = true
				}
			}
			if !alreadyKnown {
				if sig.TempC == nil || math.Abs(*sig.TempC-submitted) > 0.05 {
					return &contract.BusinessError{
						Code:    contract.ClarificationRequired,
						Message: "提交的空调温度与用户原话不一致，请确认",
					}
				}
			}
		}
		if rawPower, ok := mut.Params["power"].(string); ok && rawPower != "" {
			alreadyKnown := target != nil && target.KnownParams["power"] == rawPower
			if !alreadyKnown && sig.HVACPower != "" && sig.HVACPower != rawPower {
				return &contract.BusinessError{
					Code:    contract.ClarificationRequired,
					Message: "提交的空调开关状态与用户原话不一致，请确认",
				}
			}
		}
	}
	return nil
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
	default:
		return 0, false
	}
}
