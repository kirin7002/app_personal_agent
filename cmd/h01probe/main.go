// Command h01probe runs a Go Streamable HTTP MCP probe server and drives a real
// Hermes vgit.72c8803 gateway instance (via hermes/gateway_launcher.py) to verify
// H01 trusted per-turn binding, Scenario A/B, concurrent multi-user isolation,
// prompt-injection defense, late/cancelled call rejection, Idempotency-Key replay
// & conflict, SSE disconnect/resubscribe, and terminal polling.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

const (
	metaTurnHandleKey    = "vehicle.turn_handle"
	metaHermesSessionKey = "vehicle.hermes_session_id"
	mcpProtocolVersion   = "2025-11-25"
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type callToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Meta      map[string]any `json:"_meta,omitempty"`
}

type toolCallObservation struct {
	Timestamp             time.Time      `json:"timestamp"`
	ToolName              string         `json:"tool_name"`
	Arguments             map[string]any `json:"arguments"`
	HeaderTurnHandle      string         `json:"header_turn_handle"`
	HeaderHermesSessionID string         `json:"header_hermes_session_id"`
	MetaTurnHandle        string         `json:"meta_turn_handle"`
	MetaHermesSessionID   string         `json:"meta_hermes_session_id"`
	ResolvedScope         contract.Scope `json:"resolved_scope,omitempty"`
	ResolvedRequestID     string         `json:"resolved_request_id,omitempty"`
	IsError               bool           `json:"is_error"`
	ResultPayload         any            `json:"result_payload"`
}

type registeredTurn struct {
	Context   contract.TurnContext
	Cancelled bool
	Writes    int
}

type probeTask struct {
	TaskID        string
	Scope         contract.Scope
	Operation     contract.Operation
	KnownParams   map[string]any
	MissingFields []string
	Question      string
	Status        contract.Status
	CommandID     string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	Version       uint64
}

type probeMCPServer struct {
	serviceToken string

	mu           sync.Mutex
	turns        map[string]*registeredTurn
	tasks        map[string]*probeTask
	observations []toolCallObservation
	nextTaskNum  int
	nextCmdNum   int
	onCallHook   func(toolName string)
}

func newProbeMCPServer(serviceToken string) *probeMCPServer {
	return &probeMCPServer{
		serviceToken: serviceToken,
		turns:        make(map[string]*registeredTurn),
		tasks:        make(map[string]*probeTask),
		nextTaskNum:  101,
		nextCmdNum:   88,
	}
}

func (s *probeMCPServer) RegisterTurn(tc contract.TurnContext) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns[tc.TurnHandle] = &registeredTurn{Context: tc}
}

func (s *probeMCPServer) CancelTurn(turnHandle string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rt, ok := s.turns[turnHandle]; ok {
		rt.Cancelled = true
	}
}

func (s *probeMCPServer) SeedTask(task *probeTask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[task.TaskID] = task
}

func (s *probeMCPServer) Observations() []toolCallObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]toolCallObservation, len(s.observations))
	copy(out, s.observations)
	return out
}

func (s *probeMCPServer) ClearObservations() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observations = nil
}

func (s *probeMCPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.serviceToken != "" {
		want := "Bearer " + s.serviceToken
		if r.Header.Get("Authorization") != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusOK, jsonRPCResponse{
			JSONRPC: "2.0",
			Error:   &jsonRPCError{Code: -32700, Message: "parse error"},
		})
		return
	}

	// JSON-RPC notifications have no id; acknowledge with 202 Accepted per MCP Streamable HTTP.
	if len(req.ID) == 0 || string(req.ID) == "null" || strings.HasPrefix(req.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("MCP-Protocol-Version", mcpProtocolVersion)
	switch req.Method {
	case "initialize":
		writeJSON(w, http.StatusOK, jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": mcpProtocolVersion,
				"capabilities": map[string]any{
					"tools": map[string]any{"listChanged": false},
				},
				"serverInfo": map[string]any{
					"name":    "vehicle-h01-probe",
					"version": "0.1.0",
				},
			},
		})
	case "ping":
		writeJSON(w, http.StatusOK, jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{},
		})
	case "tools/list":
		writeJSON(w, http.StatusOK, jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": mcpToolDefinitions(),
			},
		})
	case "tools/call":
		res := s.handleToolCall(r, req.Params)
		writeJSON(w, http.StatusOK, jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  res,
		})
	default:
		writeJSON(w, http.StatusOK, jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonRPCError{Code: -32601, Message: "method not found: " + req.Method},
		})
	}
}

