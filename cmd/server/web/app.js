// Web Demo main controller: session bootstrap, dual SSE execution trace,
// simulated vehicle panel, capability drawer, and voice client integration.

const $ = (id) => document.getElementById(id);

const STATUS_LABELS = {
  running: '处理中',
  pending_reply: '等待追问',
  pending_dispatch: '待下发',
  executing: '执行中',
  completed: '已完成',
  failed: '未完成',
  expired: '已过期',
  result_unknown: '待确认',
  error: '失败',
  waiting: '等待中',
  interrupted: '已中断',
};

const DURATION_KIND_LABELS = {
  measured_local: '本地实测',
  measured_tool: '工具实测',
  boundary_wait: '模型 / 网络 / 调度区间',
  instant: '事件',
};

const WINDOW_LABELS = {
  front_left: '左前窗',
  front_right: '右前窗',
  rear_left: '左后窗',
  rear_right: '右后窗',
};

const SIM_MODE_LABELS = {
  auto_complete: '自动回执',
  accepted_only: '手动回执',
  retryable_once: '重试一次',
  rejected: '拒绝指令',
};

const state = {
  session: null, // { user_id, session_id, vehicle_id }
  config: null,
  capabilities: [],
  cursor: 0,
  telemetry: {},
  pendingTasks: [],
  recentTasks: [],
  activeCommands: [],
  vehicleResults: [],
  rawState: null,
  nextStateRequest: 0,
  appliedStateRequest: 0,

  // Timeline state (idempotent & alias-aware)
  turnOrder: [], // canonical turnKey list
  turns: new Map(), // turnKey -> turn record
  aliasToTurn: new Map(), // turn_id | request_id | command_id -> turnKey
  seenMessages: new Map(), // message_id -> record
  seenResultEvents: new Set(), // event_id
  userToggledProcess: new Map(), // turnKey -> boolean
  expandedStepData: new Set(), // `${turnKey}::${stepId}`

  // Streaming & connection
  eventSource: null,
  sseRetryMs: 1000,
  sseRetryTimer: null,
  busy: false,
  needsAuth: false,

  // Voice controller (loaded lazily from ./voice-client.js in W06)
  voiceClient: null,
  voiceMode: 'ptt', // 'ptt' | 'continuous'
  voiceConnected: false,
  voiceListening: false,
  voiceVadPaused: false,
  voiceTurnInFlight: false,
  voiceRecording: false,
  voicePlaying: false,
  voiceGeneration: 0,
  spaceHolding: false,
};

let toastTimer = null;

function toast(text) {
  const el = $('toast');
  el.textContent = text;
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => {
    el.hidden = true;
  }, 4500);
}

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null) node.textContent = String(text);
  return node;
}

function nearBottom() {
  const chat = $('chat');
  return chat.scrollHeight - chat.scrollTop - chat.clientHeight < 110;
}

function scrollChat(force = false) {
  const chat = $('chat');
  if (force || nearBottom()) {
    chat.scrollTop = chat.scrollHeight;
  }
}

function formatDuration(ms) {
  if (ms == null || Number.isNaN(ms)) return '—';
  if (ms < 1) return '<1 ms';
  if (ms < 1000) return `${Math.round(ms)} ms`;
  return `${(ms / 1000).toFixed(2)} s`;
}

function formatClock(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleTimeString('zh-CN', { hour12: false });
}

function formatCountdown(expiresAt) {
  if (!expiresAt) return '';
  const remainSec = Math.max(0, Math.floor((Date.parse(expiresAt) - Date.now()) / 1000));
  if (Number.isNaN(remainSec)) return '';
  if (remainSec <= 0) return '已过期';
  const m = Math.floor(remainSec / 60);
  const s = remainSec % 60;
  return `${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`;
}

function setStatusDot(dotId, labelId, level, text) {
  const dot = $(dotId);
  const label = $(labelId);
  if (dot) dot.className = level || '';
  if (label) label.textContent = text;
}

function makeRequestId() {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return `web-${crypto.randomUUID().slice(0, 12)}`;
  }
  return `web-${Date.now()}-${Math.random().toString(16).slice(2, 8)}`;
}

// ===== HTTP Helpers =====

async function apiFetch(path, { method = 'GET', body, allowUnauthorized = false } = {}) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const resp = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (resp.status === 401) {
    state.needsAuth = true;
    $('authBanner').hidden = false;
    if (allowUnauthorized) return null;
    throw new Error('需要 Demo 访问口令以建立会话');
  }
  const text = await resp.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = { message: text };
    }
  }
  if (!resp.ok) {
    const msg = data?.message || data?.detail || `HTTP ${resp.status}`;
    throw new Error(msg);
  }
  return data;
}

function sessionQueryParams(extra = {}) {
  const params = new URLSearchParams();
  if (state.session?.session_id) params.set('session_id', state.session.session_id);
  if (state.session?.vehicle_id) params.set('vehicle_id', state.session.vehicle_id);
  for (const [k, v] of Object.entries(extra)) {
    if (v !== undefined && v !== null && v !== '') params.set(k, String(v));
  }
  return params.toString();
}

// ===== Turn & Step Data Model (Idempotent Merge) =====

function resolveTurnKey({ turn_id, request_id, command_id, task_id, fallback_key, createIfMissing = false, channel = 'text' } = {}) {
  for (const key of [turn_id, request_id, command_id, fallback_key]) {
    if (key && state.aliasToTurn.has(key)) {
      const canonical = state.aliasToTurn.get(key);
      registerTurnAliases(canonical, { turn_id, request_id, command_id, fallback_key });
      return canonical;
    }
  }
  if (task_id && !turn_id && !request_id && !command_id) {
    // Match the most recent turn referencing this task_id (used for late executor steps).
    for (let i = state.turnOrder.length - 1; i >= 0; i--) {
      const t = state.turns.get(state.turnOrder[i]);
      if (t && t.taskId === task_id) {
        registerTurnAliases(t.key, { command_id, fallback_key });
        return t.key;
      }
    }
  }
  if (!createIfMissing) return null;

  const key = turn_id || request_id || command_id || fallback_key || `turn-${Date.now()}-${state.turnOrder.length + 1}`;
  const turn = {
    key,
    turnId: turn_id || '',
    requestId: request_id || '',
    runId: '',
    taskId: task_id || '',
    commandId: command_id || '',
    channel,
    createdAt: Date.now(),
    userText: '',
    userChannel: channel,
    userTime: new Date().toISOString(),
    assistantText: '',
    assistantTime: '',
    status: 'running',
    stepsOrder: [],
    steps: new Map(), // step_id -> stepRecord
    results: [], // VehicleResultEvent[]
  };
  state.turns.set(key, turn);
  state.turnOrder.push(key);
  registerTurnAliases(key, { turn_id, request_id, command_id, fallback_key });
  return key;
}

function registerTurnAliases(canonical, { turn_id, request_id, command_id, fallback_key } = {}) {
  if (canonical) state.aliasToTurn.set(canonical, canonical);
  if (turn_id) state.aliasToTurn.set(turn_id, canonical);
  if (request_id) state.aliasToTurn.set(request_id, canonical);
  if (command_id) state.aliasToTurn.set(command_id, canonical);
  if (fallback_key && !turn_id && !request_id && !command_id) {
    state.aliasToTurn.set(fallback_key, canonical);
  }
}

