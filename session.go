package pisdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrSessionClosed is returned when the underlying RPC subprocess is not available.
	ErrSessionClosed = errors.New("pi session is closed")
)

const defaultMaxLineBytes = 4 * 1024 * 1024

// CreateAgentSessionOptions controls process launch and default Pi RPC flags.
type CreateAgentSessionOptions struct {
	BinaryPath string

	// BinaryArgs are prepended before Pi RPC flags.
	// Useful for tests where BinaryPath is a wrapper executable.
	BinaryArgs []string

	// RawSpawnArgs replaces default RPC argument composition when provided.
	RawSpawnArgs []string

	// AdditionalArgs are appended after Pi RPC flags.
	AdditionalArgs []string

	// ExtraArgs are appended after AdditionalArgs.
	ExtraArgs []string

	Provider   string
	Model      string
	SessionDir string

	// DisableSessionPersistence adds --no-session.
	DisableSessionPersistence bool

	// SessionManager mirrors Zig behavior when set.
	SessionManager *SessionManager

	// SettingsManager applies startup settings after process launch.
	SettingsManager *SettingsManager

	// ResourceLoader can supply preloaded extension metadata.
	ResourceLoader ResourceLoader

	// AgentDir maps to PI_CODING_AGENT_DIR and PI_AGENT_DIR.
	AgentDir string

	Cwd string
	Env []string

	// Stderr receives subprocess stderr. Defaults to io.Discard.
	Stderr io.Writer

	// MaxLineBytes controls max JSON event line size. Defaults to 4MiB.
	MaxLineBytes int
}

// CreateAgentSessionResult mirrors the JS SDK surface where session creation returns a struct.
type CreateAgentSessionResult struct {
	Session              *AgentSession
	ExtensionsResult     LoadExtensionsResult
	ModelFallbackMessage *string
}

type rpcResponse struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// AgentSession is a headless RPC session backed by `pi --mode rpc`.
type AgentSession struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan rpcResponse

	subscribersMu sync.RWMutex
	subscribers   map[uint64]func(Event)

	requestSeq    uint64
	subscriberSeq uint64

	done chan struct{}

	stopOnce sync.Once

	terminalErrMu sync.Mutex
	terminalErr   error

	maxLineBytes int

	lastCommandErrMu      sync.Mutex
	lastCommandErrMessage string
}

// CreateAgentSession starts `pi --mode rpc` and returns a connected headless session.
func CreateAgentSession(ctx context.Context, opts CreateAgentSessionOptions) (*CreateAgentSessionResult, error) {
	binary := opts.BinaryPath
	if binary == "" {
		binary = "pi"
	}

	args := buildSessionArgs(opts)

	cmd := exec.CommandContext(ctx, binary, args...)
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}

	env := append([]string(nil), os.Environ()...)
	if len(opts.Env) > 0 {
		env = append(env, opts.Env...)
	}
	if opts.AgentDir != "" {
		env = setEnvVar(env, "PI_CODING_AGENT_DIR", opts.AgentDir)
		env = setEnvVar(env, "PI_AGENT_DIR", opts.AgentDir)
	}
	if len(env) > 0 {
		cmd.Env = env
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdout pipe: %w", err)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdin pipe: %w", err)
	}

	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	} else {
		cmd.Stderr = io.Discard
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start pi rpc process: %w", err)
	}

	session := &AgentSession{
		cmd:         cmd,
		stdin:       stdin,
		pending:     map[string]chan rpcResponse{},
		subscribers: map[uint64]func(Event){},
		done:        make(chan struct{}),
		maxLineBytes: func() int {
			if opts.MaxLineBytes > 0 {
				return opts.MaxLineBytes
			}
			return defaultMaxLineBytes
		}(),
	}

	go session.readLoop(stdout)
	go session.waitLoop()

	if opts.SettingsManager != nil {
		if err := applyStartupSettings(ctx, session, opts.SettingsManager.Settings); err != nil {
			_ = session.Close()
			return nil, err
		}
	}

	extensionsResult, err := resolveExtensionsResult(opts)
	if err != nil {
		_ = session.Close()
		return nil, err
	}

	return &CreateAgentSessionResult{
		Session:          session,
		ExtensionsResult: extensionsResult,
	}, nil
}