func (s *probeMCPServer) handleToolCall(r *http.Request, rawParams json.RawMessage) map[string]any {
	var params callToolParams
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return mcpBusinessError(contract.BusinessError{
			Code:    contract.InvalidArgument,
			Message: "invalid tools/call params",
		})
	}
	if s.onCallHook != nil {
		s.onCallHook(params.Name)
	}

	hdrHandle := strings.TrimSpace(r.Header.Get(contract.TurnHandleHeader))
	hdrSession := strings.TrimSpace(r.Header.Get(contract.HermesSessionHeader))
	metaHandle, _ := params.Meta[metaTurnHandleKey].(string)
	metaSession, _ := params.Meta[metaHermesSessionKey].(string)

	obs := toolCallObservation{
		Timestamp:             time.Now().UTC(),
		ToolName:              params.Name,
		Arguments:             params.Arguments,
		HeaderTurnHandle:      hdrHandle,
		HeaderHermesSessionID: hdrSession,
		MetaTurnHandle:        metaHandle,
		MetaHermesSessionID:   metaSession,
	}

	recordAndReturn := func(isErr bool, payload any, mcpRes map[string]any) map[string]any {
		obs.IsError = isErr
		obs.ResultPayload = payload
		s.mu.Lock()
		s.observations = append(s.observations, obs)
		s.mu.Unlock()
		return mcpRes
	}

	// Strict trusted metadata verification:
	// Both HTTP header and MCP _meta must be present and agree, and resolve to an active turn.
	if hdrHandle == "" || hdrSession == "" || metaHandle != hdrHandle || metaSession != hdrSession {
		be := contract.BusinessError{
			Code:    contract.TurnContextInvalid,
			Message: "missing or inconsistent trusted turn metadata",
		}
		return recordAndReturn(true, be, mcpBusinessError(be))
	}

	// Reject any attempt by the model to smuggle trusted identity fields inside tool arguments.
	for _, forbidden := range []string{"turn_handle", "user_id", "session_id", "vehicle_id", "hermes_session_id", "_meta"} {
		if _, exists := params.Arguments[forbidden]; exists {
			be := contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: "untrusted identity field in tool arguments: " + forbidden,
			}
			return recordAndReturn(true, be, mcpBusinessError(be))
		}
	}

	now := time.Now().UTC()
	s.mu.Lock()
	rt, ok := s.turns[hdrHandle]
	if !ok || rt.Cancelled || !rt.Context.ExpiresAt.After(now) || rt.Context.HermesSessionID != hdrSession {
		s.mu.Unlock()
		be := contract.BusinessError{
			Code:    contract.TurnContextInvalid,
			Message: "轮次已失效、已取消或与会话不匹配",
		}
		return recordAndReturn(true, be, mcpBusinessError(be))
	}
	tc := rt.Context
	obs.ResolvedScope = tc.Scope
	obs.ResolvedRequestID = tc.RequestID
	s.mu.Unlock()

	switch params.Name {
	case contract.CreateVehicleTask:
		res, be := s.execCreateTask(tc, rt, params.Arguments, now)
		if be != nil {
			return recordAndReturn(true, *be, mcpBusinessError(*be))
		}
		return recordAndReturn(false, res, mcpSuccessResult(res))
	case contract.DispatchVehicleCommand:
		res, be := s.execDispatch(tc, rt, params.Arguments, now)
		if be != nil {
			return recordAndReturn(true, *be, mcpBusinessError(*be))
		}
		return recordAndReturn(false, res, mcpSuccessResult(res))
	case contract.ListVehicleTasks:
		res := s.execListTasks(tc, now)
		return recordAndReturn(false, res, mcpSuccessResult(res))
	case contract.GetVehicleStatus:
		res := contract.VehicleStatusResult{
			Signals: []contract.Signal{{
				Name:         "window.rear_left",
				Value:        "open",
				SampledAt:    &now,
				Availability: "fresh",
			}},
		}
		return recordAndReturn(false, res, mcpSuccessResult(res))
	case contract.SearchVehicleManual:
		res := contract.SearchManualResult{
			Excerpts: []contract.ManualExcerpt{{
				TopicID: "window.auto_close",
				Section: "车门与车窗",
				Text:    "在中控屏车辆设置-车窗中可开启锁车自动关窗。",
				Source:  "manual-v2026.1#ch3",
				Version: "2026.1",
			}},
		}
		return recordAndReturn(false, res, mcpSuccessResult(res))
	case contract.SearchVehicleCapabilities:
		res := contract.SearchCapabilitiesResult{
			Capabilities: defaultCapabilities(),
		}
		return recordAndReturn(false, res, mcpSuccessResult(res))
	default:
		be := contract.BusinessError{
			Code:    contract.CapabilityUnavailable,
			Message: "unknown tool: " + params.Name,
		}
		return recordAndReturn(true, be, mcpBusinessError(be))
	}
}

func (s *probeMCPServer) execCreateTask(
	tc contract.TurnContext,
	rt *registeredTurn,
	args map[string]any,
	now time.Time,
) (contract.TaskResult, *contract.BusinessError) {
	device, _ := args["device"].(string)
	tool, _ := args["tool"].(string)
	rawParams, _ := args["params"].(map[string]any)
	if rawParams == nil {
		rawParams = map[string]any{}
	}
	if device == "" || tool == "" {
		return contract.TaskResult{}, &contract.BusinessError{
			Code:    contract.InvalidArgument,
			Message: "device and tool are required",
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if rt.Writes >= 1 {
		return contract.TaskResult{}, &contract.BusinessError{
			Code:    contract.IdempotencyConflict,
			Message: "同一轮次已提交另一项操作",
		}
	}
	taskID := fmt.Sprintf("T%d", s.nextTaskNum)
	s.nextTaskNum++

	missing := []string{"position"}
	question := "要关哪扇车窗？"
	if device == "hvac" {
		missing = []string{"temperature_c"}
		question = "空调要调到几度？"
	}
	exp := now.Add(contract.PendingTTL)
	s.tasks[taskID] = &probeTask{
		TaskID:        taskID,
		Scope:         tc.Scope,
		Operation:     contract.Operation{Device: device, Tool: tool},
		KnownParams:   rawParams,
		MissingFields: missing,
		Question:      question,
		Status:        contract.PendingReply,
		CreatedAt:     now,
		ExpiresAt:     exp,
		Version:       1,
	}
	rt.Writes++
	return contract.TaskResult{
		TaskID:        taskID,
		Status:        contract.PendingReply,
		MissingFields: missing,
		Question:      question,
		ExpiresAt:     &exp,
	}, nil
}

func (s *probeMCPServer) execDispatch(
	tc contract.TurnContext,
	rt *registeredTurn,
	args map[string]any,
	now time.Time,
) (contract.TaskResult, *contract.BusinessError) {
	device, _ := args["device"].(string)
	tool, _ := args["tool"].(string)
	taskID, _ := args["task_id"].(string)
	rawParams, _ := args["params"].(map[string]any)
	if rawParams == nil {
		rawParams = map[string]any{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if rt.Writes >= 1 {
		return contract.TaskResult{}, &contract.BusinessError{
			Code:    contract.IdempotencyConflict,
			Message: "同一轮次已提交另一项操作",
		}
	}

	cmdID := fmt.Sprintf("C%d", s.nextCmdNum)
	s.nextCmdNum++

	if taskID != "" {
		inCandidates := false
		for _, c := range tc.Candidates {
			if c.TaskID == taskID {
				inCandidates = true
				break
			}
		}
		if !inCandidates {
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskNotEligible,
				Message: "task_id is not in this turn's candidate snapshot",
			}
		}
		task, ok := s.tasks[taskID]
		if !ok || task.Scope != tc.Scope || task.Status != contract.PendingReply {
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskNotEligible,
				Message: "task not found or not pending_reply in scope",
			}
		}
		if !task.ExpiresAt.After(now) {
			task.Status = contract.Expired
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskExpired,
				Message: "待回复任务已到期",
			}
		}
		if task.Operation.Device != device || task.Operation.Tool != tool {
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskConflict,
				Message: "device/tool mismatch with pending task",
			}
		}
		task.Status = contract.PendingDispatch
		task.CommandID = cmdID
		task.Version++
		rt.Writes++
		return contract.TaskResult{
			TaskID:    taskID,
			CommandID: cmdID,
			Status:    contract.PendingDispatch,
		}, nil
	}

	newTaskID := fmt.Sprintf("T%d", s.nextTaskNum)
	s.nextTaskNum++
	s.tasks[newTaskID] = &probeTask{
		TaskID:      newTaskID,
		Scope:       tc.Scope,
		Operation:   contract.Operation{Device: device, Tool: tool},
		KnownParams: rawParams,
		Status:      contract.PendingDispatch,
		CommandID:   cmdID,
		CreatedAt:   now,
		Version:     1,
	}
	rt.Writes++
	return contract.TaskResult{
		TaskID:    newTaskID,
		CommandID: cmdID,
		Status:    contract.PendingDispatch,
	}, nil
}