function mergeProcessStep(envelope) {
  if (!envelope || !envelope.step) return;
  const step = envelope.step;
  const stepId = step.step_id || `${step.stage || 'step'}:${envelope.seq || Date.now()}`;
  const hasPrimaryID = Boolean(envelope.turn_id || envelope.request_id || envelope.command_id || envelope.task_id);
  const turnKey = resolveTurnKey({
    turn_id: envelope.turn_id,
    request_id: envelope.request_id,
    command_id: envelope.command_id,
    task_id: envelope.task_id,
    fallback_key: hasPrimaryID ? '' : `step:${stepId}`,
    channel: envelope.channel || 'text',
    createIfMissing: true,
  });
  const turn = state.turns.get(turnKey);
  if (envelope.turn_id) turn.turnId = envelope.turn_id;
  if (envelope.request_id) turn.requestId = envelope.request_id;
  if (envelope.run_id) turn.runId = envelope.run_id;
  if (envelope.task_id) turn.taskId = envelope.task_id;
  if (envelope.command_id) turn.commandId = envelope.command_id;
  if (envelope.channel && envelope.channel !== 'executor') turn.channel = envelope.channel;

  const existing = turn.steps.get(stepId);
  const incomingSeq = Number(envelope.seq || 0);
  if (existing && incomingSeq > 0 && existing._seq > 0 && incomingSeq < existing._seq) {
    return;
  }

  const startedMs = step.started_at ? Date.parse(step.started_at) : (existing?._startedMs || Date.now());
  const merged = {
    step_id: stepId,
    stage: step.stage || existing?.stage || 'step',
    title: step.title || existing?.title || step.stage || '执行步骤',
    status: step.status || existing?.status || 'running',
    started_at: step.started_at || existing?.started_at || new Date(startedMs).toISOString(),
    finished_at: step.finished_at ?? existing?.finished_at ?? null,
    duration_ms: step.duration_ms ?? existing?.duration_ms ?? null,
    duration_kind: step.duration_kind || existing?.duration_kind || 'measured_local',
    detail: step.detail ?? existing?.detail ?? '',
    observation: step.observation !== undefined ? step.observation : existing?.observation,
    _startedMs: Number.isNaN(startedMs) ? Date.now() : startedMs,
    _seq: Math.max(incomingSeq, existing?._seq || 0),
  };

  if (!existing) {
    turn.stepsOrder.push(stepId);
  }
  turn.steps.set(stepId, merged);

  // If ASR transcript arrived in voice step and no user bubble text yet, surface it.
  if (merged.stage === 'voice.asr' && !turn.userText && merged.observation?.transcript) {
    turn.userText = merged.observation.transcript;
    turn.userChannel = 'voice';
  }

  // Reconcile turn-level status once turn.reply finishes (including non-task Q&A voice turns).
  if (merged.stage === 'turn.reply' && isTerminalStepStatus(merged.status)) {
    if (turn.status === 'running') {
      const replyStatus = merged.observation?.output?.status;
      turn.status =
        replyStatus ||
        (merged.status === 'error' ? 'error' : merged.status === 'interrupted' ? 'interrupted' : 'completed');
    }
    if (!turn.assistantTime && merged.finished_at) {
      turn.assistantTime = merged.finished_at;
    }
  }

  // Reconcile turn-level status for native S2S or standalone playback once playback finishes.
  if (merged.stage === 'voice.playback' && isTerminalStepStatus(merged.status) && turn.status === 'running') {
    const allTerminal = turn.stepsOrder.every((id) => {
      const s = turn.steps.get(id);
      return !s || isTerminalStepStatus(s.status);
    });
    if (allTerminal) {
      turn.status =
        merged.status === 'error' ? 'error' : merged.status === 'interrupted' ? 'interrupted' : 'completed';
    }
    if (!turn.assistantTime && merged.finished_at) {
      turn.assistantTime = merged.finished_at;
    }
  }

  renderTurn(turnKey);
}

function mergeMessage(msg) {
  if (!msg) return;
  const msgId = msg.message_id || msg.id;
  if (msgId && state.seenMessages.has(msgId)) {
    const prev = state.seenMessages.get(msgId);
    if ((msg.version || 0) <= (prev.version || 0) && prev.text === msg.text) return;
  }
  if (msgId) state.seenMessages.set(msgId, msg);

  const turnKey = resolveTurnKey({
    turn_id: msg.turn_id,
    request_id: msg.request_id,
    command_id: msg.command_id,
    task_id: msg.task_id,
    channel: msg.channel || 'text',
    createIfMissing: true,
  });
  const turn = state.turns.get(turnKey);
  if (msg.turn_id) turn.turnId = msg.turn_id;
  if (msg.request_id) turn.requestId = msg.request_id;
  if (msg.task_id) turn.taskId = msg.task_id;
  if (msg.command_id) turn.commandId = msg.command_id;

  if (msg.role === 'user') {
    turn.userText = msg.text || turn.userText;
    turn.userChannel = msg.channel || turn.userChannel || 'text';
    if (msg.ts || msg.created_at) turn.userTime = msg.ts || msg.created_at;
  } else {
    if (msg.text) turn.assistantText = msg.text;
    if (msg.status) {
      turn.status = msg.status;
    } else if (msg.text && turn.status === 'running') {
      turn.status = 'completed';
    }
    if (msg.ts || msg.created_at) turn.assistantTime = msg.ts || msg.created_at;
  }
  renderTurn(turnKey);
}

function applySpeechReply(reply, channel = 'text') {
  if (!reply) return;
  const turnKey = resolveTurnKey({
    turn_id: reply.voice_turn_id,
    request_id: reply.request_id,
    command_id: reply.command_id,
    task_id: reply.task_id,
    channel,
    createIfMissing: true,
  });
  const turn = state.turns.get(turnKey);
  if (reply.voice_turn_id) turn.turnId = reply.voice_turn_id;
  if (reply.request_id) turn.requestId = reply.request_id;
  if (reply.run_id) turn.runId = reply.run_id;
  if (reply.task_id) turn.taskId = reply.task_id;
  if (reply.command_id) turn.commandId = reply.command_id;
  if (reply.status) {
    turn.status = reply.status;
  } else if (turn.status === 'running') {
    turn.status = 'completed';
  }
  if (reply.text) {
    turn.assistantText = reply.text;
  }
  turn.assistantTime = new Date().toISOString();
  renderTurn(turnKey);
}

function applyVehicleResult(ev) {
  if (!ev || !ev.event_id) return;
  if (!state.vehicleResults.some((r) => r.event_id === ev.event_id)) {
    state.vehicleResults.unshift(ev);
    if (state.vehicleResults.length > 20) state.vehicleResults.length = 20;
  }
  // Remove from active commands list once terminal.
  if (ev.command_id) {
    state.activeCommands = state.activeCommands.filter((c) => c.command_id !== ev.command_id);
  }
  renderVehiclePanel();

  if (state.seenResultEvents.has(ev.event_id)) return;
  state.seenResultEvents.add(ev.event_id);

  const turnKey = resolveTurnKey({
    request_id: ev.request_id,
    command_id: ev.command_id,
    task_id: ev.task_id,
    channel: 'executor',
    createIfMissing: true,
  });
  const turn = state.turns.get(turnKey);
  if (ev.task_id) turn.taskId = ev.task_id;
  if (ev.command_id) turn.commandId = ev.command_id;
  turn.status = ev.status || 'completed';
  if (!turn.results.some((r) => r.event_id === ev.event_id)) {
    turn.results.push(ev);
  }
  renderTurn(turnKey);
}

function formatResultMessage(ev) {
  if (ev.message) return ev.message;
  const deviceLabel = ev.device === 'window' ? '车窗' : ev.device === 'hvac' ? '空调' : `${ev.device || '车辆'}`;
  if (ev.status === 'completed') {
    if (ev.device === 'window' && ev.tool === 'close') return '车窗已关闭。';
    if (ev.device === 'window' && ev.tool === 'open') return '车窗已打开。';
    if (ev.device === 'hvac') return '空调设置已生效。';
    return `${deviceLabel}指令已完成。`;
  }
  if (ev.status === 'failed') return `${deviceLabel}指令执行失败。`;
  if (ev.status === 'result_unknown') return `${deviceLabel}执行结果暂未确认，正在对账。`;
  return `${deviceLabel}状态更新：${ev.status}`;
}

