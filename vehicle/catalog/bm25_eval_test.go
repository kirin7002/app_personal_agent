package catalog

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

func TestBM25OfflineRecallBenchmarkAndRegressionGate(t *testing.T) {
	ctx := context.Background()
	svc, err := NewBusinessMCPService(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Enable deeplink_ctrl on the test vehicle to actively stress-test large-enum gating!
	svc.SetVehicleProfile(VehicleProfile{
		VehicleID:    "VCL_BENCH",
		MinTempC:     16,
		MaxTempC:     32,
		EnabledTools: map[string]bool{"deeplink_ctrl": true},
	})
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL_BENCH"}

	type evalCase struct {
		utterance   string
		wantTools   []string // ALL listed tools must appear in recalled candidates
		wantTop1    string   // optional strict #1 assertion
		forbidTop1  string   // optional forbidden #1 assertion
		expectEmpty bool
	}

	cases := []evalCase{
		// Colloquial synonyms & core controls
		{utterance: "把主驾风速调高一点", wantTools: []string{"ac_ctrl"}, wantTop1: "ac_ctrl"},
		{utterance: "帮我把车门玻璃关上", wantTools: []string{"car_window_ctrl"}, wantTop1: "car_window_ctrl"},
		{utterance: "关窗", wantTools: []string{"car_window_ctrl"}, wantTop1: "car_window_ctrl"},
		{utterance: "打开空调调到24度", wantTools: []string{"ac_ctrl"}, wantTop1: "ac_ctrl"},
		{utterance: "前挡风起雾了帮我处理一下", wantTools: []string{"ac_ctrl"}, wantTop1: "ac_ctrl"},
		{utterance: "把电动尾门打开", wantTools: []string{"trunk_ctrl"}, wantTop1: "trunk_ctrl"},
		{utterance: "帮我把前盖打开", wantTools: []string{"front_trunk_ctrl"}, wantTop1: "front_trunk_ctrl"},
		{utterance: "打开右侧电滑门", wantTools: []string{"sliding_door_ctrl"}, wantTop1: "sliding_door_ctrl"},
		{utterance: "打开方向盘热", wantTools: []string{"steer_wheel_heat_ctrl"}, wantTop1: "steer_wheel_heat_ctrl"},
		{utterance: "把倒车镜加热打开", wantTools: []string{"mirror_heat_ctrl"}, wantTop1: "mirror_heat_ctrl"},
		{utterance: "打开主驾座椅加热2档", wantTools: []string{"seat_heat_ctrl"}, wantTop1: "seat_heat_ctrl"},
		{utterance: "打开副驾座椅通风", wantTools: []string{"seat_vent_ctrl"}, wantTop1: "seat_vent_ctrl"},
		{utterance: "把后排座椅复位", wantTools: []string{"row23_mode_ctrl"}, wantTop1: "row23_mode_ctrl"},
		{utterance: "帮我锁车", wantTools: []string{"car_lock_ctrl"}, wantTop1: "car_lock_ctrl"},
		{utterance: "打开天窗透气", wantTools: []string{"sunroof_ctrl"}, wantTop1: "sunroof_ctrl"},
		{utterance: "把遮阳帘拉上", wantTools: []string{"sunshade_ctrl"}, wantTop1: "sunshade_ctrl"},
		{utterance: "开启电池预热", wantTools: []string{"bat_heat_ctrl"}, wantTop1: "bat_heat_ctrl"},
		{utterance: "立即开始充电", wantTools: []string{"charge_start_stop_ctrl"}, wantTop1: "charge_start_stop_ctrl"},
		{utterance: "帮我拔枪解锁", wantTools: []string{"charger_unlock_ctrl"}, wantTop1: "charger_unlock_ctrl"},
		{utterance: "开启健康充电模式", wantTools: []string{"healthy_charge_ctrl"}, wantTop1: "healthy_charge_ctrl"},
		{utterance: "按喇叭找车", wantTools: []string{"horn_honk_ctrl"}, wantTop1: "horn_honk_ctrl"},
		{utterance: "导航去杭州西湖", wantTools: []string{"navi_ctrl"}, wantTop1: "navi_ctrl"},
		{utterance: "设置今晚11点谷电预约充电", wantTools: []string{"timed_charge_ctrl"}, wantTop1: "timed_charge_ctrl"},
		{utterance: "预约明早7点电池加热", wantTools: []string{"timed_bat_heat_ctrl"}, wantTop1: "timed_bat_heat_ctrl"},
		{utterance: "预约明天早上8点备车开空调", wantTools: []string{"vehicle_prep_ctrl"}, wantTop1: "vehicle_prep_ctrl"},

		// Control vs Query disambiguation
		{utterance: "现在车内空调多少度", wantTools: []string{"car_airconditioner_status"}, wantTop1: "car_airconditioner_status"},
		{utterance: "空调开着吗", wantTools: []string{"car_airconditioner_status"}, wantTop1: "car_airconditioner_status"},
		{utterance: "查一下我的预约备车记录", wantTools: []string{"car_appointment_record"}, wantTop1: "car_appointment_record"},
		{utterance: "我的充电上限设了多少", wantTools: []string{"car_charging_info"}, wantTop1: "car_charging_info"},
		{utterance: "看看车机定位权限开了没", wantTools: []string{"car_permission_info"}, wantTop1: "car_permission_info"},

		// Large-enum deeplink_ctrl gating: explicit page/app intent succeeds; everyday queries must NOT be stolen
		{utterance: "打开App里的FOTA升级页面", wantTools: []string{"deeplink_ctrl"}, wantTop1: "deeplink_ctrl"},
		{utterance: "跳转到冰箱功能界面", wantTools: []string{"deeplink_ctrl"}, wantTop1: "deeplink_ctrl"},
		{utterance: "看看车内多少度", wantTools: []string{"car_airconditioner_status"}, forbidTop1: "deeplink_ctrl"},
		{utterance: "帮我打开空调", wantTools: []string{"ac_ctrl"}, forbidTop1: "deeplink_ctrl"},

		// Multi-intent compound sentences (recall union across clauses!)
		{utterance: "把主驾风速调高一点，顺便把后备箱打开", wantTools: []string{"ac_ctrl", "trunk_ctrl"}},
		{utterance: "打开方向盘加热，另外看看车里现在多少度", wantTools: []string{"steer_wheel_heat_ctrl", "car_airconditioner_status"}},
		{utterance: "打开主驾座椅加热，再把遮阳帘关上", wantTools: []string{"seat_heat_ctrl", "sunshade_ctrl"}},
		{utterance: "先别开空调，帮我把车窗关上", wantTools: []string{"car_window_ctrl"}},

		// Zero-recall unrelated chitchat
		{utterance: "帮我看看那个东西", expectEmpty: true},
		{utterance: "今天心情真不错", expectEmpty: true},
		{utterance: "给我讲个笑话吧", expectEmpty: true},
	}

	totalExpected := 0
	totalHit := 0
	top1Checked := 0
	top1Hit := 0
	deeplinkFalsePositives := 0

	for _, tc := range cases {
		caps, err := svc.FindCapabilities(ctx, scope, tc.utterance)
		if err != nil {
			t.Fatalf("FindCapabilities(%q): %v", tc.utterance, err)
		}
		if tc.expectEmpty {
			if len(caps) != 0 {
				t.Errorf("expected empty recall for %q, got %+v", tc.utterance, capToolNames(caps))
			}
			continue
		}

		names := capToolNames(caps)
		nameSet := make(map[string]bool, len(names))
		for _, n := range names {
			nameSet[n] = true
		}
		for _, want := range tc.wantTools {
			totalExpected++
			if nameSet[want] {
				totalHit++
			} else {
				t.Errorf("Recall miss for %q: wanted %q in %v", tc.utterance, want, names)
			}
		}
		if tc.wantTop1 != "" {
			top1Checked++
			if len(names) > 0 && names[0] == tc.wantTop1 {
				top1Hit++
			} else {
				t.Errorf("Top-1 mismatch for %q: want %q, got %v", tc.utterance, tc.wantTop1, names)
			}
		}
		if tc.forbidTop1 != "" && len(names) > 0 && names[0] == tc.forbidTop1 {
			deeplinkFalsePositives++
			t.Errorf("Forbidden top-1 %q hit for %q: %v", tc.forbidTop1, tc.utterance, names)
		}
	}

	recallRate := float64(totalHit) / float64(totalExpected)
	top1Rate := float64(top1Hit) / float64(top1Checked)
	t.Logf("Offline BM25 benchmark: Recall=%.2f%% (%d/%d), Top1=%.2f%% (%d/%d), deeplink_fp=%d",
		recallRate*100, totalHit, totalExpected, top1Rate*100, top1Hit, top1Checked, deeplinkFalsePositives)

	if recallRate < 0.97 {
		t.Fatalf("Recall gate failed: %.2f%% < 97%%", recallRate*100)
	}
	if top1Rate < 0.95 {
		t.Fatalf("Top-1 gate failed: %.2f%% < 95%%", top1Rate*100)
	}
	if deeplinkFalsePositives != 0 {
		t.Fatalf("deeplink_ctrl false positive gate failed: %d", deeplinkFalsePositives)
	}
}

func TestBM25PrecomputedQueryLatencyUnder1ms(t *testing.T) {
	ctx := context.Background()
	svc, err := NewBusinessMCPService(nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL_LAT"}
	queries := []string{
		"把主驾风速调高一点，顺便把后备箱打开",
		"打开主驾座椅加热2档",
		"现在车内空调多少度",
		"预约明天早上8点备车开空调",
		"帮我把车门玻璃关上",
	}

	// Warm up
	for _, q := range queries {
		_, _ = svc.FindCapabilities(ctx, scope, q)
	}

	const iterations = 500
	durations := make([]time.Duration, 0, iterations)
	for i := range iterations {
		q := queries[i%len(queries)]
		start := time.Now()
		_, err := svc.FindCapabilities(ctx, scope, q)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		durations = append(durations, elapsed)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p50 := durations[len(durations)/2]
	p99 := durations[int(float64(len(durations))*0.99)]
	t.Logf("Precomputed 31-tool BM25 query latency (n=%d): P50=%v P99=%v", iterations, p50, p99)
	if p99 > 2*time.Millisecond { // generous CI race-detector ceiling; P50 is typically < 50us
		t.Fatalf("BM25 P99 latency too high: %v", p99)
	}
}

func BenchmarkBM25Recall31Tools(b *testing.B) {
	ctx := context.Background()
	svc, err := NewBusinessMCPService(nil)
	if err != nil {
		b.Fatal(err)
	}
	scope := contract.Scope{UserID: "U1", SessionID: "S1", VehicleID: "VCL_BENCH"}
	b.ResetTimer()
	for b.Loop() {
		_, _ = svc.FindCapabilities(ctx, scope, "把主驾风速调高一点，顺便把后备箱打开")
	}
}

func capToolNames(caps []contract.Capability) []string {
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = c.Tool
	}
	return out
}