func buildSessionArgs(opts CreateAgentSessionOptions) []string {
	if len(opts.RawSpawnArgs) > 0 {
		return append([]string(nil), opts.RawSpawnArgs...)
	}

	args := make([]string, 0, len(opts.BinaryArgs)+10+len(opts.AdditionalArgs)+len(opts.ExtraArgs))
	args = append(args, opts.BinaryArgs...)
	args = append(args, "--mode", "rpc")

	if opts.Provider != "" {
		args = append(args, "--provider", opts.Provider)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}

	if opts.SessionManager != nil {
		switch opts.SessionManager.Mode {
		case "", SessionManagerModeInMemory:
			args = append(args, "--no-session")
		case SessionManagerModePersistent:
			sessionDir := opts.SessionManager.SessionDir
			if sessionDir == "" {
				sessionDir = opts.SessionDir
			}
			if sessionDir != "" {
				args = append(args, "--session-dir", sessionDir)
			}
		}
	} else {
		if opts.SessionDir != "" {
			args = append(args, "--session-dir", opts.SessionDir)
		} else if opts.DisableSessionPersistence || opts.SessionManager == nil {
			// Zig parity default is in-memory sessions when session manager is not set.
			args = append(args, "--no-session")
		}
	}

	args = append(args, opts.AdditionalArgs...)
	args = append(args, opts.ExtraArgs...)
	return args
}

func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func applyStartupSettings(ctx context.Context, session *AgentSession, settings Settings) error {
	if settings.AutoCompactionEnabled != nil {
		if err := session.SetAutoCompaction(ctx, *settings.AutoCompactionEnabled); err != nil {
			return err
		}
	}
	if settings.AutoRetryEnabled != nil {
		if err := session.SetAutoRetry(ctx, *settings.AutoRetryEnabled); err != nil {
			return err
		}
	}
	if settings.ThinkingLevel != nil {
		if err := session.SetThinkingLevel(ctx, *settings.ThinkingLevel); err != nil {
			return err
		}
	}
	if settings.SteeringMode != nil {
		if err := session.SetSteeringMode(ctx, *settings.SteeringMode); err != nil {
			return err
		}
	}
	if settings.FollowUpMode != nil {
		if err := session.SetFollowUpMode(ctx, *settings.FollowUpMode); err != nil {
			return err
		}
	}
	return nil
}

func resolveExtensionsResult(opts CreateAgentSessionOptions) (LoadExtensionsResult, error) {
	if opts.ResourceLoader != nil {
		return LoadExtensionsResult{
			Extensions: append([]ResourceEntry(nil), opts.ResourceLoader.GetExtensions()...),
		}, nil
	}

	loader, err := NewDefaultResourceLoader(DefaultResourceLoaderOptions{
		Cwd:      opts.Cwd,
		AgentDir: opts.AgentDir,
	})
	if err != nil {
		return LoadExtensionsResult{}, err
	}

	if err := loader.Reload(); err != nil {
		return LoadExtensionsResult{}, err
	}

	return LoadExtensionsResult{
		Extensions: loader.GetExtensions(),
	}, nil
}

func (s *AgentSession) waitLoop() {
	err := s.cmd.Wait()
	s.shutdown(err)
}

func (s *AgentSession) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	maxLineBytes := s.maxLineBytes
	if maxLineBytes <= 0 {
		maxLineBytes = defaultMaxLineBytes
	}
	scanner.Buffer(make([]byte, 1024), maxLineBytes)

	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)

		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			continue
		}

		if envelope.Type == "response" {
			var response rpcResponse
			if err := json.Unmarshal(line, &response); err != nil {
				continue
			}
			// Some Pi builds emit additional response frames (for example late prompt failures).
			// Forward all response frames to subscribers so callers can react to secondary errors.
			s.dispatchEvent(Event{Type: envelope.Type, Raw: line})
			s.dispatchResponse(response)
			continue
		}

		s.dispatchEvent(Event{Type: envelope.Type, Raw: line})
	}

	if err := scanner.Err(); err != nil {
		s.shutdown(err)
		return
	}
	s.shutdown(io.EOF)
}