// ===== Timeline DOM Rendering =====

function renderTurn(turnKey) {
  const turn = state.turns.get(turnKey);
  if (!turn) return;
  $('welcome').hidden = true;

  const container = $('timeline');
  const shouldFollow = nearBottom();
  let group = container.querySelector(`[data-turn-key="${CSS.escape(turnKey)}"]`);
  if (!group) {
    group = el('article', 'turn-group');
    group.dataset.turnKey = turnKey;
    container.append(group);
  }

  // Capture user-controlled open state on existing <details> before rebuilding.
  const prevDetails = group.querySelector('details.process');
  if (prevDetails && state.userToggledProcess.has(turnKey)) {
    state.userToggledProcess.set(turnKey, prevDetails.open);
  }

  group.replaceChildren();

  // 1. User message bubble
  if (turn.userText) {
    const userBlock = el('div', 'message-block is-user');
    const channelLabel = turn.userChannel === 'voice' ? '语音' : '文字';
    const metaText = ['你', channelLabel, formatClock(turn.userTime)].filter(Boolean).join(' · ');
    userBlock.append(el('div', 'message-meta', metaText));
    userBlock.append(el('div', 'user-bubble', turn.userText));
    group.append(userBlock);
  }

  // 2. Assistant block (header + collapsible execution process + reply text + vehicle result banner)
  const hasAssistantContent =
    turn.stepsOrder.length > 0 || Boolean(turn.assistantText) || turn.results.length > 0 || turn.status === 'running';
  if (hasAssistantContent) {
    const row = el('div', 'assistant-row');
    row.append(el('span', 'avatar', 'p'));
    const body = el('div', 'assistant-body');

    const meta = el('div', 'agent-meta');
    meta.append(el('span', '', 'Personal Agent'));
    meta.append(el('small', '', turn.channel === 'voice' ? 'QWEN × HERMES' : 'HERMES × GO MCP'));
    if (turn.taskId) meta.append(el('small', '', turn.taskId));
    const statusText = STATUS_LABELS[turn.status] || turn.status || '处理中';
    meta.append(el('span', `turn-status ${turn.status || ''}`, statusText));
    body.append(meta);

    if (turn.stepsOrder.length > 0) {
      body.append(buildProcessCard(turn));
    }

    if (turn.assistantText) {
      body.append(el('p', 'assistant-text', turn.assistantText));
    } else if (turn.status === 'running' && turn.stepsOrder.length === 0) {
      body.append(el('p', 'assistant-text', '正在等待服务端执行事件…'));
    }

    for (const res of turn.results) {
      const banner = el('div', `result-banner ${res.status || ''}`);
      const icon = res.status === 'completed' ? '✓' : res.status === 'failed' ? '×' : '!';
      banner.append(el('strong', '', `${icon} 车辆回执确认：${formatResultMessage(res)}`));
      banner.append(el('small', '', res.event_id));
      body.append(banner);
    }

    row.append(body);
    group.append(row);
  }

  if (shouldFollow) scrollChat(true);
}

function isTerminalStepStatus(status) {
  return status === 'done' || status === 'error' || status === 'interrupted';
}

function buildProcessCard(turn) {
  const rawSteps = turn.stepsOrder.map((id) => turn.steps.get(id)).filter(Boolean);
  const hermesRunStep = rawSteps.find((s) => s.step_id === 'hermes.run');
  const turnReplyStep = rawSteps.find((s) => s.stage === 'turn.reply');
  const hermesRunEnded = Boolean(
    (hermesRunStep && isTerminalStepStatus(hermesRunStep.status)) ||
      (turnReplyStep && isTerminalStepStatus(turnReplyStep.status))
  );
  const hermesRunEndIso =
    hermesRunStep?.finished_at || turnReplyStep?.finished_at || turn.assistantTime || null;
  const hermesRunEndMs = hermesRunEndIso ? Date.parse(hermesRunEndIso) : NaN;

  // Defensively freeze any orphaned Hermes sub-step whose parent hermes.run already ended,
  // so no stale step can keep timers ticking forever.
  const steps = rawSteps.map((step) => {
    const isOrphanHermesSubStep =
      step.status === 'running' &&
      typeof step.step_id === 'string' &&
      step.step_id.startsWith('hermes:') &&
      hermesRunEnded;
    if (isOrphanHermesSubStep) {
      const frozenStatus =
        hermesRunStep?.status === 'error'
          ? 'error'
          : hermesRunStep?.status === 'interrupted'
            ? 'interrupted'
            : 'done';
      const frozenDur =
        step.duration_ms != null
          ? step.duration_ms
          : !Number.isNaN(hermesRunEndMs)
            ? Math.max(0, hermesRunEndMs - step._startedMs)
            : 0;
      return {
        ...step,
        status: frozenStatus,
        finished_at: step.finished_at || hermesRunEndIso,
        duration_ms: frozenDur,
        _isLiveRunning: false,
      };
    }

    const finishedMs = step.finished_at ? Date.parse(step.finished_at) : NaN;
    const derivedDur =
      step.duration_ms != null
        ? step.duration_ms
        : !Number.isNaN(finishedMs)
          ? Math.max(0, finishedMs - step._startedMs)
          : null;
    const isLiveRunning = step.status === 'running' && derivedDur == null && !step.finished_at;
    return {
      ...step,
      duration_ms: derivedDur,
      _isLiveRunning: isLiveRunning,
    };
  });

  const activeStep = steps.find((s) => s._isLiveRunning);
  const hasRunning = Boolean(activeStep);

  const details = el('details', 'process');
  const defaultOpen = hasRunning || turn.status === 'running' || turn.status === 'pending_reply' || turn.status === 'executing';
  details.open = state.userToggledProcess.has(turn.key)
    ? state.userToggledProcess.get(turn.key)
    : defaultOpen;
  details.addEventListener('toggle', () => {
    state.userToggledProcess.set(turn.key, details.open);
  });

  const summary = el('summary');
  const summaryTitle = activeStep
    ? `处理过程 · ${activeStep.title}`
    : `处理过程 · ${steps.length} 步`;
  summary.append(el('span', 'process-title', summaryTitle));

  const totalEl = el('span', 'total-time');
  const firstStart = steps.reduce(
    (acc, s) => (typeof s._startedMs === 'number' && !Number.isNaN(s._startedMs) ? Math.min(acc, s._startedMs) : acc),
    steps[0]?._startedMs || turn.createdAt
  );
  if (hasRunning) {
    totalEl.dataset.timerStart = String(firstStart);
    totalEl.textContent = formatDuration(Math.max(0, Date.now() - firstStart));
  } else {
    const lastFinish = steps.reduce((acc, s) => {
      const f = s.finished_at ? Date.parse(s.finished_at) : (s._startedMs + (s.duration_ms || 0));
      return Math.max(acc, Number.isNaN(f) ? acc : f);
    }, firstStart);
    const sumMs = steps.reduce((acc, s) => acc + (Number(s.duration_ms) || 0), 0);
    const spanMs = Math.max(0, lastFinish - firstStart);
    totalEl.textContent = formatDuration(spanMs > 0 ? spanMs : sumMs);
  }
  summary.append(totalEl);
  details.append(summary);

  const body = el('div', 'process-body');
  const toolChips = [];

  for (const step of steps) {
    const row = el('div', `process-step ${step.status}`);
    const iconChar =
      step.status === 'running' ? '◌' :
      step.status === 'error' ? '×' :
      step.status === 'waiting' ? '◔' :
      step.status === 'interrupted' ? '—' : '✓';
    row.append(el('span', 'step-icon', iconChar));

    const main = el('div', 'step-main');
    main.append(el('strong', '', step.title));
    if (step.detail) {
      main.append(el('p', 'step-detail', step.detail));
    }

    const kindLabel = DURATION_KIND_LABELS[step.duration_kind] || step.duration_kind;
    if (kindLabel) {
      main.append(el('span', 'step-kind', kindLabel));
    }

    if (step.observation && hasObservationPayload(step.observation)) {
      const dataKey = `${turn.key}::${step.step_id}`;
      const dataBox = el('details', 'step-data');
      dataBox.open = state.expandedStepData.has(dataKey);
      dataBox.addEventListener('toggle', () => {
        if (dataBox.open) state.expandedStepData.add(dataKey);
        else state.expandedStepData.delete(dataKey);
      });
      dataBox.append(el('summary', '', '▸ 查看本步观测数据 (JSON)'));
      dataBox.append(el('pre', '', JSON.stringify(step.observation, null, 2)));
      main.append(dataBox);
    }

    if (step.stage === 'mcp.tool' || step.stage === 'mcp_tool') {
      toolChips.push({
        label: step.observation?.action || step.title,
        isError: step.status === 'error' || Boolean(step.observation?.error_code),
      });
    }

    const durEl = el('span', 'step-duration');
    if (step.duration_kind === 'instant') {
      durEl.textContent = '—';
    } else if (step._isLiveRunning) {
      durEl.dataset.timerStart = String(step._startedMs);
      durEl.textContent = formatDuration(Math.max(0, Date.now() - step._startedMs));
    } else {
      durEl.textContent = formatDuration(step.duration_ms);
    }

    row.append(main, durEl);
    body.append(row);
  }

  if (toolChips.length > 0) {
    const chipsWrap = el('div', 'tool-chips');
    for (const c of toolChips) {
      chipsWrap.append(el('span', `tool-chip ${c.isError ? 'error' : ''}`, `${c.isError ? '×' : '✓'} ${c.label}`));
    }
    body.append(chipsWrap);
  }

  body.append(el('p', 'timing-notice', '步骤来自 Go 服务端真实事件流（脱敏后推送）；边界等待区间包含模型、网络与调度时间。'));
  details.append(body);
  return details;
}

