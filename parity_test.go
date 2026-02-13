package pisdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWaitForIdle(t *testing.T) {
	s := newTestSession(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- s.WaitForIdle(ctx)
	}()

	if err := s.Prompt(context.Background(), "hello", nil); err != nil {
		t.Fatalf("prompt failed: %v", err)
	}

	if err := <-waitCh; err != nil {
		t.Fatalf("WaitForIdle failed: %v", err)
	}
}

func TestWaitForIdleTimeout(t *testing.T) {
	s := newTestSession(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- s.WaitForIdle(ctx)
	}()

	if err := s.Prompt(context.Background(), "no-agent-end", nil); err != nil {
		t.Fatalf("prompt failed: %v", err)
	}

	err := <-waitCh
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestRunPrintMode(t *testing.T) {
	s := newTestSession(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var text bytes.Buffer
	if err := RunPrintMode(ctx, s, &text, RunPrintModeOptions{InitialMessage: "hello"}); err != nil {
		t.Fatalf("RunPrintMode text failed: %v", err)
	}
	if strings.TrimSpace(text.String()) != "hello from fake pi" {
		t.Fatalf("unexpected text mode output: %q", text.String())
	}

	var raw bytes.Buffer
	if err := RunPrintMode(ctx, s, &raw, RunPrintModeOptions{Mode: PrintOutputModeJSON, InitialMessage: "hello"}); err != nil {
		t.Fatalf("RunPrintMode json failed: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(raw.String()), "\n")
	if len(lines) == 0 {
		t.Fatal("expected JSON lines output")
	}

	sawMessageUpdate := false
	sawAgentEnd := false
	for _, line := range lines {
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("invalid json line %q: %v", line, err)
		}
		if envelope.Type == "message_update" {
			sawMessageUpdate = true
		}
		if envelope.Type == "agent_end" {
			sawAgentEnd = true
		}
	}
	if !sawMessageUpdate {
		t.Fatal("expected message_update in json output")
	}
	if !sawAgentEnd {
		t.Fatal("expected agent_end in json output")
	}
}

func TestRawDataJSONMethods(t *testing.T) {
	s := newTestSession(t)
	ctx := context.Background()

	messages, err := s.GetMessagesJSON(ctx)
	if err != nil {
		t.Fatalf("GetMessagesJSON failed: %v", err)
	}
	var messagePayload struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(messages, &messagePayload); err != nil {
		t.Fatalf("decode messages json: %v", err)
	}
	if len(messagePayload.Messages) != 2 {
		t.Fatalf("unexpected messages payload: %s", messages)
	}

	models, err := s.GetAvailableModelsJSON(ctx)
	if err != nil {
		t.Fatalf("GetAvailableModelsJSON failed: %v", err)
	}
	var modelPayload map[string]any
	if err := json.Unmarshal(models, &modelPayload); err != nil {
		t.Fatalf("decode models json: %v", err)
	}
	if _, ok := modelPayload["models"]; !ok {
		t.Fatalf("unexpected models payload: %s", models)
	}

	stats, err := s.GetSessionStatsJSON(ctx)
	if err != nil {
		t.Fatalf("GetSessionStatsJSON failed: %v", err)
	}
	if string(stats) != "null" {
		t.Fatalf("expected null stats payload, got %s", stats)
	}

	commands, err := s.GetCommandsJSON(ctx)
	if err != nil {
		t.Fatalf("GetCommandsJSON failed: %v", err)
	}
	if string(commands) != "null" {
		t.Fatalf("expected null commands payload, got %s", commands)
	}

	forks, err := s.GetForkMessagesJSON(ctx)
	if err != nil {
		t.Fatalf("GetForkMessagesJSON failed: %v", err)
	}
	if string(forks) != "null" {
		t.Fatalf("expected null fork payload, got %s", forks)
	}
}

func TestUnsupportedRPCMethods(t *testing.T) {
	s := newTestSession(t)

	if err := s.NavigateTree(context.Background(), "entry", true, nil, false, nil); !errors.Is(err, ErrUnsupportedInRPCMode) {
		t.Fatalf("expected ErrUnsupportedInRPCMode from NavigateTree, got %v", err)
	}
	if err := s.SendHookMessage(context.Background(), `{}`, true); !errors.Is(err, ErrUnsupportedInRPCMode) {
		t.Fatalf("expected ErrUnsupportedInRPCMode from SendHookMessage, got %v", err)
	}
}

func TestCreateAgentSessionParityOptions(t *testing.T) {
	trueValue := true
	falseValue := false
	thinking := ThinkingLow
	steering := QueueModeAll
	followUp := QueueModeOneAtATime

	commandLog := filepath.Join(t.TempDir(), "commands.log")
	agentDir := filepath.Join(t.TempDir(), "agent")

	result, err := CreateAgentSession(context.Background(), CreateAgentSessionOptions{
		BinaryPath:     os.Args[0],
		BinaryArgs:     []string{"-test.run=TestHelperProcess", "--"},
		SessionManager: InMemorySessionManager(),
		SettingsManager: InMemorySettingsManager(Settings{
			AutoCompactionEnabled: &trueValue,
			AutoRetryEnabled:      &falseValue,
			ThinkingLevel:         &thinking,
			SteeringMode:          &steering,
			FollowUpMode:          &followUp,
		}),
		AgentDir: agentDir,
		Env: []string{
			"GO_WANT_HELPER_PROCESS=1",
			"COMMAND_LOG_PATH=" + commandLog,
		},
	})
	if err != nil {
		t.Fatalf("CreateAgentSession failed: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := result.Session.Close(); closeErr != nil {
			t.Fatalf("close session: %v", closeErr)
		}
	})

	var envPayload struct {
		PiCodingAgentDir string `json:"piCodingAgentDir"`
		PiAgentDir       string `json:"piAgentDir"`
	}
	if err := result.Session.Call(context.Background(), map[string]any{"type": "get_test_env"}, &envPayload); err != nil {
		t.Fatalf("get_test_env failed: %v", err)
	}
	if envPayload.PiCodingAgentDir != agentDir {
		t.Fatalf("expected PI_CODING_AGENT_DIR %q, got %q", agentDir, envPayload.PiCodingAgentDir)
	}
	if envPayload.PiAgentDir != agentDir {
		t.Fatalf("expected PI_AGENT_DIR %q, got %q", agentDir, envPayload.PiAgentDir)
	}

	logBytes, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatalf("read command log: %v", err)
	}

	expected := []string{
		"set_auto_compaction",
		"set_auto_retry",
		"set_thinking_level",
		"set_steering_mode",
		"set_follow_up_mode",
	}

	lines := strings.Split(strings.TrimSpace(string(logBytes)), "\n")
	if len(lines) < len(expected) {
		t.Fatalf("expected at least %d commands in log, got %d", len(expected), len(lines))
	}

	seen := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var command map[string]any
		if err := json.Unmarshal([]byte(line), &command); err != nil {
			t.Fatalf("decode command log line %q: %v", line, err)
		}
		typeName, _ := command["type"].(string)
		seen = append(seen, typeName)
	}

	for _, commandType := range expected {
		if !slices.Contains(seen, commandType) {
			t.Fatalf("expected command %q in startup sequence, saw %v", commandType, seen)
		}
	}

	if len(result.ExtensionsResult.Extensions) != 0 {
		t.Fatalf("expected no discovered extensions in test harness, got %d", len(result.ExtensionsResult.Extensions))
	}
}

