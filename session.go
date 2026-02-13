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
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrSessionClosed is returned when the underlying RPC subprocess is not available.
	ErrSessionClosed = errors.New("pi session is closed")
)

// CreateAgentSessionOptions controls process launch and default Pi RPC flags.
type CreateAgentSessionOptions struct {
	BinaryPath string

	// BinaryArgs are prepended before Pi RPC flags.
	// Useful for tests where BinaryPath is a wrapper executable.
	BinaryArgs []string

	// AdditionalArgs are appended after Pi RPC flags.
	AdditionalArgs []string

	Provider   string
	Model      string
	SessionDir string

	// DisableSessionPersistence adds --no-session.
	DisableSessionPersistence bool

	Cwd string
	Env []string

	// Stderr receives subprocess stderr. Defaults to io.Discard.
	Stderr io.Writer
}

// CreateAgentSessionResult mirrors the JS SDK surface where session creation returns a struct.
type CreateAgentSessionResult struct {
	Session *AgentSession
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
}

// CreateAgentSession starts `pi --mode rpc` and returns a connected headless session.
func CreateAgentSession(ctx context.Context, opts CreateAgentSessionOptions) (*CreateAgentSessionResult, error) {
	binary := opts.BinaryPath
	if binary == "" {
		binary = "pi"
	}

	args := make([]string, 0, len(opts.BinaryArgs)+10+len(opts.AdditionalArgs))
	args = append(args, opts.BinaryArgs...)
	args = append(args, "--mode", "rpc")
	if opts.Provider != "" {
		args = append(args, "--provider", opts.Provider)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.DisableSessionPersistence {
		args = append(args, "--no-session")
	}
	if opts.SessionDir != "" {
		args = append(args, "--session-dir", opts.SessionDir)
	}
	args = append(args, opts.AdditionalArgs...)

	cmd := exec.CommandContext(ctx, binary, args...)
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}

	if len(opts.Env) > 0 {
		cmd.Env = append(os.Environ(), opts.Env...)
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
	}

	go session.readLoop(stdout)
	go session.waitLoop()

	return &CreateAgentSessionResult{Session: session}, nil
}

func (s *AgentSession) waitLoop() {
	err := s.cmd.Wait()
	s.shutdown(err)
}

func (s *AgentSession) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024), 10*1024*1024)

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
	_, err := s.callRPC(ctx, command)
	return err
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