function hasObservationPayload(obs) {
  if (!obs || typeof obs !== 'object') return false;
  return Object.keys(obs).some((k) => obs[k] !== null && obs[k] !== undefined && obs[k] !== '');
}

function tickLiveTimers() {
  const now = Date.now();
  document.querySelectorAll('[data-timer-start]').forEach((node) => {
    const start = Number(node.dataset.timerStart);
    if (!Number.isNaN(start)) {
      node.textContent = formatDuration(Math.max(0, now - start));
    }
  });
  document.querySelectorAll('[data-expires-at]').forEach((node) => {
    node.textContent = formatCountdown(node.dataset.expiresAt);
  });
  if (state.pendingTasks.length > 0) {
    $('pendingBarTimer').textContent = `剩余 ${formatCountdown(state.pendingTasks[0].expires_at)}`;
  }
  const taskDeadlines = $('recentTasksList').querySelectorAll('[data-expires-at]');
  if (Array.from(taskDeadlines).some((node) => Date.parse(node.dataset.expiresAt) <= now)) {
    renderRecentTasks(now);
  }
}

// ===== Simulated Vehicle Panel Rendering =====

function readSignal(signals, name) {
  const pt = signals?.[name];
  if (!pt) return { available: false, value: null, sampledAt: null };
  const available = pt.Available ?? pt.available ?? true;
  const value = pt.Value ?? pt.value ?? null;
  const sampledAt = pt.SampledAt ?? pt.sampled_at ?? null;
  return { available, value, sampledAt };
}

function renderRecentTasks(now = Date.now()) {
  const list = $('recentTasksList');
  $('recentTasksCount').textContent = String(state.recentTasks.length);
  list.replaceChildren();
  if (state.recentTasks.length === 0) {
    list.append(el('p', 'panel-empty', '暂无任务'));
    return;
  }

  for (const task of state.recentTasks) {
    const { device, tool } = task.operation || {};
    const capability = state.capabilities.find((cap) => cap.device === device && cap.tool === tool);
    const title = capability?.title || (device && tool ? `${device}.${tool}` : '车辆任务');
    const status = task.status === 'pending_reply' && Date.parse(task.expires_at) <= now ? 'expired' : task.status;
    const label = { pending_reply: '待补充', failed: '执行失败' }[status] || STATUS_LABELS[status] || status;
    const card = el('div', 'panel-card');
    const head = el('div', 'panel-card-head');
    head.append(el('strong', '', title), el('span', `turn-status ${status}`, label));
    card.append(head);
    if (status === 'pending_reply' && task.question) {
      card.append(el('div', 'panel-card-sub', task.question));
    }
    const details = [formatClock(task.created_at), task.task_id].filter(Boolean).join(' · ');
    const meta = el('div', 'panel-card-meta', details);
    if (status === 'pending_reply' && task.expires_at) {
      const timer = el('span', '', formatCountdown(task.expires_at));
      timer.dataset.expiresAt = task.expires_at;
      meta.append(' · 剩余 ', timer);
    }
    card.append(meta);
    list.append(card);
  }
}

function renderVehiclePanel() {
  let latestSample = '';
  for (const pos of ['front_left', 'front_right', 'rear_left', 'rear_right']) {
    const sig = readSignal(state.telemetry, `window.${pos}`);
    if (sig.sampledAt && sig.sampledAt > latestSample) latestSample = sig.sampledAt;
    const textEl = $(`win-${pos}`);
    const svgEl = $(`svg-win-${pos}`);
    const slotEl = document.querySelector(`.window-slot[data-window="${pos}"]`);
    const isOpen = sig.value === 'open';
    const isClosed = sig.value === 'closed';
    if (textEl) textEl.textContent = !sig.available ? '未知' : isOpen ? '开启' : isClosed ? '已关闭' : String(sig.value ?? '—');
    if (svgEl) {
      svgEl.classList.toggle('open', isOpen);
      svgEl.classList.toggle('closed', isClosed);
    }
    if (slotEl) {
      slotEl.classList.toggle('open', isOpen);
      slotEl.classList.toggle('closed', isClosed);
    }
  }

  const hvacPower = readSignal(state.telemetry, 'hvac.power');
  const hvacTemp = readSignal(state.telemetry, 'hvac.temperature_c');
  if (hvacPower.sampledAt && hvacPower.sampledAt > latestSample) latestSample = hvacPower.sampledAt;
  if (hvacTemp.sampledAt && hvacTemp.sampledAt > latestSample) latestSample = hvacTemp.sampledAt;

  $('hvacPowerVal').textContent = !hvacPower.available
    ? '未知'
    : hvacPower.value === 'on' ? '开启' : hvacPower.value === 'off' ? '关闭' : String(hvacPower.value ?? '—');
  $('hvacTempVal').textContent = !hvacTemp.available || hvacTemp.value == null
    ? '—'
    : `${hvacTemp.value} °C`;
  $('telemetryUpdatedAt').textContent = latestSample ? formatClock(latestSample) : '等待刷新';

  // Composer context for tasks that still need user input.
  const pending = state.pendingTasks || [];
  $('pendingCounter').textContent = String(pending.length);

  const pendingBar = $('pendingBar');
  if (pending.length > 0) {
    const top = pending[0];
    pendingBar.hidden = false;
    $('pendingBarText').textContent = `待回复 (${pending.length}) · ${top.device}.${top.tool}：${top.question || '等待补充参数'}`;
    $('pendingBarTimer').textContent = `剩余 ${formatCountdown(top.expires_at)}`;
  } else {
    pendingBar.hidden = true;
  }

  renderRecentTasks();

  // Active commands (with manual receipt buttons when debug enabled)
  const active = state.activeCommands || [];
  $('activeCommandsCount').textContent = String(active.length);
  const cmdList = $('activeCommandsList');
  cmdList.replaceChildren();
  if (!active.length) {
    cmdList.append(el('p', 'panel-empty', '暂无执行中指令'));
  } else {
    for (const cmd of active) {
      const card = el('div', 'panel-card');
      const head = el('div', 'panel-card-head');
      head.append(el('strong', '', `${cmd.device}.${cmd.tool}`));
      head.append(el('span', 'panel-card-meta', STATUS_LABELS[cmd.status] || cmd.status));
      card.append(head);
      card.append(el('div', 'panel-card-meta', `${cmd.command_id}${cmd.downstream_id ? ` · ${cmd.downstream_id}` : ''}`));
      if (cmd.params && Object.keys(cmd.params).length) {
        card.append(el('div', 'panel-card-sub', JSON.stringify(cmd.params)));
      }
      if (state.config?.debug_enabled !== false) {
        const actions = el('div', 'receipt-actions');
        const okBtn = el('button', 'receipt-btn', '模拟完成');
        okBtn.type = 'button';
        okBtn.onclick = () => sendDebugReceipt(cmd.command_id, 'completed', okBtn);
        const failBtn = el('button', 'receipt-btn fail', '模拟失败');
        failBtn.type = 'button';
        failBtn.onclick = () => sendDebugReceipt(cmd.command_id, 'failed', failBtn);
        actions.append(okBtn, failBtn);
        card.append(actions);
      }
      cmdList.append(card);
    }
  }

  // Recent vehicle results
  const results = state.vehicleResults || [];
  $('resultsCount').textContent = String(results.length);
  const resList = $('resultsList');
  resList.replaceChildren();
  if (!results.length) {
    resList.append(el('p', 'panel-empty', '暂无已完成结果事件'));
  } else {
    for (const res of results.slice(0, 8)) {
      const card = el('div', 'panel-card');
      const head = el('div', 'panel-card-head');
      head.append(el('strong', '', `${res.device}.${res.tool}`));
      head.append(el('span', 'panel-card-meta', STATUS_LABELS[res.status] || res.status));
      card.append(head);
      card.append(el('div', 'panel-card-sub', formatResultMessage(res)));
      card.append(el('div', 'panel-card-meta', `${res.event_id} · ${formatClock(res.occurred_at)}`));
      resList.append(card);
    }
  }
}

