// Package mcp implements the Go Streamable HTTP MCP server exposing the six
// vehicle tools with service auth, trusted turn-metadata verification,
// cancellation-serialized write transactions, and Go-side write intent guards (H04).
package mcp

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/catalog"
	"github.com/kirin7002/app_personal_agent/vehicle/tasks"
	"github.com/kirin7002/app_personal_agent/vehicle/turns"
)

const (
	MCPProtocolVersion   = "2025-11-25"
	MetaTurnHandleKey    = "vehicle.turn_handle"
	MetaHermesSessionKey = "vehicle.hermes_session_id"
)

var forbiddenIdentityArgs = map[string]bool{
	"turn_handle":       true,
	"hermes_session_id": true,
	"user_id":           true,
	"session_id":        true,
	"vehicle_id":        true,
	"scope":             true,
	"request_id":        true,
	"_meta":             true,
}

// ToolAuditEvent is a sanitized read-only observation of a single MCP tools/call execution.
type ToolAuditEvent struct {
	CallID       string
	Scope        contract.Scope
	TurnID       string
	RequestID    string
	ToolName     string
	IsWrite      bool
	Status       string // "running", "done", "error"
	StartedAt    time.Time
	FinishedAt   *time.Time
	DurationMs   *int64
	Arguments    map[string]any
	Output       any
	IsError      bool
	ErrorCode    contract.ErrorCode
	ErrorMessage string
	TaskID       string
	CommandID    string
	WriteStatus  contract.Status
}

// Config configures the H04 Go MCP HTTP server.
type Config struct {
	// ServiceToken is required in Authorization: Bearer <token> when non-empty.
	ServiceToken string
	// RequireMeta mandates that MCP params._meta also carries vehicle.turn_handle
	// and vehicle.hermes_session_id matching the HTTP headers. When false, headers
	// are still mandatory and any _meta keys present must still strictly match headers.
	RequireMeta bool
	// Turns resolves read turns (and locks write turns in memory/SQLite/MySQL mode).
	Turns contract.TurnRegistry
	// LockTurn optionally overrides the transaction-scoped turn lock used during
	// SQL writes. When nil, Server automatically uses Turns' own LockWritableTurn
	// method (e.g., *turns.SQLiteStore) and falls back to turns.LockWritableTurn (MySQL).
	LockTurn func(ctx context.Context, tx *sql.Tx, meta contract.TurnMetadata, now time.Time) (contract.TurnContext, error)
	// Tasks stores vehicle tasks, turn write idempotency, and command outbox (H03).
	Tasks tasks.Store
	// Catalog provides schemas, BM25 search, telemetry status, and manual retrieval (H06).
	Catalog *catalog.Service
	// Now overrides clock for deterministic testing.
	Now func() time.Time
	// OnToolCall is an optional read-only observer notified at the start and end
	// of every tools/call invocation with sanitized arguments and structured output.
	OnToolCall func(ToolAuditEvent)
}

type Server struct {
	cfg    Config
	callID atomic.Uint64
}

func NewServer(cfg Config) *Server {
	return &Server{cfg: cfg}
}

