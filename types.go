package pisdk

import (
	"encoding/json"
	"fmt"
)

// StreamingBehavior controls how prompt messages are queued while the agent is already streaming.
type StreamingBehavior string

const (
	StreamingBehaviorSteer    StreamingBehavior = "steer"
	StreamingBehaviorFollowUp StreamingBehavior = "followUp"
)

// ThinkingLevel controls reasoning depth for models that support thinking tokens.
type ThinkingLevel string

const (
	ThinkingOff     ThinkingLevel = "off"
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
)

// QueueMode controls steering/follow-up queue behavior.
type QueueMode string

const (
	QueueModeAll        QueueMode = "all"
	QueueModeOneAtATime QueueMode = "one-at-a-time"
)

// ImageContent matches Pi RPC image payloads.
type ImageContent struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// MessageOptions are shared by steer/follow_up commands.
type MessageOptions struct {
	Images []ImageContent `json:"images,omitempty"`
}

// PromptOptions configures prompt command behavior.
type PromptOptions struct {
	Images            []ImageContent    `json:"images,omitempty"`
	StreamingBehavior StreamingBehavior `json:"streamingBehavior,omitempty"`
}

// ModelCost tracks model pricing metadata.
type ModelCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// Model describes a configured LLM model.
type Model struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	API           string     `json:"api"`
	Provider      string     `json:"provider"`
	BaseURL       string     `json:"baseUrl,omitempty"`
	Reasoning     bool       `json:"reasoning"`
	Input         []string   `json:"input,omitempty"`
	ContextWindow int        `json:"contextWindow"`
	MaxTokens     int        `json:"maxTokens"`
	Cost          *ModelCost `json:"cost,omitempty"`
}

// AgentMessage is the raw message shape from the Pi agent.
type AgentMessage map[string]any

// SessionState mirrors get_state RPC data.
type SessionState struct {
	Model                 *Model        `json:"model"`
	ThinkingLevel         ThinkingLevel `json:"thinkingLevel"`
	IsStreaming           bool          `json:"isStreaming"`
	IsCompacting          bool          `json:"isCompacting"`
	SteeringMode          QueueMode     `json:"steeringMode"`
	FollowUpMode          QueueMode     `json:"followUpMode"`
	SessionFile           string        `json:"sessionFile,omitempty"`
	SessionID             string        `json:"sessionId"`
	SessionName           string        `json:"sessionName,omitempty"`
	AutoCompactionEnabled bool          `json:"autoCompactionEnabled"`
	MessageCount          int           `json:"messageCount"`
	PendingMessageCount   int           `json:"pendingMessageCount"`
}

// ModelCycleResult mirrors cycle_model RPC data.
type ModelCycleResult struct {
	Model         Model         `json:"model"`
	ThinkingLevel ThinkingLevel `json:"thinkingLevel"`
	IsScoped      bool          `json:"isScoped"`
}

// CompactionResult mirrors compact RPC data.
type CompactionResult struct {
	Summary          string         `json:"summary"`
	FirstKeptEntryID string         `json:"firstKeptEntryId"`
	TokensBefore     int            `json:"tokensBefore"`
	Details          map[string]any `json:"details"`
}

// BashResult mirrors bash RPC data.
type BashResult struct {
	Output         string `json:"output"`
	ExitCode       int    `json:"exitCode"`
	Cancelled      bool   `json:"cancelled"`
	Truncated      bool   `json:"truncated"`
	FullOutputPath string `json:"fullOutputPath,omitempty"`
}

// SessionTokens mirrors get_session_stats token metrics.
type SessionTokens struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	Total      int `json:"total"`
}

// SessionStats mirrors get_session_stats RPC data.
type SessionStats struct {
	SessionFile       string        `json:"sessionFile"`
	SessionID         string        `json:"sessionId"`
	UserMessages      int           `json:"userMessages"`
	AssistantMessages int           `json:"assistantMessages"`
	ToolCalls         int           `json:"toolCalls"`
	ToolResults       int           `json:"toolResults"`
	TotalMessages     int           `json:"totalMessages"`
	Tokens            SessionTokens `json:"tokens"`
	Cost              float64       `json:"cost"`
}

// SessionActionResult mirrors new_session/switch_session result payloads.
type SessionActionResult struct {
	Cancelled bool `json:"cancelled"`
}

// ForkResult mirrors fork RPC data.
type ForkResult struct {
	Text      string `json:"text"`
	Cancelled bool   `json:"cancelled"`
}

// ForkMessage mirrors get_fork_messages entry payloads.
type ForkMessage struct {
	EntryID string `json:"entryId"`
	Text    string `json:"text"`
}

// CommandDescriptor mirrors get_commands payload items.
type CommandDescriptor struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`
	Location    string `json:"location,omitempty"`
	Path        string `json:"path,omitempty"`
}

// AssistantOutcome captures the latest assistant message summary.
type AssistantOutcome struct {
	Text         string
	ErrorMessage string
	StopReason   string
	Message      AgentMessage
}

// Event is a raw RPC event envelope (non-response JSON line).
type Event struct {
	Type string
	Raw  json.RawMessage
}

// Decode unmarshals the event JSON into a concrete struct.
func (e Event) Decode(v any) error {
	return json.Unmarshal(e.Raw, v)
}

// MessageUpdateEvent is a typed helper for message_update events.
type MessageUpdateEvent struct {
	Type                  string                `json:"type"`
	Message               AgentMessage          `json:"message"`
	AssistantMessageEvent AssistantMessageEvent `json:"assistantMessageEvent"`
}

// AssistantMessageEvent captures common streaming delta fields.
type AssistantMessageEvent struct {
	Type         string       `json:"type"`
	ContentIndex int          `json:"contentIndex,omitempty"`
	Delta        string       `json:"delta,omitempty"`
	Reason       string       `json:"reason,omitempty"`
	ToolCall     AgentMessage `json:"toolCall,omitempty"`
}

// CommandError is returned when Pi responds with success=false.
type CommandError struct {
	Command string
	Message string
}

func (e *CommandError) Error() string {
	if e.Command == "" {
		return fmt.Sprintf("pi command failed: %s", e.Message)
	}
	return fmt.Sprintf("pi command %q failed: %s", e.Command, e.Message)
}