async function sendDebugReceipt(commandId, status, button) {
  if (button) button.disabled = true;
  try {
    await apiFetch('/debug/receipt', {
      method: 'POST',
      body: {
        command_id: commandId,
        status,
        reason: status === 'failed' ? 'simulated failure from web demo' : '',
      },
    });
    toast(`已发送模拟回执：${commandId} → ${status}`);
    await refreshState().catch(() => {});
  } catch (err) {
    toast(`发送回执失败：${err.message}`);
    if (button) button.disabled = false;
  }
}

// ===== Session-Level SSE (/api/events/stream) =====

function connectEventStream() {
  clearTimeout(state.sseRetryTimer);
  if (state.eventSource) {
    state.eventSource.close();
    state.eventSource = null;
  }
  if (state.needsAuth || !state.session) return;

  const qs = sessionQueryParams({ after: state.cursor });
  const es = new EventSource(`/api/events/stream?${qs}`);
  state.eventSource = es;
  setStatusDot('sseDot', 'sseStatus', 'warn', '连接中');

  es.onopen = () => {
    if (state.eventSource !== es) return;
    state.sseRetryMs = 1000;
    setStatusDot('sseDot', 'sseStatus', 'ready', '实时同步');
  };

  const trackSeq = (event, data) => {
    const seq = Number(data?.seq ?? event.lastEventId ?? 0);
    if (!Number.isNaN(seq) && seq > state.cursor) state.cursor = seq;
  };

  es.addEventListener('process.step', (event) => {
    try {
      const data = JSON.parse(event.data);
      trackSeq(event, data);
      mergeProcessStep(data);
    } catch {}
  });

  es.addEventListener('message.upsert', (event) => {
    try {
      const data = JSON.parse(event.data);
      trackSeq(event, data);
      mergeMessage(data.message || data);
    } catch {}
  });

  es.addEventListener('task.updated', (event) => {
    try {
      const data = JSON.parse(event.data);
      trackSeq(event, data);
      if (Array.isArray(data.pending_tasks)) {
        state.pendingTasks = data.pending_tasks;
      }
      if (Array.isArray(data.active_commands)) {
        state.activeCommands = data.active_commands;
      }
      renderVehiclePanel();
      refreshState({ quiet: true }).catch(() => {});
    } catch {}
  });

  es.addEventListener('vehicle.result', (event) => {
    try {
      const data = JSON.parse(event.data);
      trackSeq(event, data);
      applyVehicleResult(data.result || data);
    } catch {}
  });

  es.addEventListener('telemetry.updated', (event) => {
    try {
      const data = JSON.parse(event.data);
      trackSeq(event, data);
      if (data.telemetry && typeof data.telemetry === 'object') {
        state.telemetry = { ...state.telemetry, ...data.telemetry };
        renderVehiclePanel();
      }
    } catch {}
  });

  es.addEventListener('stream.reset', async (event) => {
    try {
      const data = JSON.parse(event.data);
      if (data.cursor != null) state.cursor = Number(data.cursor) || 0;
    } catch {}
    await refreshState({ quiet: true }).catch(() => {});
  });

  es.addEventListener('ping', () => {
    // Keepalive heartbeat; no UI change needed.
  });

  es.onerror = () => {
    if (state.eventSource !== es) return;
    es.close();
    state.eventSource = null;
    setStatusDot('sseDot', 'sseStatus', 'warn', '重连中');
    const delay = state.sseRetryMs;
    state.sseRetryMs = Math.min(state.sseRetryMs * 2, 10000);
    state.sseRetryTimer = setTimeout(async () => {
      await refreshState({ quiet: true }).catch(() => {});
      connectEventStream();
    }, delay);
  };
}

// ===== Streaming Text Submission (POST /voice/utterances) =====

async function submitTextUtterance(text) {
  if (!text || state.busy || state.needsAuth || !state.session) return;
  const requestId = makeRequestId();
  state.busy = true;
  $('sendButton').disabled = true;

  // Render optimistic user bubble immediately & anchor upcoming steps by request_id.
  const turnKey = resolveTurnKey({ request_id: requestId, channel: 'text', createIfMissing: true });
  const turn = state.turns.get(turnKey);
  turn.userText = text;
  turn.userChannel = 'text';
  turn.userTime = new Date().toISOString();
  turn.status = 'running';
  renderTurn(turnKey);
  scrollChat(true);

  try {
    const resp = await fetch('/voice/utterances', {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        'Content-Type': 'application/json',
        Accept: 'text/event-stream',
      },
      body: JSON.stringify({
        session_id: state.session.session_id,
        vehicle_id: state.session.vehicle_id,
        request_id: requestId,
        text,
      }),
    });

    if (resp.status === 401) {
      state.needsAuth = true;
      $('authBanner').hidden = false;
      throw new Error('会话已失效，请重新输入 Demo 口令');
    }

    if (!resp.ok) {
      const errText = await resp.text();
      let msg = errText || `HTTP ${resp.status}`;
      try {
        const parsed = JSON.parse(errText);
        msg = parsed.message || parsed.code || msg;
      } catch {}
      throw new Error(msg);
    }

    await consumeSSEResponse(resp, (eventName, rawData) => {
      let payload;
      try {
        payload = JSON.parse(rawData);
      } catch {
        return;
      }
      if (eventName === 'process.step') {
        mergeProcessStep(payload);
      } else if (eventName === 'speech.reply' || (!eventName && payload.voice_turn_id)) {
        applySpeechReply(payload, 'text');
      } else if (eventName === 'turn.error') {
        const t = state.turns.get(turnKey);
        if (t) {
          t.status = 'error';
          t.assistantText = payload.message || payload.code || '本轮处理失败';
          renderTurn(turnKey);
        }
      }
    });

    await refreshState({ quiet: true }).catch(() => {});
  } catch (err) {
    const t = state.turns.get(turnKey);
    if (t && t.status === 'running') {
      t.status = 'error';
      t.assistantText = `请求失败：${err.message}`;
      renderTurn(turnKey);
    }
    toast(err.message);
  } finally {
    state.busy = false;
    $('sendButton').disabled = false;
  }
}