func (s *probeMCPServer) execListTasks(tc contract.TurnContext, now time.Time) contract.ListTasksResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	var tasks []contract.TaskResult
	for _, t := range s.tasks {
		if t.Scope != tc.Scope {
			continue
		}
		if t.Status == contract.PendingReply && !t.ExpiresAt.After(now) {
			continue
		}
		exp := t.ExpiresAt
		tasks = append(tasks, contract.TaskResult{
			TaskID:        t.TaskID,
			CommandID:     t.CommandID,
			Status:        t.Status,
			MissingFields: t.MissingFields,
			Question:      t.Question,
			ExpiresAt:     &exp,
		})
	}
	return contract.ListTasksResult{Tasks: tasks}
}

func mcpSuccessResult(payload any) map[string]any {
	raw, _ := json.Marshal(payload)
	return map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": string(raw),
		}},
		"structuredContent": payload,
		"isError":           false,
	}
}

func mcpBusinessError(be contract.BusinessError) map[string]any {
	raw, _ := json.Marshal(be)
	return map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": string(raw),
		}},
		"structuredContent": be,
		"isError":           true,
	}
}

func defaultCapabilities() []contract.Capability {
	return []contract.Capability{
		{
			Operation:    contract.Operation{Device: "window", Tool: "close"},
			MCPTool:      contract.DispatchVehicleCommand,
			ParamsSchema: json.RawMessage(`{"type":"object","properties":{"position":{"type":"string","enum":["front_left","front_right","rear_left","rear_right"]}},"required":["position"],"additionalProperties":false}`),
		},
		{
			Operation:    contract.Operation{Device: "hvac", Tool: "set_climate"},
			MCPTool:      contract.DispatchVehicleCommand,
			ParamsSchema: json.RawMessage(`{"type":"object","properties":{"power":{"type":"string","enum":["on","off"]},"temperature_c":{"type":"number","minimum":16,"maximum":32}},"required":["power","temperature_c"],"additionalProperties":false}`),
		},
		{
			Operation:    contract.Operation{Device: "hvac", Tool: "set_temperature"},
			MCPTool:      contract.DispatchVehicleCommand,
			ParamsSchema: json.RawMessage(`{"type":"object","properties":{"temperature_c":{"type":"number","minimum":16,"maximum":32}},"required":["temperature_c"],"additionalProperties":false}`),
		},
		{
			Operation:    contract.Operation{Device: "window", Tool: "get_status"},
			MCPTool:      contract.GetVehicleStatus,
			ParamsSchema: json.RawMessage(`{"type":"object","properties":{"positions":{"type":"array","items":{"type":"string","enum":["front_left","front_right","rear_left","rear_right"]},"minItems":1}},"required":["positions"],"additionalProperties":false}`),
		},
	}
}

func mcpToolDefinitions() []map[string]any {
	mutationSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"device":  map[string]any{"type": "string"},
			"tool":    map[string]any{"type": "string"},
			"task_id": map[string]any{"type": "string"},
			"params":  map[string]any{"type": "object"},
		},
		"required":             []string{"device", "tool", "params"},
		"additionalProperties": false,
	}
	return []map[string]any{
		{
			"name":        contract.CreateVehicleTask,
			"description": "Create or partially update a pending_reply vehicle task when required parameters are missing.",
			"inputSchema": mutationSchema,
			"annotations": map[string]any{"readOnlyHint": false},
		},
		{
			"name":        contract.ListVehicleTasks,
			"description": "List active vehicle tasks in the current authenticated session and vehicle scope.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"cursor": map[string]any{"type": "string"},
				},
				"additionalProperties": false,
			},
			"annotations": map[string]any{"readOnlyHint": true},
		},
		{
			"name":        contract.DispatchVehicleCommand,
			"description": "Dispatch a vehicle command once required parameters are complete. Include task_id only when resuming a matching pending_reply candidate.",
			"inputSchema": mutationSchema,
			"annotations": map[string]any{"readOnlyHint": false},
		},
		{
			"name":        contract.GetVehicleStatus,
			"description": "Read current vehicle telemetry status without creating any task.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"device": map[string]any{"type": "string"},
					"tool":   map[string]any{"type": "string"},
					"params": map[string]any{"type": "object"},
				},
				"required":             []string{"device", "tool", "params"},
				"additionalProperties": false,
			},
			"annotations": map[string]any{"readOnlyHint": true},
		},
		{
			"name":        contract.SearchVehicleManual,
			"description": "Search the vehicle manual for feature usage or settings.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":    map[string]any{"type": "string"},
					"topic_id": map[string]any{"type": "string"},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
			"annotations": map[string]any{"readOnlyHint": true},
		},
		{
			"name":        contract.SearchVehicleCapabilities,
			"description": "Search available vehicle capabilities and parameter schemas.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
			"annotations": map[string]any{"readOnlyHint": true},
		},
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

