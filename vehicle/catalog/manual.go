package catalog

import (
	"context"
	"strings"

	"github.com/kirin7002/app_personal_agent/contract"
)

// ManualEntry stores a versioned, model-scoped vehicle manual excerpt.
type ManualEntry struct {
	Model    string
	Version  string
	TopicID  string
	Section  string
	Text     string
	Source   string
	Keywords []string
}

func defaultManualEntries() []ManualEntry {
	return []ManualEntry{
		{
			Model:    "EV-SUV-2026",
			Version:  DefaultCatalogVersion,
			TopicID:  "window.auto_close",
			Section:  "车门与车窗 · 锁车自动关窗",
			Text:     "在中控屏进入「车辆设置 → 门窗与锁 → 锁车自动关窗」，开启后锁车时四扇车窗将自动升至关闭位置。",
			Source:   "owner-manual-EV-SUV-2026-v2026.1#ch3.2",
			Keywords: []string{"自动关窗", "锁车关窗", "车窗设置", "关窗在哪里设置", "window auto close"},
		},
		{
			Model:    "EV-SUV-2026",
			Version:  DefaultCatalogVersion,
			TopicID:  "window.pinch_protection",
			Section:  "车门与车窗 · 车窗防夹功能",
			Text:     "四门电动车窗均具备防夹保护，自动升窗遇阻时会自动回退。若防夹失效，可长按升窗键 5 秒完成车窗初始化。",
			Source:   "owner-manual-EV-SUV-2026-v2026.1#ch3.3",
			Keywords: []string{"防夹", "车窗初始化", "升窗回退"},
		},
		{
			Model:    "EV-SUV-2026",
			Version:  DefaultCatalogVersion,
			TopicID:  "hvac.auto_climate",
			Section:  "空调与舒适 · 自动空调与温区控制",
			Text:     "自动空调支持 16°C 至 32°C 设定。可在「中控屏 → 空调」或通过语音直接开启空调并设置目标温度。",
			Source:   "owner-manual-EV-SUV-2026-v2026.1#ch5.1",
			Keywords: []string{"空调设置", "自动空调", "温区", "温度范围"},
		},
		{
			Model:    "EV-SEDAN-2025",
			Version:  "2025.3",
			TopicID:  "window.auto_close",
			Section:  "车窗控制 · 遥控落锁关窗",
			Text:     "2025 款轿车请在「设置 → 车身控制 → 遥控升窗」中开启落锁自动关窗。",
			Source:   "owner-manual-EV-SEDAN-2025-v2025.3#ch2.4",
			Keywords: []string{"自动关窗", "遥控升窗", "锁车关窗"},
		},
	}
}

func (s *Service) AddManualEntry(entry ManualEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manuals = append(s.manuals, entry)
}

// SearchManual retrieves excerpts strictly matching the vehicle's model and manual version.
func (s *Service) SearchManual(_ context.Context, scope contract.Scope, input contract.SearchManualInput) (contract.SearchManualResult, error) {
	if !scope.Valid() {
		return contract.SearchManualResult{}, &contract.BusinessError{
			Code:    contract.TurnContextInvalid,
			Message: "invalid scope",
		}
	}
	query := strings.TrimSpace(input.Query)
	topicID := strings.TrimSpace(input.TopicID)
	if query == "" && topicID == "" {
		return contract.SearchManualResult{}, &contract.BusinessError{
			Code:    contract.InvalidArgument,
			Message: "query or topic_id is required",
		}
	}

	s.mu.RLock()
	profile, ok := s.profiles[scope.VehicleID]
	if !ok {
		profile = DefaultProfile(scope.VehicleID)
	}
	var scoped []ManualEntry
	for _, entry := range s.manuals {
		if entry.Model == profile.Model && entry.Version == profile.ManualVersion {
			scoped = append(scoped, entry)
		}
	}
	s.mu.RUnlock()

	if len(scoped) == 0 {
		return contract.SearchManualResult{Excerpts: []contract.ManualExcerpt{}}, nil
	}

	if topicID != "" {
		var exact []contract.ManualExcerpt
		for _, e := range scoped {
			if e.TopicID == topicID {
				exact = append(exact, toManualExcerpt(e))
			}
		}
		if len(exact) > 0 {
			return contract.SearchManualResult{Excerpts: exact}, nil
		}
	}

	if query == "" {
		return contract.SearchManualResult{Excerpts: []contract.ManualExcerpt{}}, nil
	}

	corpus := make([]string, len(scoped))
	for i, e := range scoped {
		corpus[i] = e.TopicID + " " + e.Section + " " + e.Text + " " + strings.Join(e.Keywords, " ")
	}
	hits := buildBM25Index(corpus).search(query)
	excerpts := make([]contract.ManualExcerpt, 0, min(len(hits), 3))
	for _, h := range hits[:min(len(hits), 3)] {
		excerpts = append(excerpts, toManualExcerpt(scoped[h.id]))
	}
	return contract.SearchManualResult{Excerpts: excerpts}, nil
}

func toManualExcerpt(e ManualEntry) contract.ManualExcerpt {
	return contract.ManualExcerpt{
		TopicID: e.TopicID,
		Section: e.Section,
		Text:    e.Text,
		Source:  e.Source,
		Version: e.Version,
	}
}