async function consumeSSEResponse(resp, onEvent) {
  if (!resp.body) {
    const full = await resp.text();
    parseSSEChunks(full, onEvent);
    return;
  }
  const reader = resp.body.getReader();
  const decoder = new TextDecoder('utf-8');
  let buffer = '';
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    buffer = drainSSEBuffer(buffer, onEvent);
  }
  buffer += decoder.decode();
  if (buffer.trim()) {
    drainSSEBuffer(`${buffer}\n\n`, onEvent);
  }
}

function parseSSEChunks(text, onEvent) {
  drainSSEBuffer(`${text}\n\n`, onEvent);
}

function drainSSEBuffer(buffer, onEvent) {
  const normalized = buffer.replace(/\r\n/g, '\n');
  const blocks = normalized.split('\n\n');
  const remainder = blocks.pop() ?? '';
  for (const block of blocks) {
    if (!block.trim()) continue;
    let eventName = '';
    const dataLines = [];
    for (const line of block.split('\n')) {
      if (line.startsWith(':')) continue;
      if (line.startsWith('event:')) {
        eventName = line.slice(6).trim();
      } else if (line.startsWith('data:')) {
        dataLines.push(line.slice(5).replace(/^ /, ''));
      }
    }
    if (dataLines.length > 0) {
      onEvent(eventName, dataLines.join('\n'));
    }
  }
  return remainder;
}

// ===== Snapshot & Catalog Loading =====

async function initSession(token = '') {
  const body = {};
  if (state.session?.session_id) body.session_id = state.session.session_id;
  if (state.session?.vehicle_id) body.vehicle_id = state.session.vehicle_id;
  if (token) body.token = token;

  const data = await apiFetch('/api/session', { method: 'POST', body, allowUnauthorized: true });
  if (!data) return false;

  state.needsAuth = false;
  $('authBanner').hidden = true;
  state.session = {
    user_id: data.user_id || 'demo-user',
    session_id: data.session_id || 'web-demo',
    vehicle_id: data.vehicle_id || 'sim-car',
  };
  if (typeof data.cursor === 'number') {
    state.cursor = Math.max(state.cursor, data.cursor);
  }
  $('vehicleChip').textContent = state.session.vehicle_id;
  return true;
}

async function loadConfig() {
  const cfg = await apiFetch('/api/config');
  state.config = cfg;

  setStatusDot(
    'hermesDot',
    'hermesStatus',
    cfg.hermes_ready ? 'ready' : 'error',
    cfg.hermes_ready ? 'API 已连接' : '未连接'
  );
  setStatusDot(
    'voiceDot',
    'voiceStatus',
    cfg.voice_enabled ? (state.voiceConnected ? 'ready' : 'warn') : '',
    cfg.voice_enabled ? (state.voiceConnected ? '已连接' : '已就绪 · 待连接') : '未配置 Key'
  );

  const simLabel = SIM_MODE_LABELS[cfg.sim_mode] || cfg.sim_mode || '自动回执';
  setStatusDot('simDot', 'simStatus', 'ready', simLabel);
  $('simModeBadge').textContent = simLabel;
  $('simModeBadge').classList.toggle('manual', cfg.sim_mode === 'accepted_only');

  const model = cfg.default_vehicle?.model || 'EV-SUV-2026';
  const vid = state.session?.vehicle_id || cfg.default_vehicle?.vehicle_id || 'sim-car';
  $('vehicleModelTitle').textContent = model;
  $('vehicleChip').textContent = `${vid} · ${model}`;
  $('sidebarFoot').textContent = `DEMO · ${cfg.storage || 'sqlite'} · TTS ${cfg.tts_backend || 'auto'}`;

  $('voiceConnectBtn').disabled = !cfg.voice_enabled;
  if (!cfg.voice_enabled) {
    $('voiceConnectBtn').title = '未配置 DASHSCOPE_API_KEY，当前仅启用文字模式';
  } else {
    $('voiceConnectBtn').title = '连接 /voice/audio 实时语音通道';
  }

  const suggestions = $('suggestions');
  suggestions.replaceChildren();
  const list = Array.isArray(cfg.suggestions) && cfg.suggestions.length
    ? cfg.suggestions
    : ['把车窗关一下', '打开空调调到24度', '左后窗关了吗', '儿童锁怎么用'];
  for (const prompt of list) {
    const chip = el('button', 'chip', prompt);
    chip.type = 'button';
    chip.onclick = () => {
      $('message').value = prompt;
      $('message').focus();
    };
    suggestions.append(chip);
  }
}

async function loadCapabilities() {
  const qs = sessionQueryParams();
  const data = await apiFetch(`/api/capabilities?${qs}`);
  const caps = Array.isArray(data) ? data : (Array.isArray(data?.capabilities) ? data.capabilities : []);
  state.capabilities = caps;
  $('catalogCount').textContent = `${caps.length} 项`;
  $('catalogSummary').textContent = `当前车型共启用 ${caps.length} 项能力（来源：vehicle/catalog 单一事实源）`;

  const grid = $('catalogGrid');
  grid.replaceChildren();
  for (const cap of caps) {
    const card = el('div', 'catalog-card');
    const opName = `${cap.device}.${cap.tool}`;
    card.append(el('strong', '', cap.description || opName));
    card.append(el('code', '', `${opName} → ${cap.mcp_tool || ''}`));
    if (Array.isArray(cap.aliases) && cap.aliases.length) {
      card.append(el('p', '', `关键词：${cap.aliases.slice(0, 6).join('、')}`));
    }
    const actions = el('div', 'catalog-card-actions');
    const sample = Array.isArray(cap.utterances) && cap.utterances.length ? cap.utterances[0] : '';
    if (sample) {
      const useBtn = el('button', 'text-button', '填入示例');
      useBtn.type = 'button';
      useBtn.onclick = () => {
        $('message').value = sample;
        $('catalogDialog').close();
        $('message').focus();
      };
      actions.append(useBtn);
    }
    const schemaBtn = el('button', 'text-button', '查看 Schema');
    schemaBtn.type = 'button';
    schemaBtn.onclick = () => {
      $('schemaTitle').textContent = `${opName} · 参数 Schema`;
      $('schemaCode').textContent = JSON.stringify(cap.params_schema ?? cap, null, 2);
      $('schemaViewer').hidden = false;
      $('schemaViewer').scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    };
    actions.append(schemaBtn);
    card.append(actions);
    grid.append(card);
  }
}

async function refreshState({ quiet = false } = {}) {
  if (state.needsAuth || !state.session) return;
  const session = state.session;
  const request = ++state.nextStateRequest;
  const qs = sessionQueryParams();
  const data = await apiFetch(`/api/state?${qs}`);
  if (!data || state.session !== session || request < state.appliedStateRequest) return;
  state.appliedStateRequest = request;

  state.rawState = data;
  if (typeof data.cursor === 'number' && data.cursor > state.cursor) {
    state.cursor = data.cursor;
  }
  if (data.telemetry && typeof data.telemetry === 'object') {
    state.telemetry = data.telemetry;
  }
  state.pendingTasks = Array.isArray(data.pending_tasks) ? data.pending_tasks : [];
  state.recentTasks = Array.isArray(data.recent_tasks) ? data.recent_tasks : [];
  state.activeCommands = Array.isArray(data.active_commands) ? data.active_commands : [];
  if (Array.isArray(data.vehicle_results)) {
    state.vehicleResults = data.vehicle_results;
  }
  for (const m of Array.isArray(data.messages) ? data.messages : []) {
    mergeMessage(m);
  }
  for (const r of Array.isArray(data.vehicle_results) ? data.vehicle_results : []) {
    applyVehicleResult(r);
  }
  for (const s of Array.isArray(data.process_steps) ? data.process_steps : []) {
    mergeProcessStep(s);
  }
  renderVehiclePanel();

  if ($('stateDialog').open) {
    renderStateSnapshotDialog();
  }
  if (!quiet) {
    $('stateCursorLabel').textContent = `cursor: ${state.cursor}`;
  }
}