func (s *AgentSession) shutdown(err error) {
	s.stopOnce.Do(func() {
		s.setTerminalErr(err)
		close(s.done)

		s.pendingMu.Lock()
		for _, ch := range s.pending {
			close(ch)
		}
		s.pending = nil
		s.pendingMu.Unlock()
	})
}

func (s *AgentSession) setTerminalErr(err error) {
	if err == nil {
		return
	}
	s.terminalErrMu.Lock()
	defer s.terminalErrMu.Unlock()
	if s.terminalErr == nil || (errors.Is(s.terminalErr, io.EOF) && !errors.Is(err, io.EOF)) {
		s.terminalErr = err
	}
}

// Done is closed when the RPC subprocess exits or the session is closed.
func (s *AgentSession) Done() <-chan struct{} {
	return s.done
}

// Err returns the terminal subprocess error, if any.
func (s *AgentSession) Err() error {
	s.terminalErrMu.Lock()
	defer s.terminalErrMu.Unlock()
	return s.terminalErr
}

// Close shuts down the RPC process and releases session resources.
func (s *AgentSession) Close() error {
	_ = s.stdin.Close()

	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		<-s.done
	}

	err := s.Err()
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (s *AgentSession) sessionClosedError() error {
	err := s.Err()
	if err == nil || errors.Is(err, io.EOF) {
		return ErrSessionClosed
	}
	return fmt.Errorf("%w: %v", ErrSessionClosed, err)
}

func (s *AgentSession) nextRequestID() string {
	id := atomic.AddUint64(&s.requestSeq, 1)
	return fmt.Sprintf("req-%d", id)
}

func (s *AgentSession) writeCommand(command map[string]any) error {
	payload, err := json.Marshal(command)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}
	payload = append(payload, '\n')

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if _, err := s.stdin.Write(payload); err != nil {
		return fmt.Errorf("write command: %w", err)
	}
	return nil
}

func (s *AgentSession) registerPending(id string) (chan rpcResponse, error) {
	ch := make(chan rpcResponse, 1)

	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.pending == nil {
		return nil, s.sessionClosedError()
	}
	s.pending[id] = ch
	return ch, nil
}

func (s *AgentSession) removePending(id string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.pending == nil {
		return
	}
	delete(s.pending, id)
}

func (s *AgentSession) dispatchResponse(response rpcResponse) {
	if response.ID == "" {
		return
	}

	s.pendingMu.Lock()
	if s.pending == nil {
		s.pendingMu.Unlock()
		return
	}
	ch, ok := s.pending[response.ID]
	if ok {
		delete(s.pending, response.ID)
	}
	s.pendingMu.Unlock()

	if ok {
		ch <- response
		close(ch)
	}
}

func (s *AgentSession) dispatchEvent(event Event) {
	s.subscribersMu.RLock()
	callbacks := make([]func(Event), 0, len(s.subscribers))
	for _, fn := range s.subscribers {
		callbacks = append(callbacks, fn)
	}
	s.subscribersMu.RUnlock()

	for _, fn := range callbacks {
		fn(event)
	}
}

func (s *AgentSession) callRPC(ctx context.Context, command map[string]any) (rpcResponse, error) {
	commandCopy := make(map[string]any, len(command)+1)
	for key, value := range command {
		commandCopy[key] = value
	}

	commandType, ok := commandCopy["type"].(string)
	if !ok || commandType == "" {
		return rpcResponse{}, errors.New("command must include non-empty type")
	}
	s.setLastCommandError("")

	select {
	case <-s.done:
		return rpcResponse{}, s.sessionClosedError()
	default:
	}

	id := s.nextRequestID()
	commandCopy["id"] = id

	responseCh, err := s.registerPending(id)
	if err != nil {
		return rpcResponse{}, err
	}

	if err := s.writeCommand(commandCopy); err != nil {
		s.removePending(id)
		return rpcResponse{}, err
	}

	select {
	case response, ok := <-responseCh:
		if !ok {
			return rpcResponse{}, s.sessionClosedError()
		}
		if !response.Success {
			s.setLastCommandError(response.Error)
			return rpcResponse{}, &CommandError{Command: response.Command, Message: response.Error}
		}
		return response, nil
	case <-ctx.Done():
		s.removePending(id)
		return rpcResponse{}, ctx.Err()
	case <-s.done:
		s.removePending(id)
		return rpcResponse{}, s.sessionClosedError()
	}
}