func (s *Server) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now().UTC()
	}
	return time.Now().UTC()
}

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

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ServiceToken != "" {
		scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") ||
			subtle.ConstantTimeCompare([]byte(strings.TrimSpace(tok)), []byte(s.cfg.ServiceToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
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
	if len(req.ID) == 0 || string(req.ID) == "null" || strings.HasPrefix(req.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("MCP-Protocol-Version", MCPProtocolVersion)
	switch req.Method {
	case "initialize":
		writeJSON(w, http.StatusOK, jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": MCPProtocolVersion,
				"capabilities": map[string]any{
					"tools": map[string]any{"listChanged": false},
				},
				"serverInfo": map[string]any{
					"name":    "vehicle-control-mcp",
					"version": "1.0.0",
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
			Result:  map[string]any{"tools": ToolDefinitions()},
		})
	case "tools/call":
		res := s.handleToolCall(r.Context(), r.Header, req.Params)
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

func (s *Server) handleToolCall(ctx context.Context, hdr http.Header, rawParams json.RawMessage) map[string]any {
	startedAt := time.Now().UTC()
	var params callToolParams
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return errorResult(&contract.BusinessError{
			Code:    contract.InvalidArgument,
			Message: "invalid tools/call parameters",
		})
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}

	var auditBase ToolAuditEvent
	emitAudit := s.cfg.OnToolCall != nil
	if emitAudit {
		seq := s.callID.Add(1)
		auditBase = ToolAuditEvent{
			CallID:    "mcp:" + params.Name + ":" + strconv.FormatUint(seq, 10),
			ToolName:  params.Name,
			IsWrite:   params.Name == contract.CreateVehicleTask || params.Name == contract.DispatchVehicleCommand,
			StartedAt: startedAt,
			Arguments: sanitizeAuditMap(params.Arguments),
		}
	}

	finishAudit := func(res map[string]any) map[string]any {
		if !emitAudit {
			return res
		}
		end := time.Now().UTC()
		dur := end.Sub(startedAt).Milliseconds()
		ev := auditBase
		ev.FinishedAt = &end
		ev.DurationMs = &dur
		ev.Status = "done"
		if isErr, _ := res["isError"].(bool); isErr {
			ev.IsError = true
			ev.Status = "error"
		}
		if sc, ok := res["structuredContent"]; ok {
			ev.Output = sanitizeAuditValue(sc)
			switch v := sc.(type) {
			case contract.BusinessError:
				ev.ErrorCode = v.Code
				ev.ErrorMessage = v.Message
			case *contract.BusinessError:
				if v != nil {
					ev.ErrorCode = v.Code
					ev.ErrorMessage = v.Message
				}
			case contract.TaskResult:
				ev.TaskID = v.TaskID
				ev.CommandID = v.CommandID
				ev.WriteStatus = v.Status
			}
		}
		s.cfg.OnToolCall(ev)
		return res
	}

	// Reject any untrusted identity/turn field smuggled in model tool arguments.
	for k := range params.Arguments {
		if forbiddenIdentityArgs[k] {
			return finishAudit(errorResult(&contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: "forbidden identity argument: " + k,
			}))
		}
	}

	meta, err := s.extractTrustedMetadata(hdr, params.Meta)
	if err != nil {
		return finishAudit(errorResult(err))
	}
	if s.cfg.Turns == nil || s.cfg.Tasks == nil || s.cfg.Catalog == nil {
		return finishAudit(errorResult(&contract.BusinessError{
			Code:      contract.CapabilityUnavailable,
			Message:   "vehicle MCP dependencies not configured",
			Retryable: true,
		}))
	}

	now := s.now()
	if emitAudit {
		if turn, resolveErr := s.cfg.Turns.ResolveTurn(ctx, meta, now); resolveErr == nil {
			auditBase.Scope = turn.Scope
			auditBase.TurnID = turn.VoiceTurnID
			auditBase.RequestID = turn.RequestID
		}
		runningEv := auditBase
		runningEv.Status = "running"
		s.cfg.OnToolCall(runningEv)
	}

	switch params.Name {
	case contract.CreateVehicleTask:
		return finishAudit(s.callWriteTool(ctx, meta, params.Arguments, false, now))
	case contract.DispatchVehicleCommand:
		return finishAudit(s.callWriteTool(ctx, meta, params.Arguments, true, now))
	case contract.ListVehicleTasks:
		return finishAudit(s.callListTasks(ctx, meta, params.Arguments, now))
	case contract.GetVehicleStatus:
		return finishAudit(s.callGetStatus(ctx, meta, params.Arguments, now))
	case contract.SearchVehicleManual:
		return finishAudit(s.callSearchManual(ctx, meta, params.Arguments, now))
	case contract.SearchVehicleCapabilities:
		return finishAudit(s.callSearchCapabilities(ctx, meta, params.Arguments, now))
	default:
		return finishAudit(errorResult(&contract.BusinessError{
			Code:    contract.CapabilityUnavailable,
			Message: "unknown MCP tool: " + params.Name,
		}))
	}
}

func isSensitiveAuditKey(k string) bool {
	if forbiddenIdentityArgs[k] {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(k))
	for _, needle := range []string{"turn_handle", "authorization", "token", "api_key", "apikey", "secret", "password", "cookie", "dsn"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func sanitizeAuditMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if isSensitiveAuditKey(k) {
			continue
		}
		out[k] = sanitizeAuditValue(v)
	}
	return out
}

func sanitizeAuditValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return sanitizeAuditMap(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = sanitizeAuditValue(item)
		}
		return out
	default:
		return v
	}
}

func (s *Server) extractTrustedMetadata(hdr http.Header, mcpMeta map[string]any) (contract.TurnMetadata, error) {
	handle := strings.TrimSpace(hdr.Get(contract.TurnHandleHeader))
	sessionID := strings.TrimSpace(hdr.Get(contract.HermesSessionHeader))
	if handle == "" || sessionID == "" {
		return contract.TurnMetadata{}, &contract.BusinessError{
			Code:    contract.TurnContextInvalid,
			Message: "missing trusted turn headers",
		}
	}

	rawMetaHandle, hasMetaHandle := mcpMeta[MetaTurnHandleKey]
	rawMetaSession, hasMetaSession := mcpMeta[MetaHermesSessionKey]
	if s.cfg.RequireMeta && (!hasMetaHandle || !hasMetaSession) {
		return contract.TurnMetadata{}, &contract.BusinessError{
			Code:    contract.TurnContextInvalid,
			Message: "missing trusted MCP _meta turn binding",
		}
	}
	if hasMetaHandle {
		mh, _ := rawMetaHandle.(string)
		if strings.TrimSpace(mh) != handle {
			return contract.TurnMetadata{}, &contract.BusinessError{
				Code:    contract.TurnContextInvalid,
				Message: "turn handle mismatch between header and _meta",
			}
		}
	}
	if hasMetaSession {
		ms, _ := rawMetaSession.(string)
		if strings.TrimSpace(ms) != sessionID {
			return contract.TurnMetadata{}, &contract.BusinessError{
				Code:    contract.TurnContextInvalid,
				Message: "hermes session ID mismatch between header and _meta",
			}
		}
	}
	return contract.TurnMetadata{
		TurnHandle:      handle,
		HermesSessionID: sessionID,
	}, nil
}

func (s *Server) callWriteTool(
	ctx context.Context,
	meta contract.TurnMetadata,
	args map[string]any,
	isDispatch bool,
	now time.Time,
) map[string]any {
	mut, err := parseTaskMutation(args)
	if err != nil {
		return errorResult(err)
	}

	// Fast pre-check via TurnRegistry (re-checked and row-locked inside write tx below).
	turn, err := s.cfg.Turns.ResolveTurn(ctx, meta, now)
	if err != nil {
		return errorResult(err)
	}

	// Candidate membership & expected version check from this turn's immutable snapshot.
	var expectedVersion uint64
	if mut.TaskID != "" {
		found := false
		for _, cv := range turn.Candidates {
			if cv.TaskID == mut.TaskID {
				expectedVersion = cv.Version
				found = true
				break
			}
		}
		if !found {
			return errorResult(&contract.BusinessError{
				Code:    contract.TaskNotEligible,
				Message: "task_id is not in this turn's candidate snapshot",
			})
		}
	}

	// If this turn already has a persisted write, skip live-candidate intent checks so H03's
	// transactional idempotency check returns either the exact replayed TaskResult (same params)
	// or IDEMPOTENCY_CONFLICT (different params).
	_, progErr := s.cfg.Tasks.ReadTurnProgress(ctx, turn.Scope, turn.RequestID)
	alreadyWrote := progErr == nil
	if !alreadyWrote {
		if err := evaluateWriteIntent(ctx, s.cfg.Tasks, turn, mut, isDispatch); err != nil {
			return errorResult(err)
		}
	}

	type txTurnLocker interface {
		LockWritableTurn(context.Context, *sql.Tx, contract.TurnMetadata, time.Time) (contract.TurnContext, error)
	}
	lockTx := s.cfg.LockTurn
	if lockTx == nil {
		if locker, ok := s.cfg.Turns.(txTurnLocker); ok {
			lockTx = locker.LockWritableTurn
		} else {
			lockTx = turns.LockWritableTurn
		}
	}

	req := tasks.TaskWriteRequest{
		Turn:            turn,
		Metadata:        meta,
		Mutation:        mut,
		ExpectedVersion: expectedVersion,
		Now:             now,
		TxLock: func(txCtx context.Context, tx *sql.Tx, lockNow time.Time) (contract.TurnContext, error) {
			return lockTx(txCtx, tx, meta, lockNow)
		},
		MemLock: func(lockCtx context.Context, lockNow time.Time) (contract.TurnContext, error) {
			return s.cfg.Turns.ResolveTurn(lockCtx, meta, lockNow)
		},
	}

	var res contract.TaskResult
	if isDispatch {
		res, err = s.cfg.Tasks.DispatchCommand(ctx, req)
	} else {
		res, err = s.cfg.Tasks.CreateOrUpdateTask(ctx, req)
	}
	if err != nil {
		return errorResult(err)
	}
	return successResult(res)
}

func (s *Server) callListTasks(ctx context.Context, meta contract.TurnMetadata, args map[string]any, now time.Time) map[string]any {
	turn, err := s.cfg.Turns.ResolveTurn(ctx, meta, now)
	if err != nil {
		return errorResult(err)
	}
	var input contract.ListTasksInput
	if err := decodeStrictArgs(args, &input); err != nil {
		return errorResult(err)
	}
	res, err := s.cfg.Tasks.ListTasks(ctx, turn.Scope, input, now)
	if err != nil {
		return errorResult(err)
	}
	return successResult(res)
}

func (s *Server) callGetStatus(ctx context.Context, meta contract.TurnMetadata, args map[string]any, now time.Time) map[string]any {
	turn, err := s.cfg.Turns.ResolveTurn(ctx, meta, now)
	if err != nil {
		return errorResult(err)
	}
	var input contract.VehicleStatusInput
	if err := decodeStrictArgs(args, &input); err != nil {
		return errorResult(err)
	}
	res, err := s.cfg.Catalog.GetVehicleStatus(ctx, turn.Scope, input, now)
	if err != nil {
		return errorResult(err)
	}
	return successResult(res)
}

func (s *Server) callSearchManual(ctx context.Context, meta contract.TurnMetadata, args map[string]any, now time.Time) map[string]any {
	turn, err := s.cfg.Turns.ResolveTurn(ctx, meta, now)
	if err != nil {
		return errorResult(err)
	}
	var input contract.SearchManualInput
	if err := decodeStrictArgs(args, &input); err != nil {
		return errorResult(err)
	}
	res, err := s.cfg.Catalog.SearchManual(ctx, turn.Scope, input)
	if err != nil {
		return errorResult(err)
	}
	return successResult(res)
}

func (s *Server) callSearchCapabilities(ctx context.Context, meta contract.TurnMetadata, args map[string]any, now time.Time) map[string]any {
	turn, err := s.cfg.Turns.ResolveTurn(ctx, meta, now)
	if err != nil {
		return errorResult(err)
	}
	var input contract.SearchCapabilitiesInput
	if err := decodeStrictArgs(args, &input); err != nil {
		return errorResult(err)
	}
	res, err := s.cfg.Catalog.SearchCapabilities(ctx, turn.Scope, input)
	if err != nil {
		return errorResult(err)
	}
	return successResult(res)
}

func parseTaskMutation(args map[string]any) (contract.TaskMutation, error) {
	var mut contract.TaskMutation
	if err := decodeStrictArgs(args, &mut); err != nil {
		return contract.TaskMutation{}, err
	}
	if strings.TrimSpace(mut.Device) == "" || strings.TrimSpace(mut.Tool) == "" {
		return contract.TaskMutation{}, &contract.BusinessError{
			Code:    contract.InvalidArgument,
			Message: "device and tool are required",
		}
	}
	if mut.Params == nil {
		mut.Params = map[string]any{}
	}
	return mut, nil
}

func decodeStrictArgs(args map[string]any, dst any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return &contract.BusinessError{Code: contract.InvalidArgument, Message: "invalid arguments"}
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &contract.BusinessError{
			Code:    contract.InvalidArgument,
			Message: "invalid tool arguments: " + err.Error(),
		}
	}
	return nil
}