function renderStateSnapshotDialog() {
  $('stateCursorLabel').textContent = `cursor: ${state.cursor}`;
  $('stateJsonCode').textContent = JSON.stringify(state.rawState || {}, null, 2);
}

// ===== Voice Integration (Lazy Bridge to W06 ./voice-client.js) =====

const SILENT_UNLOCK_WAV =
  'data:audio/wav;base64,UklGRkYAAABXQVZFZm10IBAAAAABAAEAwF0AAIC7AAACABAAZGF0YSIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA';

function prepareGestureUnlockedPlayback() {
  let playbackCtx = null;
  let audioEl = null;
  try {
    if (navigator.audioSession) navigator.audioSession.type = 'play-and-record';
  } catch {}
  try {
    const AudioCtx = window.AudioContext || window.webkitAudioContext;
    if (AudioCtx) {
      playbackCtx = new AudioCtx();
      playbackCtx.resume?.().catch(() => {});
    }
  } catch {}
  try {
    audioEl = document.createElement('audio');
    audioEl.preload = 'auto';
    audioEl.playsInline = true;
    audioEl.muted = false;
    audioEl.volume = 1.0;
    audioEl.style.display = 'none';
    document.body.appendChild(audioEl);
    audioEl.src = SILENT_UNLOCK_WAV;
    const p = audioEl.play?.();
    if (p && typeof p.then === 'function') {
      p.then(() => {
        audioEl.dataset.unlocked = '1';
        audioEl.pause();
      }).catch(() => {});
    } else {
      audioEl.dataset.unlocked = '1';
    }
  } catch {}
  return { playbackCtx, audioEl };
}

async function toggleVoiceConnection() {
  if (state.voiceClient) {
    state.voiceClient.disconnect();
    state.voiceClient = null;
    updateVoiceUI({ connected: false, recording: false, playing: false, generation: 0 });
    return;
  }
  if (!state.config?.voice_enabled) {
    toast('未配置 DASHSCOPE_API_KEY，暂不可使用语音');
    return;
  }
  if (!state.session) {
    toast('会话尚未就绪');
    return;
  }

  // Unlock Safari/WebKit audio playback synchronously inside the click gesture before any await.
  const unlocked = prepareGestureUnlockedPlayback();

  $('voiceConnectBtn').disabled = true;
  $('voiceConnectBtn').textContent = '◉ 连接中…';
  setStatusDot('voiceDot', 'voiceStatus', 'warn', '连接中');

  try {
    const mod = await import('./voice-client.js');
    if (typeof mod?.VehicleVoiceClient !== 'function') {
      throw new Error('语音客户端模块不可用（请先完成 W06）');
    }
    const client = new mod.VehicleVoiceClient({
      sessionId: state.session.session_id,
      vehicleId: state.session.vehicle_id,
      mode: state.voiceMode,
      playbackCtx: unlocked.playbackCtx,
      audioEl: unlocked.audioEl,
      onStateChange: (vState) => updateVoiceUI(vState),
      onReply: (reply) => applySpeechReply(reply, 'voice'),
      onResult: (result) => applyVehicleResult(result),
      onError: (err) => toast(`语音通道：${err.message || err}`),
    });
    state.voiceClient = client;
    await client.connect();
  } catch (err) {
    try { unlocked.playbackCtx?.close?.(); } catch {}
    try { unlocked.audioEl?.remove?.(); } catch {}
    state.voiceClient = null;
    updateVoiceUI({ connected: false, recording: false, playing: false, generation: 0 });
    toast(err.message || '连接语音通道失败');
  } finally {
    $('voiceConnectBtn').disabled = !state.config?.voice_enabled;
  }
}

function setVoiceMode(mode) {
  const next = mode === 'continuous' ? 'continuous' : 'ptt';
  state.voiceMode = next;
  if (state.voiceClient) {
    state.voiceClient.setMode(next);
  }
  updateVoiceUI({
    connected: state.voiceConnected,
    mode: state.voiceMode,
    listening: state.voiceClient ? state.voiceClient.listening : false,
    vadPaused: state.voiceClient ? state.voiceClient.vadPaused : false,
    turnInFlight: state.voiceClient ? state.voiceClient.turnInFlight : false,
    recording: state.voiceRecording,
    playing: state.voicePlaying,
    generation: state.voiceGeneration,
  });
}

function updateVoiceUI({ connected, mode, listening, vadPaused, turnInFlight, recording, playing, generation, reason } = {}) {
  if (mode) state.voiceMode = mode === 'continuous' ? 'continuous' : 'ptt';
  state.voiceConnected = Boolean(connected);
  state.voiceListening = Boolean(listening);
  state.voiceVadPaused = Boolean(vadPaused);
  state.voiceTurnInFlight = Boolean(turnInFlight);
  state.voiceRecording = Boolean(recording);
  state.voicePlaying = Boolean(playing);
  if (generation != null) state.voiceGeneration = generation;

  const isContinuous = state.voiceMode === 'continuous';
  $('modePttBtn').classList.toggle('active', !isContinuous);
  $('modePttBtn').setAttribute('aria-pressed', String(!isContinuous));
  $('modeContinuousBtn').classList.toggle('active', isContinuous);
  $('modeContinuousBtn').setAttribute('aria-pressed', String(isContinuous));

  $('voiceConnectBtn').setAttribute('aria-pressed', String(state.voiceConnected));
  $('voiceConnectBtn').textContent = state.voiceConnected ? '◉ 断开语音' : '◉ 连接语音';

  const pttBtn = $('pttBtn');
  pttBtn.disabled = !state.voiceConnected;
  pttBtn.setAttribute('aria-pressed', String(state.voiceRecording));
  pttBtn.classList.toggle('is-listening', state.voiceConnected && isContinuous && state.voiceListening && !state.voiceRecording);

  if (isContinuous) {
    if (state.voiceRecording) {
      pttBtn.textContent = '正在收音 · 点击发送';
    } else if (state.voicePlaying) {
      pttBtn.textContent = '打断并说话';
    } else if (state.voiceVadPaused) {
      pttBtn.textContent = '恢复持续监听';
    } else if (state.voiceTurnInFlight) {
      pttBtn.textContent = '处理中 · 点击插话';
    } else {
      pttBtn.textContent = state.voiceConnected ? '监听中 · 暂停' : '持续对话';
    }
  } else {
    pttBtn.textContent = state.voiceRecording ? '正在录音 · 松开发送' : '按住说话';
  }

  $('channelBadge').textContent = state.voiceConnected
    ? (isContinuous ? '持续语音 + 文字' : '按住语音 + 文字')
    : '文字入口';
  setStatusDot(
    'voiceDot',
    'voiceStatus',
    state.voiceConnected ? 'ready' : state.config?.voice_enabled ? 'warn' : '',
    state.voiceConnected
      ? (state.voiceRecording
          ? '收音中'
          : state.voicePlaying
            ? '播报中'
            : isContinuous && state.voiceListening
              ? '持续监听'
              : '已连接')
      : (state.config?.voice_enabled ? '已就绪 · 待连接' : '未配置 Key')
  );

  const bar = $('voiceBar');
  bar.hidden = !state.voiceConnected;
  const indicator = $('voiceIndicator');
  indicator.className =
    'voice-indicator' +
    (state.voiceRecording
      ? ' recording'
      : state.voicePlaying
        ? ' playing'
        : isContinuous && state.voiceListening
          ? ' listening'
          : '');
  $('voiceBarText').textContent = state.voiceRecording
    ? (isContinuous ? '检测到说话 · 停顿自动发送…' : '正在采集 16 kHz PCM 语音…')
    : state.voicePlaying
      ? (isContinuous ? '正在播报语音回复（播完自动恢复监听，也可点击打断）' : '正在播报 24 kHz 语音回复（起讲可立即打断）')
      : reason === 'reconnecting'
        ? '语音连接重建中，请稍候重说…'
        : isContinuous
          ? (state.voiceVadPaused
              ? '持续对话已暂停，点击「恢复持续监听」继续'
              : state.voiceTurnInFlight
                ? '正在等待本轮回复…'
                : '持续对话监听中 · 直接开口说话，停顿后自动发送')
          : '按住说话模式已连接（按住按钮或按住空格键说话）';
  $('voiceGenBadge').textContent = state.voiceGeneration ? `gen #${state.voiceGeneration}` : '';
  $('stopPlaybackBtn').hidden = !state.voicePlaying;
}