func (s *AgentSession) setLastCommandError(message string) {
	s.lastCommandErrMu.Lock()
	defer s.lastCommandErrMu.Unlock()
	s.lastCommandErrMessage = message
}

// LastErrorMessage returns the most recent RPC command failure message, if any.
func (s *AgentSession) LastErrorMessage() *string {
	s.lastCommandErrMu.Lock()
	defer s.lastCommandErrMu.Unlock()
	if s.lastCommandErrMessage == "" {
		return nil
	}
	value := s.lastCommandErrMessage
	return &value
}

func (s *AgentSession) decodeData(raw json.RawMessage, out any) error {
	if out == nil {
		return nil
	}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response data: %w", err)
	}
	return nil
}

// Subscribe registers a callback for all non-response JSON events.
func (s *AgentSession) Subscribe(listener func(Event)) func() {
	id := atomic.AddUint64(&s.subscriberSeq, 1)

	s.subscribersMu.Lock()
	s.subscribers[id] = listener
	s.subscribersMu.Unlock()

	return func() {
		s.subscribersMu.Lock()
		delete(s.subscribers, id)
		s.subscribersMu.Unlock()
	}
}

// Call executes a raw RPC command map and decodes response.data into out.
func (s *AgentSession) Call(ctx context.Context, command map[string]any, out any) error {
	resp, err := s.callRPC(ctx, command)
	if err != nil {
		return err
	}
	return s.decodeData(resp.Data, out)
}