type runStartResponse struct {
	RunID    string `json:"run_id"`
	Status   string `json:"status"`
	Replayed bool   `json:"replayed"`
}

type sseEvent struct {
	ID   string
	Data map[string]any
	Raw  string
}

type probeReport struct {
	HermesCommit        string                `json:"hermes_commit"`
	Model               string                `json:"model"`
	MCPProtocolVersion  string                `json:"mcp_protocol_version"`
	Checks              map[string]bool       `json:"checks"`
	ScenarioARun1Output string                `json:"scenario_a_run1_output"`
	ScenarioARun2Output string                `json:"scenario_a_run2_output"`
	ScenarioBOutput     string                `json:"scenario_b_output"`
	ObservedToolNames   []string              `json:"observed_tool_names"`
	RedactedTraces      []map[string]any      `json:"redacted_traces"`
	Observations        []toolCallObservation `json:"observations"`
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func readEnvValue(envPath, key string) string {
	data, err := os.ReadFile(envPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func startRun(
	ctx context.Context,
	client *http.Client,
	baseURL, apiKey, idempotencyKey, turnHandle, hermesSessionID string,
	body map[string]any,
) (int, http.Header, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/runs", bytes.NewReader(raw))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if turnHandle != "" {
		req.Header.Set(contract.TurnHandleHeader, turnHandle)
	}
	if hermesSessionID != "" {
		req.Header.Set(contract.HermesSessionHeader, hermesSessionID)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, respBody, err
}

func streamRunEvents(
	ctx context.Context,
	client *http.Client,
	baseURL, apiKey, runID, lastEventID string,
	maxEvents int,
) ([]sseEvent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/runs/"+runID+"/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET events status %d: %s", resp.StatusCode, string(b))
	}

	var events []sseEvent
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var curID string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id:") {
			curID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var parsed map[string]any
			_ = json.Unmarshal([]byte(raw), &parsed)
			events = append(events, sseEvent{ID: curID, Data: parsed, Raw: raw})
			curID = ""
			if maxEvents > 0 && len(events) >= maxEvents {
				break
			}
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return events, err
	}
	return events, nil
}

func pollRunStatus(ctx context.Context, client *http.Client, baseURL, apiKey, runID string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/runs/"+runID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET run status %d: %s", resp.StatusCode, string(b))
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}

func findRunOutput(events []sseEvent) (string, []string) {
	var output string
	var tools []string
	seen := map[string]bool{}
	for _, ev := range events {
		name, _ := ev.Data["event"].(string)
		if name == "tool.started" || name == "tool.completed" {
			if tn, ok := ev.Data["tool"].(string); ok && tn != "" && !seen[tn] {
				seen[tn] = true
				tools = append(tools, tn)
			}
			if tn, ok := ev.Data["name"].(string); ok && tn != "" && !seen[tn] {
				seen[tn] = true
				tools = append(tools, tn)
			}
		}
		if name == "run.completed" {
			if out, ok := ev.Data["output"].(string); ok {
				output = out
			}
		}
	}
	return output, tools
}

func buildInstructions(promptText string, pending []contract.PendingCandidate) string {
	ctxObj := map[string]any{
		"candidate_tools": defaultCapabilities(),
		"pending_tasks":   pending,
	}
	raw, _ := json.Marshal(ctxObj)
	return promptText + "\n\n当前轮次上下文 JSON：\n" + string(raw)
}

func main() {
	var (
		hermesPython   = flag.String("hermes-python", "/Users/kirin/Documents/ChatGPT/personal_agent/.runtime/hermes-home/installs/b59ccf3b57118ba2/environments/caa4495d4a1446e8821e88cb9961e91b/venv/bin/python", "Path to Hermes venv python")
		hermesAgentDir = flag.String("hermes-agent-dir", "/Users/kirin/Documents/ChatGPT/personal_agent/.runtime/hermes-agent", "Path to pinned Hermes agent repo")
		envSource      = flag.String("env-source", "/Users/kirin/Documents/ChatGPT/personal_agent/.runtime/hermes-home/.env", "Path to .env containing DASHSCOPE_API_KEY")
		outReport      = flag.String("out", "", "Optional output path for JSON probe report")
	)
	flag.Parse()

	if err := runLiveProbe(*hermesPython, *hermesAgentDir, *envSource, *outReport); err != nil {
		fmt.Fprintf(os.Stderr, "H01 probe failed: %v\n", err)
		os.Exit(1)
	}
}