func TestCreateAgentSessionRawSpawnArgs(t *testing.T) {
	argsLog := filepath.Join(t.TempDir(), "args.log")

	result, err := CreateAgentSession(context.Background(), CreateAgentSessionOptions{
		BinaryPath:     os.Args[0],
		RawSpawnArgs:   []string{"-test.run=TestHelperProcess", "--", "--raw-flag"},
		MaxLineBytes:   128 * 1024,
		ResourceLoader: &fakeResourceLoader{extensions: []ResourceEntry{{Path: "/tmp/ext", Location: ResourceLocationProject}}},
		Env: []string{
			"GO_WANT_HELPER_PROCESS=1",
			"ARGS_LOG_PATH=" + argsLog,
		},
	})
	if err != nil {
		t.Fatalf("CreateAgentSession failed: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := result.Session.Close(); closeErr != nil {
			t.Fatalf("close session: %v", closeErr)
		}
	})

	argsBytes, err := os.ReadFile(argsLog)
	if err != nil {
		for range 20 {
			time.Sleep(25 * time.Millisecond)
			argsBytes, err = os.ReadFile(argsLog)
			if err == nil {
				break
			}
		}
	}
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	if !slices.Contains(args, "--raw-flag") {
		t.Fatalf("expected raw arg in child process args, got %v", args)
	}
	if slices.Contains(args, "--mode") {
		t.Fatalf("did not expect default rpc args with RawSpawnArgs, got %v", args)
	}

	if result.Session.maxLineBytes != 128*1024 {
		t.Fatalf("unexpected max line bytes: %d", result.Session.maxLineBytes)
	}
	if len(result.ExtensionsResult.Extensions) != 1 {
		t.Fatalf("expected custom resource loader extension, got %d", len(result.ExtensionsResult.Extensions))
	}
}