// Prompt sends a prompt to the agent.
func (s *AgentSession) Prompt(ctx context.Context, message string, opts *PromptOptions) error {
	command := map[string]any{
		"type":    "prompt",
		"message": message,
	}
	if opts != nil {
		if len(opts.Images) > 0 {
			command["images"] = opts.Images
		}
		if opts.StreamingBehavior != "" {
			command["streamingBehavior"] = opts.StreamingBehavior
		}
	}

	lateErrorCh := make(chan rpcResponse, 1)
	unsubscribe := s.Subscribe(func(event Event) {
		if event.Type != "response" {
			return
		}

		var response rpcResponse
		if err := event.Decode(&response); err != nil {
			return
		}
		if response.Command != "prompt" || response.Success {
			return
		}

		select {
		case lateErrorCh <- response:
		default:
		}
	})
	defer unsubscribe()

	initialResponse, err := s.callRPC(ctx, command)
	if err != nil {
		return err
	}

	// Guard window for late prompt failures after an initial ack.
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()

	for {
		select {
		case response := <-lateErrorCh:
			if response.ID != initialResponse.ID {
				continue
			}
			return &CommandError{Command: response.Command, Message: response.Error}
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Steer queues a steering message to interrupt the current run.
func (s *AgentSession) Steer(ctx context.Context, message string, opts *MessageOptions) error {
	command := map[string]any{
		"type":    "steer",
		"message": message,
	}
	if opts != nil && len(opts.Images) > 0 {
		command["images"] = opts.Images
	}
	_, err := s.callRPC(ctx, command)
	return err
}

// FollowUp queues a message to run after the current agent run completes.
func (s *AgentSession) FollowUp(ctx context.Context, message string, opts *MessageOptions) error {
	command := map[string]any{
		"type":    "follow_up",
		"message": message,
	}
	if opts != nil && len(opts.Images) > 0 {
		command["images"] = opts.Images
	}
	_, err := s.callRPC(ctx, command)
	return err
}

// Abort aborts the current agent operation.
func (s *AgentSession) Abort(ctx context.Context) error {
	_, err := s.callRPC(ctx, map[string]any{"type": "abort"})
	return err
}

// WaitForIdle waits for the next agent_end event.
func (s *AgentSession) WaitForIdle(ctx context.Context) error {
	idleCh := make(chan struct{}, 1)
	unsubscribe := s.Subscribe(func(event Event) {
		if event.Type != "agent_end" {
			return
		}
		select {
		case idleCh <- struct{}{}:
		default:
		}
	})
	defer unsubscribe()

	select {
	case <-idleCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.sessionClosedError()
	}
}

// NewSession starts a fresh session, optionally tracking a parent session path.
func (s *AgentSession) NewSession(ctx context.Context, parentSession string) (SessionActionResult, error) {
	command := map[string]any{"type": "new_session"}
	if parentSession != "" {
		command["parentSession"] = parentSession
	}

	var data SessionActionResult
	resp, err := s.callRPC(ctx, command)
	if err != nil {
		return SessionActionResult{}, err
	}
	if err := s.decodeData(resp.Data, &data); err != nil {
		return SessionActionResult{}, err
	}
	return data, nil
}

// SwitchSession loads a specific session file.
func (s *AgentSession) SwitchSession(ctx context.Context, sessionPath string) (SessionActionResult, error) {
	var data SessionActionResult
	err := s.Call(ctx, map[string]any{"type": "switch_session", "sessionPath": sessionPath}, &data)
	if err != nil {
		return SessionActionResult{}, err
	}
	return data, nil
}

// GetState fetches current runtime/session state.
func (s *AgentSession) GetState(ctx context.Context) (SessionState, error) {
	var state SessionState
	err := s.Call(ctx, map[string]any{"type": "get_state"}, &state)
	if err != nil {
		return SessionState{}, err
	}
	return state, nil
}

// GetMessages fetches full conversation history.
func (s *AgentSession) GetMessages(ctx context.Context) ([]AgentMessage, error) {
	var data struct {
		Messages []AgentMessage `json:"messages"`
	}
	err := s.Call(ctx, map[string]any{"type": "get_messages"}, &data)
	if err != nil {
		return nil, err
	}
	return data.Messages, nil
}

// GetMessagesJSON returns raw `data` JSON for get_messages.
func (s *AgentSession) GetMessagesJSON(ctx context.Context) ([]byte, error) {
	return s.getRawDataJSON(ctx, "get_messages")
}

// SetModel switches to a specific provider/model pair.
func (s *AgentSession) SetModel(ctx context.Context, provider, modelID string) (Model, error) {
	var model Model
	err := s.Call(ctx, map[string]any{"type": "set_model", "provider": provider, "modelId": modelID}, &model)
	if err != nil {
		return Model{}, err
	}
	return model, nil
}

// CycleModel cycles to the next available model, nil if only one model is available.
func (s *AgentSession) CycleModel(ctx context.Context) (*ModelCycleResult, error) {
	var result *ModelCycleResult
	err := s.Call(ctx, map[string]any{"type": "cycle_model"}, &result)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetAvailableModels returns all configured models.
func (s *AgentSession) GetAvailableModels(ctx context.Context) ([]Model, error) {
	var data struct {
		Models []Model `json:"models"`
	}
	err := s.Call(ctx, map[string]any{"type": "get_available_models"}, &data)
	if err != nil {
		return nil, err
	}
	return data.Models, nil
}

// GetAvailableModelsJSON returns raw `data` JSON for get_available_models.
func (s *AgentSession) GetAvailableModelsJSON(ctx context.Context) ([]byte, error) {
	return s.getRawDataJSON(ctx, "get_available_models")
}

// SetThinkingLevel updates reasoning level.
func (s *AgentSession) SetThinkingLevel(ctx context.Context, level ThinkingLevel) error {
	_, err := s.callRPC(ctx, map[string]any{"type": "set_thinking_level", "level": level})
	return err
}

// CycleThinkingLevel cycles reasoning levels, nil if model has no thinking support.
func (s *AgentSession) CycleThinkingLevel(ctx context.Context) (*ThinkingLevel, error) {
	var data *struct {
		Level ThinkingLevel `json:"level"`
	}
	err := s.Call(ctx, map[string]any{"type": "cycle_thinking_level"}, &data)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	level := data.Level
	return &level, nil
}

// SetSteeringMode sets steer queue behavior.
func (s *AgentSession) SetSteeringMode(ctx context.Context, mode QueueMode) error {
	_, err := s.callRPC(ctx, map[string]any{"type": "set_steering_mode", "mode": mode})
	return err
}

// SetFollowUpMode sets follow-up queue behavior.
func (s *AgentSession) SetFollowUpMode(ctx context.Context, mode QueueMode) error {
	_, err := s.callRPC(ctx, map[string]any{"type": "set_follow_up_mode", "mode": mode})
	return err
}

// Compact triggers manual context compaction.
func (s *AgentSession) Compact(ctx context.Context, customInstructions string) (CompactionResult, error) {
	command := map[string]any{"type": "compact"}
	if customInstructions != "" {
		command["customInstructions"] = customInstructions
	}

	var result CompactionResult
	err := s.Call(ctx, command, &result)
	if err != nil {
		return CompactionResult{}, err
	}
	return result, nil
}

// AbortCompaction aborts compaction work (RPC alias to abort).
func (s *AgentSession) AbortCompaction(ctx context.Context) error {
	return s.Abort(ctx)
}

// SetAutoCompaction toggles automatic compaction.
func (s *AgentSession) SetAutoCompaction(ctx context.Context, enabled bool) error {
	_, err := s.callRPC(ctx, map[string]any{"type": "set_auto_compaction", "enabled": enabled})
	return err
}

// SetAutoRetry toggles automatic retries for transient errors.
func (s *AgentSession) SetAutoRetry(ctx context.Context, enabled bool) error {
	_, err := s.callRPC(ctx, map[string]any{"type": "set_auto_retry", "enabled": enabled})
	return err
}

// AbortRetry aborts an in-progress retry wait.
func (s *AgentSession) AbortRetry(ctx context.Context) error {
	_, err := s.callRPC(ctx, map[string]any{"type": "abort_retry"})
	return err
}

// Bash executes a shell command through Pi and stores output in conversation context.
func (s *AgentSession) Bash(ctx context.Context, commandText string) (BashResult, error) {
	var result BashResult
	err := s.Call(ctx, map[string]any{"type": "bash", "command": commandText}, &result)
	if err != nil {
		return BashResult{}, err
	}
	return result, nil
}

// AbortBash aborts a running bash RPC command.
func (s *AgentSession) AbortBash(ctx context.Context) error {
	_, err := s.callRPC(ctx, map[string]any{"type": "abort_bash"})
	return err
}

// GetSessionStats returns token/cost usage for the current session.
func (s *AgentSession) GetSessionStats(ctx context.Context) (SessionStats, error) {
	var result SessionStats
	err := s.Call(ctx, map[string]any{"type": "get_session_stats"}, &result)
	if err != nil {
		return SessionStats{}, err
	}
	return result, nil
}

// GetSessionStatsJSON returns raw `data` JSON for get_session_stats.
func (s *AgentSession) GetSessionStatsJSON(ctx context.Context) ([]byte, error) {
	return s.getRawDataJSON(ctx, "get_session_stats")
}

// ExportHTML exports current session to HTML and returns the output path.
func (s *AgentSession) ExportHTML(ctx context.Context, outputPath string) (string, error) {
	command := map[string]any{"type": "export_html"}
	if outputPath != "" {
		command["outputPath"] = outputPath
	}

	var data struct {
		Path string `json:"path"`
	}
	err := s.Call(ctx, command, &data)
	if err != nil {
		return "", err
	}
	return data.Path, nil
}

// Fork creates a new fork from the given entry ID.
func (s *AgentSession) Fork(ctx context.Context, entryID string) (ForkResult, error) {
	var result ForkResult
	err := s.Call(ctx, map[string]any{"type": "fork", "entryId": entryID}, &result)
	if err != nil {
		return ForkResult{}, err
	}
	return result, nil
}

// GetForkMessages returns user messages available for forking.
func (s *AgentSession) GetForkMessages(ctx context.Context) ([]ForkMessage, error) {
	var data struct {
		Messages []ForkMessage `json:"messages"`
	}
	err := s.Call(ctx, map[string]any{"type": "get_fork_messages"}, &data)
	if err != nil {
		return nil, err
	}
	return data.Messages, nil
}

// GetForkMessagesJSON returns raw `data` JSON for get_fork_messages.
func (s *AgentSession) GetForkMessagesJSON(ctx context.Context) ([]byte, error) {
	return s.getRawDataJSON(ctx, "get_fork_messages")
}

// GetLastAssistantText returns the latest assistant text, or nil if no assistant messages exist.
func (s *AgentSession) GetLastAssistantText(ctx context.Context) (*string, error) {
	var data struct {
		Text *string `json:"text"`
	}
	err := s.Call(ctx, map[string]any{"type": "get_last_assistant_text"}, &data)
	if err != nil {
		return nil, err
	}
	return data.Text, nil
}

// GetLastAssistantOutcome returns the latest assistant message summary, including text and error fields.
func (s *AgentSession) GetLastAssistantOutcome(ctx context.Context) (*AssistantOutcome, error) {
	outcome, _, err := s.GetLatestAssistantOutcomeSince(ctx, 0)
	if err != nil {
		return nil, err
	}
	return outcome, nil
}

// GetLatestAssistantOutcomeSince returns the latest assistant outcome at or after startIndex.
func (s *AgentSession) GetLatestAssistantOutcomeSince(ctx context.Context, startIndex int) (*AssistantOutcome, int, error) {
	messages, err := s.GetMessages(ctx)
	if err != nil {
		return nil, -1, err
	}
	if startIndex < 0 {
		startIndex = 0
	}
	if startIndex > len(messages) {
		startIndex = len(messages)
	}

	for i := len(messages) - 1; i >= 0; i-- {
		if i < startIndex {
			break
		}

		message := messages[i]

		role, _ := message["role"].(string)
		if role != "assistant" {
			continue
		}

		return parseAssistantOutcome(message), i, nil
	}

	return nil, -1, nil
}

func parseAssistantOutcome(message AgentMessage) *AssistantOutcome {
	outcome := &AssistantOutcome{
		Message: message,
	}

	if stopReason, ok := message["stopReason"].(string); ok {
		outcome.StopReason = stopReason
	}
	if errorMessage, ok := message["errorMessage"].(string); ok {
		outcome.ErrorMessage = strings.TrimSpace(errorMessage)
	}

	content, _ := message["content"].([]any)
	var textBuilder strings.Builder
	for _, item := range content {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if partType, _ := part["type"].(string); partType != "text" {
			continue
		}
		text, _ := part["text"].(string)
		if strings.TrimSpace(text) == "" {
			continue
		}
		textBuilder.WriteString(text)
	}

	outcome.Text = strings.TrimSpace(textBuilder.String())
	return outcome
}

// SetSessionName sets the display name for the active session.
func (s *AgentSession) SetSessionName(ctx context.Context, name string) error {
	_, err := s.callRPC(ctx, map[string]any{"type": "set_session_name", "name": name})
	return err
}

// GetCommands lists extension/prompt/skill slash commands.
func (s *AgentSession) GetCommands(ctx context.Context) ([]CommandDescriptor, error) {
	var data struct {
		Commands []CommandDescriptor `json:"commands"`
	}
	err := s.Call(ctx, map[string]any{"type": "get_commands"}, &data)
	if err != nil {
		return nil, err
	}
	return data.Commands, nil
}

// GetCommandsJSON returns raw `data` JSON for get_commands.
func (s *AgentSession) GetCommandsJSON(ctx context.Context) ([]byte, error) {
	return s.getRawDataJSON(ctx, "get_commands")
}

func (s *AgentSession) getRawDataJSON(ctx context.Context, commandType string) ([]byte, error) {
	resp, err := s.callRPC(ctx, map[string]any{"type": commandType})
	if err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return []byte("null"), nil
	}

	out := make([]byte, len(resp.Data))
	copy(out, resp.Data)
	return out, nil
}

// NavigateTree is not currently exposed over Pi RPC mode.
func (s *AgentSession) NavigateTree(
	ctx context.Context,
	targetID string,
	summarize bool,
	customInstructions *string,
	replaceInstructions bool,
	label *string,
) error {
	_ = ctx
	_ = targetID
	_ = summarize
	_ = customInstructions
	_ = replaceInstructions
	_ = label
	return ErrUnsupportedInRPCMode
}

// SendHookMessage is not currently exposed over Pi RPC mode.
func (s *AgentSession) SendHookMessage(ctx context.Context, messageJSON string, triggerTurn bool) error {
	_ = ctx
	_ = messageJSON
	_ = triggerTurn
	return ErrUnsupportedInRPCMode
}