func runLiveProbe(hermesPython, hermesAgentDir, envSource, outReport string) error {
	dashscopeKey := os.Getenv("DASHSCOPE_API_KEY")
	if dashscopeKey == "" {
		dashscopeKey = readEnvValue(envSource, "DASHSCOPE_API_KEY")
	}
	if dashscopeKey == "" {
		return errors.New("DASHSCOPE_API_KEY not found in env or env-source")
	}

	repoRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	promptBytes, err := os.ReadFile(filepath.Join(repoRoot, "hermes", "prompts", "vehicle_system_prompt.md"))
	if err != nil {
		return fmt.Errorf("read prompt: %w", err)
	}
	promptText := string(promptBytes)

	mcpServiceToken := "probe-mcp-svc-token-01"
	apiServerKey := "probe-hermes-api-key-01"

	// 1. Start Go Streamable HTTP MCP probe server.
	mcpSrv := newProbeMCPServer(mcpServiceToken)
	mcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer mcpListener.Close()
	mcpURL := fmt.Sprintf("http://%s/mcp", mcpListener.Addr().String())
	httpServer := &http.Server{Handler: mcpSrv}
	go func() { _ = httpServer.Serve(mcpListener) }()
	defer httpServer.Shutdown(context.Background())

	// 2. Prepare isolated HERMES_HOME and start real Hermes gateway via hermes.gateway_launcher.
	hermesPort, err := freePort()
	if err != nil {
		return err
	}
	hermesHome, err := os.MkdirTemp("", "hermes-h01-probe-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(hermesHome)

	cfg := map[string]any{
		"model": map[string]any{
			"default":        "deepseek-v4.1-flash-1017",
			"provider":       "custom:vehicle-bailian",
			"context_length": 65536,
		},
		"providers": map[string]any{
			"vehicle-bailian": map[string]any{
				"api":             "https://dashscope.aliyuncs.com/apps/anthropic",
				"key_env":         "DASHSCOPE_API_KEY",
				"transport":       "anthropic_messages",
				"default_model":   "deepseek-v4.1-flash-1017",
				"context_length":  65536,
				"discover_models": false,
			},
		},
		"auth":    map[string]any{"adopt_external_logins": false},
		"updates": map[string]any{"check": false},
		"memory":  map[string]any{"memory_enabled": false, "user_profile_enabled": false},
		"agent":   map[string]any{"max_turns": 6},
		"tools":   map[string]any{"tool_search": map[string]any{"enabled": "off"}},
		"toolsets": []string{"mcp-vehicle"},
		"platform_toolsets": map[string]any{
			"api_server": []string{"mcp-vehicle"},
		},
		"mcp_servers": map[string]any{
			"vehicle": map[string]any{
				"url": mcpURL,
				"headers": map[string]any{
					"Authorization": "Bearer " + mcpServiceToken,
				},
				"strict_redirect_headers": true,
				"timeout":                 15,
				"connect_timeout":         10,
				"trust":                   "full",
				"sampling":                map[string]any{"enabled": false},
			},
		},
		"gateway": map[string]any{
			"api_server": map[string]any{
				"enabled":             true,
				"host":                "127.0.0.1",
				"port":                hermesPort,
				"max_concurrent_runs": 6,
			},
		},
	}
	cfgBytes, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(hermesHome, "config.yaml"), cfgBytes, 0o600); err != nil {
		return err
	}
	envContent := fmt.Sprintf("DASHSCOPE_API_KEY=%s\nAPI_SERVER_KEY=%s\nAPI_SERVER_ENABLED=true\n", dashscopeKey, apiServerKey)
	if err := os.WriteFile(filepath.Join(hermesHome, ".env"), []byte(envContent), 0o600); err != nil {
		return err
	}

	gatewayCtx, gatewayCancel := context.WithCancel(context.Background())
	defer gatewayCancel()

	cmd := exec.CommandContext(gatewayCtx, hermesPython, "-m", "hermes.gateway_launcher")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"HERMES_HOME="+hermesHome,
		"HERMES_AGENT_DIR="+hermesAgentDir,
		"PYTHONPATH="+hermesAgentDir+":"+repoRoot,
		"DASHSCOPE_API_KEY="+dashscopeKey,
		"API_SERVER_KEY="+apiServerKey,
		"API_SERVER_ENABLED=true",
		"PYTHONUNBUFFERED=1",
	)
	var gatewayLogs bytes.Buffer
	cmd.Stdout = &gatewayLogs
	cmd.Stderr = &gatewayLogs
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start hermes gateway: %w", err)
	}
	defer func() {
		gatewayCancel()
		_ = cmd.Wait()
	}()

	hermesBase := fmt.Sprintf("http://127.0.0.1:%d", hermesPort)
	client := &http.Client{Timeout: 90 * time.Second}

	// Wait until Hermes API server is ready.
	ready := false
	for i := 0; i < 60; i++ {
		req, _ := http.NewRequest(http.MethodGet, hermesBase+"/health", nil)
		req.Header.Set("Authorization", "Bearer "+apiServerKey)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ready {
		return fmt.Errorf("hermes gateway did not become ready:\n%s", gatewayLogs.String())
	}
	// Give MCP discovery a brief moment to complete if async.
	time.Sleep(1500 * time.Millisecond)

	report := probeReport{
		HermesCommit:       "72c88037",
		Model:              "deepseek-v4.1-flash-1017",
		MCPProtocolVersion: mcpProtocolVersion,
		Checks:             map[string]bool{},
	}
	ctx := context.Background()
	now := time.Now().UTC()

	// --- Check 1: Scenario A Run 1 ("把车窗关一下") + Idempotency-Key replay & 409 conflict + SSE resubscribe ---
	scopeA := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL1"}
	hermesSessionA := "hermes-U1-S1-VCL1"
	handleA1 := "TH-A1-0001"
	mcpSrv.RegisterTurn(contract.TurnContext{
		VehicleTurn: contract.VehicleTurn{
			Scope:           scopeA,
			VoiceTurnID:     "V1",
			RequestID:       "R1",
			FinalTranscript: contract.FinalTranscript{ProviderSessionID: "Q1", InputItemID: "I1", Text: "把车窗关一下"},
		},
		HermesSessionID: hermesSessionA,
		TurnHandle:      handleA1,
		ExpiresAt:       now.Add(5 * time.Minute),
	})
	bodyA1 := map[string]any{
		"session_id":   hermesSessionA,
		"input":        "把车窗关一下",
		"instructions": buildInstructions(promptText, []contract.PendingCandidate{}),
	}
	status, _, respBody, err := startRun(ctx, client, hermesBase, apiServerKey, "idem-A1", handleA1, hermesSessionA, bodyA1)
	if err != nil || status != http.StatusAccepted {
		return fmt.Errorf("scenario A1 start failed: status=%d err=%v body=%s", status, err, string(respBody))
	}
	var startA1 runStartResponse
	if err := json.Unmarshal(respBody, &startA1); err != nil {
		return err
	}

	// Verify Idempotency-Key replay returns same run_id and Idempotency-Replayed: true.
	repStatus, repHdr, repBody, err := startRun(ctx, client, hermesBase, apiServerKey, "idem-A1", handleA1, hermesSessionA, bodyA1)
	if err != nil || repStatus != http.StatusAccepted {
		return fmt.Errorf("idempotency replay failed: status=%d err=%v body=%s", repStatus, err, string(repBody))
	}
	var replayA1 runStartResponse
	_ = json.Unmarshal(repBody, &replayA1)
	report.Checks["idempotency_replay"] = replayA1.RunID == startA1.RunID && strings.EqualFold(repHdr.Get("Idempotency-Replayed"), "true")

	// Verify Idempotency-Key with different payload returns 409 conflict.
	conflictBody := map[string]any{"session_id": hermesSessionA, "input": "打开天窗"}
	confStatus, _, confBody, err := startRun(ctx, client, hermesBase, apiServerKey, "idem-A1", handleA1, hermesSessionA, conflictBody)
	if err != nil {
		return err
	}
	report.Checks["idempotency_conflict_409"] = confStatus == http.StatusConflict && strings.Contains(string(confBody), "idempotency_key_conflict")

	// Stream first 1 event, disconnect, then resubscribe with Last-Event-ID to verify replay/resubscribe!
	partialEvents, err := streamRunEvents(ctx, client, hermesBase, apiServerKey, startA1.RunID, "", 1)
	if err != nil {
		return fmt.Errorf("scenario A1 initial SSE: %w", err)
	}
	lastID := ""
	if len(partialEvents) > 0 {
		lastID = partialEvents[len(partialEvents)-1].ID
	}
	restEvents, err := streamRunEvents(ctx, client, hermesBase, apiServerKey, startA1.RunID, lastID, 0)
	if err != nil {
		return fmt.Errorf("scenario A1 resubscribe SSE: %w", err)
	}
	allA1Events := append(partialEvents, restEvents...)
	outA1, toolsA1 := findRunOutput(allA1Events)
	report.ScenarioARun1Output = outA1
	report.ObservedToolNames = append(report.ObservedToolNames, toolsA1...)
	report.Checks["sse_disconnect_resubscribe"] = len(partialEvents) >= 1 && outA1 != ""

	// Verify GET /v1/runs/{run_id} terminal polling.
	polledA1, err := pollRunStatus(ctx, client, hermesBase, apiServerKey, startA1.RunID)
	if err != nil {
		return fmt.Errorf("poll A1: %w", err)
	}
	report.Checks["terminal_polling"] = polledA1["status"] == "completed" && polledA1["output"] == outA1

	obsA1 := mcpSrv.Observations()
	createdTaskID := ""
	for _, o := range obsA1 {
		if o.ToolName == contract.CreateVehicleTask && !o.IsError && o.HeaderTurnHandle == handleA1 && o.MetaTurnHandle == handleA1 {
			if tr, ok := o.ResultPayload.(contract.TaskResult); ok {
				createdTaskID = tr.TaskID
			}
		}
	}
	report.Checks["scenario_a_run1_create_task"] = createdTaskID != ""
	if createdTaskID == "" {
		return fmt.Errorf("scenario A1 did not create task via trusted handle; obs=%+v output=%q", obsA1, outA1)
	}

	// --- Check 2: Scenario A Run 2 ("左后窗") resumes createdTaskID with a fresh turn_handle ---
	handleA2 := "TH-A2-0002"
	createdTask := mcpSrv.tasks[createdTaskID]
	pendingA2 := []contract.PendingCandidate{{
		Operation:     createdTask.Operation,
		TaskID:        createdTask.TaskID,
		Version:       createdTask.Version,
		CreatedAt:     createdTask.CreatedAt,
		ExpiresAt:     createdTask.ExpiresAt,
		KnownParams:   createdTask.KnownParams,
		MissingFields: createdTask.MissingFields,
		Question:      createdTask.Question,
		ParamsSchema:  defaultCapabilities()[0].ParamsSchema,
	}}
	mcpSrv.RegisterTurn(contract.TurnContext{
		VehicleTurn: contract.VehicleTurn{
			Scope:           scopeA,
			VoiceTurnID:     "V2",
			RequestID:       "R2",
			FinalTranscript: contract.FinalTranscript{ProviderSessionID: "Q1", InputItemID: "I2", Text: "左后窗"},
		},
		HermesSessionID: hermesSessionA,
		TurnHandle:      handleA2,
		Candidates:      []contract.CandidateVersion{{TaskID: createdTaskID, Version: createdTask.Version}},
		ExpiresAt:       now.Add(5 * time.Minute),
	})
	status, _, respBody, err = startRun(ctx, client, hermesBase, apiServerKey, "idem-A2", handleA2, hermesSessionA, map[string]any{
		"session_id":   hermesSessionA,
		"input":        "左后窗",
		"instructions": buildInstructions(promptText, pendingA2),
	})
	if err != nil || status != http.StatusAccepted {
		return fmt.Errorf("scenario A2 start failed: status=%d err=%v body=%s", status, err, string(respBody))
	}
	var startA2 runStartResponse
	_ = json.Unmarshal(respBody, &startA2)
	eventsA2, err := streamRunEvents(ctx, client, hermesBase, apiServerKey, startA2.RunID, "", 0)
	if err != nil {
		return err
	}
	outA2, toolsA2 := findRunOutput(eventsA2)
	report.ScenarioARun2Output = outA2
	report.ObservedToolNames = append(report.ObservedToolNames, toolsA2...)

	dispatchedA2 := false
	for _, o := range mcpSrv.Observations() {
		if o.HeaderTurnHandle == handleA2 && o.MetaTurnHandle == handleA2 &&
			o.ToolName == contract.DispatchVehicleCommand && !o.IsError {
			tid, _ := o.Arguments["task_id"].(string)
			p, _ := o.Arguments["params"].(map[string]any)
			pos, _ := p["position"].(string)
			if tid == createdTaskID && pos == "rear_left" {
				dispatchedA2 = true
			}
		}
	}
	report.Checks["scenario_a_run2_resume_dispatch"] = dispatchedA2

	// --- Check 3: Scenario B (T102 hvac newer + T101 window older; user says "左后窗") ---
	scopeB := contract.Scope{UserID: "U1", SessionID: "S_B", VehicleID: "VCL1"}
	hermesSessionB := "hermes-U1-SB-VCL1"
	t101 := &probeTask{
		TaskID: "T201", Scope: scopeB, Operation: contract.Operation{Device: "window", Tool: "close"},
		KnownParams: map[string]any{}, MissingFields: []string{"position"}, Question: "要关哪扇车窗？",
		Status: contract.PendingReply, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), Version: 1,
	}
	t102 := &probeTask{
		TaskID: "T202", Scope: scopeB, Operation: contract.Operation{Device: "hvac", Tool: "set_climate"},
		KnownParams: map[string]any{"power": "on"}, MissingFields: []string{"temperature_c"}, Question: "空调要调到几度？",
		Status: contract.PendingReply, CreatedAt: now.Add(1 * time.Minute), ExpiresAt: now.Add(11 * time.Minute), Version: 1,
	}
	mcpSrv.SeedTask(t101)
	mcpSrv.SeedTask(t102)

	handleB := "TH-B1-0003"
	mcpSrv.RegisterTurn(contract.TurnContext{
		VehicleTurn: contract.VehicleTurn{
			Scope:           scopeB,
			VoiceTurnID:     "V3",
			RequestID:       "R3",
			FinalTranscript: contract.FinalTranscript{ProviderSessionID: "Q1", InputItemID: "I3", Text: "左后窗"},
		},
		HermesSessionID: hermesSessionB,
		TurnHandle:      handleB,
		Candidates: []contract.CandidateVersion{
			{TaskID: t102.TaskID, Version: t102.Version},
			{TaskID: t101.TaskID, Version: t101.Version},
		},
		ExpiresAt: now.Add(5 * time.Minute),
	})
	pendingB := []contract.PendingCandidate{
		{
			Operation: t102.Operation, TaskID: t102.TaskID, Version: t102.Version,
			CreatedAt: t102.CreatedAt, ExpiresAt: t102.ExpiresAt,
			KnownParams: t102.KnownParams, MissingFields: t102.MissingFields,
			Question: t102.Question, ParamsSchema: defaultCapabilities()[1].ParamsSchema,
		},
		{
			Operation: t101.Operation, TaskID: t101.TaskID, Version: t101.Version,
			CreatedAt: t101.CreatedAt, ExpiresAt: t101.ExpiresAt,
			KnownParams: t101.KnownParams, MissingFields: t101.MissingFields,
			Question: t101.Question, ParamsSchema: defaultCapabilities()[0].ParamsSchema,
		},
	}
	status, _, respBody, err = startRun(ctx, client, hermesBase, apiServerKey, "idem-B1", handleB, hermesSessionB, map[string]any{
		"session_id":   hermesSessionB,
		"input":        "左后窗",
		"instructions": buildInstructions(promptText, pendingB),
	})
	if err != nil || status != http.StatusAccepted {
		return fmt.Errorf("scenario B start failed: status=%d err=%v body=%s", status, err, string(respBody))
	}
	var startB runStartResponse
	_ = json.Unmarshal(respBody, &startB)
	eventsB, err := streamRunEvents(ctx, client, hermesBase, apiServerKey, startB.RunID, "", 0)
	if err != nil {
		return err
	}
	outB, _ := findRunOutput(eventsB)
	report.ScenarioBOutput = outB

	scenarioBOk := false
	for _, o := range mcpSrv.Observations() {
		if o.HeaderTurnHandle == handleB && o.ToolName == contract.DispatchVehicleCommand && !o.IsError {
			if tid, _ := o.Arguments["task_id"].(string); tid == t101.TaskID {
				scenarioBOk = true
			}
		}
	}
	report.Checks["scenario_b_dual_candidate_match"] = scenarioBOk && mcpSrv.tasks[t102.TaskID].Status == contract.PendingReply

	// --- Check 4: Parallel two users / two vehicles cross-turn isolation ---
	scopeP1 := contract.Scope{UserID: "U_PAR_1", SessionID: "S_PAR_1", VehicleID: "VCL_PAR_1"}
	scopeP2 := contract.Scope{UserID: "U_PAR_2", SessionID: "S_PAR_2", VehicleID: "VCL_PAR_2"}
	sessP1, sessP2 := "hermes-PAR-1", "hermes-PAR-2"
	handleP1, handleP2 := "TH-PAR-1", "TH-PAR-2"
	mcpSrv.RegisterTurn(contract.TurnContext{
		VehicleTurn: contract.VehicleTurn{
			Scope:           scopeP1,
			VoiceTurnID:     "VP1",
			RequestID:       "RP1",
			FinalTranscript: contract.FinalTranscript{ProviderSessionID: "QP1", InputItemID: "IP1", Text: "把左后窗关上"},
		},
		HermesSessionID: sessP1,
		TurnHandle:      handleP1,
		ExpiresAt:       now.Add(5 * time.Minute),
	})
	mcpSrv.RegisterTurn(contract.TurnContext{
		VehicleTurn: contract.VehicleTurn{
			Scope:           scopeP2,
			VoiceTurnID:     "VP2",
			RequestID:       "RP2",
			FinalTranscript: contract.FinalTranscript{ProviderSessionID: "QP2", InputItemID: "IP2", Text: "把空调打开调到25度"},
		},
		HermesSessionID: sessP2,
		TurnHandle:      handleP2,
		ExpiresAt:       now.Add(5 * time.Minute),
	})

	var wg sync.WaitGroup
	var parErr1, parErr2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		st, _, rb, e := startRun(ctx, client, hermesBase, apiServerKey, "idem-P1", handleP1, sessP1, map[string]any{
			"session_id":   sessP1,
			"input":        "把左后窗关上",
			"instructions": buildInstructions(promptText, []contract.PendingCandidate{}),
		})
		if e != nil || st != http.StatusAccepted {
			parErr1 = fmt.Errorf("par1 start: %d %v %s", st, e, string(rb))
			return
		}
		var sr runStartResponse
		_ = json.Unmarshal(rb, &sr)
		_, parErr1 = streamRunEvents(ctx, client, hermesBase, apiServerKey, sr.RunID, "", 0)
	}()
	go func() {
		defer wg.Done()
		st, _, rb, e := startRun(ctx, client, hermesBase, apiServerKey, "idem-P2", handleP2, sessP2, map[string]any{
			"session_id":   sessP2,
			"input":        "把空调打开调到25度",
			"instructions": buildInstructions(promptText, []contract.PendingCandidate{}),
		})
		if e != nil || st != http.StatusAccepted {
			parErr2 = fmt.Errorf("par2 start: %d %v %s", st, e, string(rb))
			return
		}
		var sr runStartResponse
		_ = json.Unmarshal(rb, &sr)
		_, parErr2 = streamRunEvents(ctx, client, hermesBase, apiServerKey, sr.RunID, "", 0)
	}()
	wg.Wait()
	if parErr1 != nil || parErr2 != nil {
		return fmt.Errorf("parallel runs failed: err1=%v err2=%v", parErr1, parErr2)
	}

	p1Matched, p2Matched, crossLeak := false, false, false
	for _, o := range mcpSrv.Observations() {
		switch o.HeaderTurnHandle {
		case handleP1:
			if o.HeaderHermesSessionID != sessP1 || o.MetaTurnHandle != handleP1 || o.ResolvedScope != scopeP1 {
				crossLeak = true
			}
			if dev, _ := o.Arguments["device"].(string); dev == "window" && !o.IsError {
				p1Matched = true
			}
			if dev, _ := o.Arguments["device"].(string); dev == "hvac" {
				crossLeak = true
			}
		case handleP2:
			if o.HeaderHermesSessionID != sessP2 || o.MetaTurnHandle != handleP2 || o.ResolvedScope != scopeP2 {
				crossLeak = true
			}
			if dev, _ := o.Arguments["device"].(string); dev == "hvac" && !o.IsError {
				p2Matched = true
			}
			if dev, _ := o.Arguments["device"].(string); dev == "window" {
				crossLeak = true
			}
		}
	}
	report.Checks["parallel_multi_user_isolation"] = p1Matched && p2Matched && !crossLeak

	// --- Check 5: Cancelled / expired turn late MCP call rejected ---
	scopeCancel := contract.Scope{UserID: "U_C", SessionID: "S_C", VehicleID: "VCL_C"}
	sessCancel := "hermes-CANCEL-1"
	handleCancel := "TH-CANCEL-1"
	mcpSrv.RegisterTurn(contract.TurnContext{
		VehicleTurn: contract.VehicleTurn{
			Scope:           scopeCancel,
			VoiceTurnID:     "VC1",
			RequestID:       "RC1",
			FinalTranscript: contract.FinalTranscript{ProviderSessionID: "QC1", InputItemID: "IC1", Text: "把左后窗关上"},
		},
		HermesSessionID: sessCancel,
		TurnHandle:      handleCancel,
		ExpiresAt:       now.Add(5 * time.Minute),
	})
	// Cancel immediately before model calls MCP -> late call must be rejected with TURN_CONTEXT_INVALID.
	mcpSrv.CancelTurn(handleCancel)
	status, _, respBody, err = startRun(ctx, client, hermesBase, apiServerKey, "idem-C1", handleCancel, sessCancel, map[string]any{
		"session_id":   sessCancel,
		"input":        "把左后窗关上",
		"instructions": buildInstructions(promptText, []contract.PendingCandidate{}),
	})
	if err != nil || status != http.StatusAccepted {
		return fmt.Errorf("cancelled turn start failed: %d %v %s", status, err, string(respBody))
	}
	var startC runStartResponse
	_ = json.Unmarshal(respBody, &startC)
	_, err = streamRunEvents(ctx, client, hermesBase, apiServerKey, startC.RunID, "", 0)
	if err != nil {
		return err
	}
	cancelledRejected := false
	for _, o := range mcpSrv.Observations() {
		if o.HeaderTurnHandle == handleCancel && o.IsError {
			if be, ok := o.ResultPayload.(contract.BusinessError); ok && be.Code == contract.TurnContextInvalid {
				cancelledRejected = true
			}
		}
	}
	report.Checks["cancelled_late_call_rejected"] = cancelledRejected

	// --- Check 6: Missing turn handle (simulating stock Hermes without adapter) rejected ---
	status, _, respBody, err = startRun(ctx, client, hermesBase, apiServerKey, "idem-NOHDR", "", "hermes-NOHDR", map[string]any{
		"session_id":   "hermes-NOHDR",
		"input":        "把左后窗关上",
		"instructions": buildInstructions(promptText, []contract.PendingCandidate{}),
	})
	if err != nil || status != http.StatusAccepted {
		return fmt.Errorf("no-handle turn start failed: %d %v %s", status, err, string(respBody))
	}
	var startNoHdr runStartResponse
	_ = json.Unmarshal(respBody, &startNoHdr)
	_, err = streamRunEvents(ctx, client, hermesBase, apiServerKey, startNoHdr.RunID, "", 0)
	if err != nil {
		return err
	}
	missingRejected := false
	for _, o := range mcpSrv.Observations() {
		if o.HeaderTurnHandle == "" && o.IsError {
			if be, ok := o.ResultPayload.(contract.BusinessError); ok && be.Code == contract.TurnContextInvalid {
				missingRejected = true
			}
		}
	}
	report.Checks["missing_handle_rejected"] = missingRejected

	// Build redacted observations & traces.
	for _, o := range mcpSrv.Observations() {
		redacted := o
		if redacted.HeaderTurnHandle != "" {
			redacted.HeaderTurnHandle = redactHandle(redacted.HeaderTurnHandle)
		}
		if redacted.MetaTurnHandle != "" {
			redacted.MetaTurnHandle = redactHandle(redacted.MetaTurnHandle)
		}
		report.Observations = append(report.Observations, redacted)
	}
	report.RedactedTraces = []map[string]any{
		{
			"scenario": "A1",
			"run_id":   startA1.RunID,
			"events":   len(allA1Events),
			"output":   outA1,
		},
		{
			"scenario": "A2",
			"run_id":   startA2.RunID,
			"events":   len(eventsA2),
			"output":   outA2,
		},
		{
			"scenario": "B",
			"run_id":   startB.RunID,
			"events":   len(eventsB),
			"output":   outB,
		},
	}

	outBytes, _ := json.MarshalIndent(report, "", "  ")
	if outReport != "" {
		if err := os.WriteFile(outReport, outBytes, 0o644); err != nil {
			return err
		}
	}
	fmt.Println(string(outBytes))

	var failed []string
	for k, ok := range report.Checks {
		if !ok {
			failed = append(failed, k)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed H01 checks: %s", strings.Join(failed, ", "))
	}
	return nil
}

func redactHandle(h string) string {
	if len(h) <= 6 {
		return "***"
	}
	return h[:6] + "***"
}