func successResult(payload any) map[string]any {
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

func errorResult(err error) map[string]any {
	var be *contract.BusinessError
	if !errors.As(err, &be) {
		be = &contract.BusinessError{
			Code:      contract.CapabilityUnavailable,
			Message:   err.Error(),
			Retryable: true,
		}
	}
	raw, _ := json.Marshal(be)
	return map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": string(raw),
		}},
		"structuredContent": *be,
		"isError":           true,
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// ToolDefinitions returns the MCP tools/list metadata aligned with H01 & contract/tools.go.
func ToolDefinitions() []map[string]any {
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
			"description": "Create a pending_reply vehicle task or partially fill missing parameters for an existing pending_reply task.",
			"inputSchema": mutationSchema,
			"annotations": map[string]any{"readOnlyHint": false},
		},
		{
			"name":        contract.ListVehicleTasks,
			"description": "List vehicle tasks in the current authenticated session and vehicle scope.",
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
			"description": "Dispatch a complete vehicle command. Include task_id only when resuming a uniquely matched pending_reply candidate.",
			"inputSchema": mutationSchema,
			"annotations": map[string]any{"readOnlyHint": false},
		},
		{
			"name":        contract.GetVehicleStatus,
			"description": "Query current vehicle status signals without creating any task.",
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
			"description": "Search the model- and version-specific vehicle manual.",
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
