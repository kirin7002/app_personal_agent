package contract

import "time"

const (
	CreateVehicleTask         = "create_vehicle_task"
	ListVehicleTasks          = "list_vehicle_tasks"
	DispatchVehicleCommand    = "dispatch_vehicle_command"
	GetVehicleStatus          = "get_vehicle_status"
	SearchVehicleManual       = "search_vehicle_manual"
	SearchVehicleCapabilities = "search_vehicle_capabilities"
)

// TaskMutation is shared by create (including partial completion) and dispatch.
type TaskMutation struct {
	Operation
	TaskID string         `json:"task_id,omitempty"`
	Params map[string]any `json:"params"`
}

type TaskResult struct {
	TaskID        string     `json:"task_id"`
	CommandID     string     `json:"command_id,omitempty"`
	Status        Status     `json:"status"`
	MissingFields []string   `json:"missing_fields,omitempty"`
	Question      string     `json:"question,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
}

type ListTasksInput struct {
	Status []Status `json:"status,omitempty"`
	Cursor string   `json:"cursor,omitempty"`
}

type ListTasksResult struct {
	Tasks      []TaskResult `json:"tasks"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

type VehicleStatusInput struct {
	Operation
	Params map[string]any `json:"params"`
}

type Signal struct {
	Name         string     `json:"name"`
	Value        any        `json:"value"`
	SampledAt    *time.Time `json:"sampled_at"`
	Availability string     `json:"availability"`
}

type VehicleStatusResult struct {
	Signals []Signal `json:"signals"`
}

type SearchManualInput struct {
	Query   string `json:"query"`
	TopicID string `json:"topic_id,omitempty"`
}

type ManualExcerpt struct {
	TopicID string `json:"topic_id"`
	Section string `json:"section"`
	Text    string `json:"text"`
	Source  string `json:"source"`
	Version string `json:"version"`
}

type SearchManualResult struct {
	Excerpts []ManualExcerpt `json:"excerpts"`
}

type SearchCapabilitiesInput struct {
	Query string `json:"query"`
}

type SearchCapabilitiesResult struct {
	Capabilities []Capability `json:"capabilities"`
}
