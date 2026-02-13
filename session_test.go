package pisdk

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPromptStreamsMessageUpdate(t *testing.T) {
	s := newTestSession(t)

	eventCh := make(chan Event, 1)
	unsubscribe := s.Subscribe(func(event Event) {
		if event.Type == "message_update" {
			eventCh <- event
		}
	})
	defer unsubscribe()

	if err := s.Prompt(context.Background(), "hello", nil); err != nil {
		t.Fatalf("prompt failed: %v", err)
	}

	select {
	case event := <-eventCh:
		var update MessageUpdateEvent
		if err := event.Decode(&update); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if update.AssistantMessageEvent.Type != "text_delta" {
			t.Fatalf("unexpected event type: %s", update.AssistantMessageEvent.Type)
		}
		if update.AssistantMessageEvent.Delta != "hello from fake pi" {
			t.Fatalf("unexpected delta: %q", update.AssistantMessageEvent.Delta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message_update event")
	}
}

func TestGetStateAndCycleModel(t *testing.T) {
	s := newTestSession(t)

	state, err := s.GetState(context.Background())
	if err != nil {
		t.Fatalf("get_state failed: %v", err)
	}
	if !state.AutoCompactionEnabled {
		t.Fatal("expected auto compaction to be enabled")
	}
	if state.SessionID != "session-123" {
		t.Fatalf("unexpected session id: %s", state.SessionID)
	}

	cycle, err := s.CycleModel(context.Background())
	if err != nil {
		t.Fatalf("cycle_model failed: %v", err)
	}
	if cycle != nil {
		t.Fatal("expected nil cycle result")
	}
}

func TestSetModelCommandError(t *testing.T) {
	s := newTestSession(t)

	_, err := s.SetModel(context.Background(), "anthropic", "missing")
	if err == nil {
		t.Fatal("expected command error")
	}

	cmdErr, ok := err.(*CommandError)
	if !ok {
		t.Fatalf("expected CommandError, got %T", err)
	}
	if cmdErr.Command != "set_model" {
		t.Fatalf("unexpected command in error: %s", cmdErr.Command)
	}
	lastErr := s.LastErrorMessage()
	if lastErr == nil || *lastErr != "model not found" {
		t.Fatalf("unexpected last error message: %v", lastErr)
	}
}

func TestGetAvailableModels(t *testing.T) {
	s := newTestSession(t)

	models, err := s.GetAvailableModels(context.Background())
	if err != nil {
		t.Fatalf("get_available_models failed: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("unexpected model count: %d", len(models))
	}
	if models[0].Provider != "anthropic" {
		t.Fatalf("unexpected provider: %s", models[0].Provider)
	}
}

func TestGetLastAssistantOutcome(t *testing.T) {
	s := newTestSession(t)

	outcome, err := s.GetLastAssistantOutcome(context.Background())
	if err != nil {
		t.Fatalf("GetLastAssistantOutcome failed: %v", err)
	}
	if outcome == nil {
		t.Fatal("expected assistant outcome")
	}
	if outcome.ErrorMessage != "model quota exceeded" {
		t.Fatalf("unexpected error message: %q", outcome.ErrorMessage)
	}
	if outcome.Text != "" {
		t.Fatalf("expected empty text, got: %q", outcome.Text)
	}
	if outcome.StopReason != "error" {
		t.Fatalf("unexpected stop reason: %q", outcome.StopReason)
	}

	outcomeSince, index, err := s.GetLatestAssistantOutcomeSince(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetLatestAssistantOutcomeSince failed: %v", err)
	}
	if outcomeSince == nil {
		t.Fatal("expected assistant outcome since index")
	}
	if index != 1 {
		t.Fatalf("unexpected index: %d", index)
	}
	if outcomeSince.ErrorMessage != "model quota exceeded" {
		t.Fatalf("unexpected error message since index: %q", outcomeSince.ErrorMessage)
	}
}

func TestPromptLateFailureResponse(t *testing.T) {
	s := newTestSession(t)

	err := s.Prompt(context.Background(), "fail-after-ack", nil)
	if err == nil {
		t.Fatal("expected prompt to fail")
	}

	cmdErr, ok := err.(*CommandError)
	if !ok {
		t.Fatalf("expected CommandError, got %T", err)
	}
	if cmdErr.Command != "prompt" {
		t.Fatalf("unexpected command in error: %s", cmdErr.Command)
	}
	if cmdErr.Message != "No API key found for opencode." {
		t.Fatalf("unexpected prompt late failure message: %q", cmdErr.Message)
	}
}

func newTestSession(t *testing.T) *AgentSession {
	t.Helper()

	result, err := CreateAgentSession(context.Background(), CreateAgentSessionOptions{
		BinaryPath:                os.Args[0],
		BinaryArgs:                []string{"-test.run=TestHelperProcess", "--"},
		DisableSessionPersistence: true,
		Env:                       []string{"GO_WANT_HELPER_PROCESS=1"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := result.Session.Close(); closeErr != nil {
			t.Fatalf("close session: %v", closeErr)
		}
	})

	return result.Session
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	runFakeRPC()
	os.Exit(0)
}

func runFakeRPC() {
	appendArgsLog()

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024), 1024*1024)

	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()

	write := func(v any) {
		bytes, _ := json.Marshal(v)
		_, _ = writer.Write(bytes)
		_, _ = writer.WriteString("\n")
		_ = writer.Flush()
	}

	for scanner.Scan() {
		raw := append([]byte(nil), scanner.Bytes()...)
		var command map[string]any
		if err := json.Unmarshal(raw, &command); err != nil {
			continue
		}

		commandType, _ := command["type"].(string)
		id, _ := command["id"].(string)
		appendCommandLog(raw)

		switch commandType {
		case "prompt":
			message, _ := command["message"].(string)
			if message == "fail-after-ack" {
				write(map[string]any{"type": "response", "id": id, "command": commandType, "success": true})
				write(map[string]any{
					"type":    "response",
					"id":      id,
					"command": commandType,
					"success": false,
					"error":   "No API key found for opencode.",
				})
				continue
			}

			write(map[string]any{"type": "response", "id": id, "command": commandType, "success": true})
			if message != "no-agent-end" {
				write(map[string]any{
					"type":    "message_update",
					"message": map[string]any{"role": "assistant"},
					"assistantMessageEvent": map[string]any{
						"type":  "text_delta",
						"delta": "hello from fake pi",
					},
				})
				write(map[string]any{"type": "agent_end"})
			}
		case "get_state":
			write(map[string]any{
				"type":    "response",
				"id":      id,
				"command": commandType,
				"success": true,
				"data": map[string]any{
					"model": map[string]any{
						"id":            "claude-sonnet-4-20250514",
						"name":          "Claude Sonnet 4",
						"api":           "anthropic-messages",
						"provider":      "anthropic",
						"reasoning":     true,
						"contextWindow": 200000,
						"maxTokens":     16384,
					},
					"thinkingLevel":         "medium",
					"isStreaming":           false,
					"isCompacting":          false,
					"steeringMode":          "all",
					"followUpMode":          "one-at-a-time",
					"sessionFile":           "/tmp/session.jsonl",
					"sessionId":             "session-123",
					"autoCompactionEnabled": true,
					"messageCount":          3,
					"pendingMessageCount":   0,
				},
			})
		case "set_model":
			if modelID, _ := command["modelId"].(string); modelID == "missing" {
				write(map[string]any{
					"type":    "response",
					"id":      id,
					"command": commandType,
					"success": false,
					"error":   "model not found",
				})
				continue
			}
			write(map[string]any{
				"type":    "response",
				"id":      id,
				"command": commandType,
				"success": true,
				"data": map[string]any{
					"id":            "claude-sonnet-4-20250514",
					"name":          "Claude Sonnet 4",
					"api":           "anthropic-messages",
					"provider":      "anthropic",
					"reasoning":     true,
					"contextWindow": 200000,
					"maxTokens":     16384,
				},
			})
		case "cycle_model":
			write(map[string]any{"type": "response", "id": id, "command": commandType, "success": true, "data": nil})
		case "get_available_models":
			write(map[string]any{
				"type":    "response",
				"id":      id,
				"command": commandType,
				"success": true,
				"data": map[string]any{
					"models": []map[string]any{{
						"id":            "claude-sonnet-4-20250514",
						"name":          "Claude Sonnet 4",
						"api":           "anthropic-messages",
						"provider":      "anthropic",
						"reasoning":     true,
						"contextWindow": 200000,
						"maxTokens":     16384,
					}},
				},
			})
		case "get_messages":
			write(map[string]any{
				"type":    "response",
				"id":      id,
				"command": commandType,
				"success": true,
				"data": map[string]any{
					"messages": []map[string]any{
						{
							"role": "user",
							"content": []map[string]any{
								{"type": "text", "text": "hello"},
							},
						},
						{
							"role":         "assistant",
							"content":      []map[string]any{},
							"stopReason":   "error",
							"errorMessage": "model quota exceeded",
						},
					},
				},
			})
		case "get_test_env":
			write(map[string]any{
				"type":    "response",
				"id":      id,
				"command": commandType,
				"success": true,
				"data": map[string]any{
					"piCodingAgentDir": os.Getenv("PI_CODING_AGENT_DIR"),
					"piAgentDir":       os.Getenv("PI_AGENT_DIR"),
				},
			})
		default:
			write(map[string]any{"type": "response", "id": id, "command": commandType, "success": true})
		}
	}
}

func appendCommandLog(raw []byte) {
	path := strings.TrimSpace(os.Getenv("COMMAND_LOG_PATH"))
	if path == "" {
		return
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()

	_, _ = f.Write(raw)
	_, _ = f.WriteString("\n")
}

func appendArgsLog() {
	path := strings.TrimSpace(os.Getenv("ARGS_LOG_PATH"))
	if path == "" {
		return
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()

	for _, arg := range os.Args {
		_, _ = f.WriteString(arg)
		_, _ = f.WriteString("\n")
	}
}