// ===== Event Wiring =====

function wireEvents() {
  $('composer').addEventListener('submit', (e) => {
    e.preventDefault();
    const text = $('message').value.trim();
    if (!text || state.busy) return;
    $('message').value = '';
    submitTextUtterance(text);
  });

  $('message').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      $('composer').requestSubmit();
    }
  });

  $('authBanner').addEventListener('submit', async (e) => {
    e.preventDefault();
    const token = $('authTokenInput').value.trim();
    if (!token) {
      toast('请输入 Demo 访问口令');
      return;
    }
    $('authSubmitBtn').disabled = true;
    try {
      const ok = await initSession(token);
      if (!ok) {
        toast('口令无效，请核对 DEMO_AUTH_TOKEN');
        return;
      }
      $('authTokenInput').value = '';
      await bootstrapAfterAuth();
      toast('会话已建立');
    } catch (err) {
      toast(err.message);
    } finally {
      $('authSubmitBtn').disabled = false;
    }
  });

  $('voiceConnectBtn').addEventListener('click', toggleVoiceConnection);

  $('testAudioBtn').addEventListener('click', async () => {
    if (!state.voiceClient) return;
    try {
      const info = await state.voiceClient.playTestTone();
      toast(`测试提示音已播报（${info.mode}）`);
    } catch (err) {
      toast(`测试播报失败：${err.message || err}`);
    }
  });

  $('stopPlaybackBtn').addEventListener('click', () => {
    state.voiceClient?.stopPlayback?.();
  });

  $('modePttBtn').addEventListener('click', () => setVoiceMode('ptt'));
  $('modeContinuousBtn').addEventListener('click', () => setVoiceMode('continuous'));

  // Voice button interactions: hold-to-talk (PTT) OR hands-free continuous controls
  const ptt = $('pttBtn');
  ptt.addEventListener('pointerdown', (e) => {
    if (!state.voiceConnected || !state.voiceClient) return;
    if (state.voiceMode !== 'ptt') return;
    e.preventDefault();
    ptt.setPointerCapture?.(e.pointerId);
    state.voiceClient.startTalking();
  });
  const endHold = () => {
    if (!state.voiceConnected || !state.voiceClient) return;
    if (state.voiceMode !== 'ptt') return;
    if (state.voiceRecording) state.voiceClient.stopTalking();
  };
  ptt.addEventListener('pointerup', endHold);
  ptt.addEventListener('pointercancel', endHold);
  ptt.addEventListener('click', () => {
    if (!state.voiceConnected || !state.voiceClient || state.voiceMode !== 'continuous') return;
    if (state.voiceRecording) {
      state.voiceClient.stopTalking();
    } else if (state.voicePlaying || state.voiceTurnInFlight) {
      state.voiceClient.startTalking();
    } else {
      state.voiceClient.setVadPaused(!state.voiceVadPaused);
    }
  });

  // Spacebar PTT (or manual barge-in in continuous mode) when input is not focused
  window.addEventListener('keydown', (e) => {
    if (e.code !== 'Space' || e.repeat || !state.voiceConnected || !state.voiceClient) return;
    const tag = document.activeElement?.tagName;
    if (tag === 'TEXTAREA' || tag === 'INPUT' || tag === 'BUTTON') return;
    e.preventDefault();
    state.spaceHolding = true;
    state.voiceClient.startTalking();
  });
  window.addEventListener('keyup', (e) => {
    if (e.code !== 'Space' || !state.spaceHolding) return;
    state.spaceHolding = false;
    if (state.voiceRecording) state.voiceClient?.stopTalking();
  });

  // Sidebar & drawers
  $('focusChat').addEventListener('click', () => {
    $('catalogDialog').close();
    $('stateDialog').close();
    $('message').focus();
  });
  $('openCatalog').addEventListener('click', () => {
    if (!$('catalogDialog').open) $('catalogDialog').showModal();
  });
  $('openStateModal').addEventListener('click', async () => {
    await refreshState({ quiet: true }).catch(() => {});
    renderStateSnapshotDialog();
    if (!$('stateDialog').open) $('stateDialog').showModal();
  });
  document.querySelectorAll('[data-close]').forEach((btn) => {
    btn.addEventListener('click', () => $(btn.dataset.close)?.close());
  });
  $('copySchemaBtn').addEventListener('click', () => copyText($('schemaCode').textContent));
  $('copyStateJsonBtn').addEventListener('click', () => copyText($('stateJsonCode').textContent));
  $('refreshStateJsonBtn').addEventListener('click', async () => {
    await refreshState({ quiet: true }).catch((e) => toast(e.message));
    renderStateSnapshotDialog();
  });

  // Header controls
  $('toggleVehiclePanel').addEventListener('click', () => {
    const open = $('vehiclePanel').classList.toggle('open');
    $('toggleVehiclePanel').setAttribute('aria-expanded', String(open));
  });
  $('closeVehiclePanel').addEventListener('click', () => {
    $('vehiclePanel').classList.remove('open');
    $('toggleVehiclePanel').setAttribute('aria-expanded', 'false');
  });
  $('reconnectBtn').addEventListener('click', async () => {
    await refreshState().catch((e) => toast(e.message));
    connectEventStream();
    toast('已重新同步状态与 SSE');
  });
  $('clearLocalBtn').addEventListener('click', () => {
    state.turnOrder = [];
    state.turns.clear();
    state.aliasToTurn.clear();
    state.seenMessages.clear();
    $('timeline').replaceChildren();
    $('welcome').hidden = false;
  });

  window.addEventListener('pagehide', () => {
    clearTimeout(state.sseRetryTimer);
    state.eventSource?.close();
    state.voiceClient?.disconnect?.();
  });
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text || '');
    toast('已复制到剪贴板');
  } catch {
    toast('复制失败，请手动选中复制');
  }
}

async function bootstrapAfterAuth() {
  await loadConfig();
  await Promise.all([
    loadCapabilities().catch(() => {}),
    refreshState({ quiet: true }).catch(() => {}),
  ]);
  connectEventStream();
}

async function bootstrap() {
  wireEvents();
  setInterval(tickLiveTimers, 100);
  setInterval(() => {
    if (!state.needsAuth && state.session) {
      refreshState({ quiet: true }).catch(() => {});
    }
  }, 10000);

  try {
    const ok = await initSession();
    if (ok) {
      await bootstrapAfterAuth();
    }
  } catch (err) {
    toast(`初始化失败：${err.message}`);
  }
}

bootstrap();