func TestCreateAgentSessionDefaultInMemory(t *testing.T) {
	argsLog := filepath.Join(t.TempDir(), "args.log")

	result, err := CreateAgentSession(context.Background(), CreateAgentSessionOptions{
		BinaryPath: os.Args[0],
		BinaryArgs: []string{"-test.run=TestHelperProcess", "--"},
		Env: []string{
			"GO_WANT_HELPER_PROCESS=1",
			"ARGS_LOG_PATH=" + argsLog,
		},
	})
	if err != nil {
		t.Fatalf("CreateAgentSession failed: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := result.Session.Close(); closeErr != nil {
			t.Fatalf("close session: %v", closeErr)
		}
	})

	argsBytes, err := os.ReadFile(argsLog)
	if err != nil {
		for range 20 {
			time.Sleep(25 * time.Millisecond)
			argsBytes, err = os.ReadFile(argsLog)
			if err == nil {
				break
			}
		}
	}
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}

	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	if !slices.Contains(args, "--no-session") {
		t.Fatalf("expected --no-session by default for zig parity, got %v", args)
	}
}

func TestToolFactories(t *testing.T) {
	if ReadTool.Name != "read" {
		t.Fatalf("unexpected ReadTool: %+v", ReadTool)
	}
	if CreateReadTool("/tmp").Cwd != "/tmp" {
		t.Fatalf("unexpected read tool cwd")
	}

	coding := CreateCodingTools("/tmp/work")
	if len(coding) != 4 {
		t.Fatalf("unexpected coding tool count: %d", len(coding))
	}
	if coding[0].Name != "read" || coding[3].Name != "write" {
		t.Fatalf("unexpected coding tools: %+v", coding)
	}
}

func TestDefaultResourceLoaderReload(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "project")
	workDir := filepath.Join(projectRoot, "nested", "work")
	agentDir := filepath.Join(root, "agent")

	mustMkdirAll(t, filepath.Join(workDir, ".pi", "extensions"))
	mustMkdirAll(t, filepath.Join(workDir, ".pi", "skills", "skill-a"))
	mustMkdirAll(t, filepath.Join(workDir, ".pi", "prompts"))
	mustMkdirAll(t, filepath.Join(agentDir, "extensions"))
	mustMkdirAll(t, filepath.Join(agentDir, "skills", "skill-b"))
	mustMkdirAll(t, filepath.Join(agentDir, "prompts"))
	mustMkdirAll(t, workDir)

	mustWriteFile(t, filepath.Join(workDir, ".pi", "extensions", "project-ext.txt"), "x")
	mustWriteFile(t, filepath.Join(agentDir, "extensions", "user-ext.txt"), "y")
	mustWriteFile(t, filepath.Join(workDir, ".pi", "skills", "skill-a", "SKILL.md"), "# skill")
	mustWriteFile(t, filepath.Join(agentDir, "skills", "skill-b", "SKILL.md"), "# skill")
	mustWriteFile(t, filepath.Join(workDir, ".pi", "prompts", "project.md"), "# prompt")
	mustWriteFile(t, filepath.Join(agentDir, "prompts", "user.md"), "# prompt")
	mustWriteFile(t, filepath.Join(agentDir, "AGENTS.md"), "global")
	mustWriteFile(t, filepath.Join(workDir, "AGENTS.md"), "local")
	mustWriteFile(t, filepath.Join(projectRoot, "AGENTS.md"), "project")

	loader, err := NewDefaultResourceLoader(DefaultResourceLoaderOptions{
		Cwd:      workDir,
		AgentDir: agentDir,
	})
	if err != nil {
		t.Fatalf("NewDefaultResourceLoader failed: %v", err)
	}
	if err := loader.Reload(); err != nil {
		t.Fatalf("Reload failed: %v", err)
	}

	if len(loader.GetExtensions()) != 2 {
		t.Fatalf("expected 2 extensions, got %d", len(loader.GetExtensions()))
	}
	if len(loader.GetSkills()) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(loader.GetSkills()))
	}
	if len(loader.GetPrompts()) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(loader.GetPrompts()))
	}
	if len(loader.GetAgentsFiles()) != 3 {
		t.Fatalf("expected 3 AGENTS files, got %d", len(loader.GetAgentsFiles()))
	}
}

func TestDefaultAgentDir(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_AGENT_DIR", "")
	t.Setenv("HOME", "/tmp/test-home")

	got, err := defaultAgentDir()
	if err != nil {
		t.Fatalf("defaultAgentDir failed: %v", err)
	}
	if got != "/tmp/test-home/.pi/agent" {
		t.Fatalf("unexpected default agent dir: %q", got)
	}
}

type fakeResourceLoader struct {
	extensions []ResourceEntry
}

func (f *fakeResourceLoader) GetExtensions() []ResourceEntry {
	return append([]ResourceEntry(nil), f.extensions...)
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	mustMkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}
