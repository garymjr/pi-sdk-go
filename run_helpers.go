package pisdk

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// RunPrintMode executes one or more prompts and writes text/json event output.
func RunPrintMode(ctx context.Context, session *AgentSession, writer io.Writer, opts RunPrintModeOptions) error {
	mode := opts.Mode
	if mode == "" {
		mode = PrintOutputModeText
	}

	if opts.InitialMessage != "" {
		if err := runSinglePrintPrompt(ctx, session, writer, mode, opts.InitialMessage, opts.InitialImages); err != nil {
			return err
		}
	}

	for _, message := range opts.Messages {
		if err := runSinglePrintPrompt(ctx, session, writer, mode, message, nil); err != nil {
			return err
		}
	}

	return nil
}

func runSinglePrintPrompt(
	ctx context.Context,
	session *AgentSession,
	writer io.Writer,
	mode PrintOutputMode,
	message string,
	images []ImageContent,
) error {
	eventCh := make(chan Event, 128)
	unsubscribe := session.Subscribe(func(event Event) {
		select {
		case eventCh <- event:
		default:
		}
	})
	defer unsubscribe()

	if err := session.Prompt(ctx, message, &PromptOptions{Images: images}); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.Done():
			return session.sessionClosedError()
		case event := <-eventCh:
			switch mode {
			case PrintOutputModeJSON:
				if _, err := writer.Write(event.Raw); err != nil {
					return err
				}
				if _, err := writer.Write([]byte("\n")); err != nil {
					return err
				}
			case PrintOutputModeText:
				if event.Type == "message_update" {
					var update MessageUpdateEvent
					if err := event.Decode(&update); err == nil && update.AssistantMessageEvent.Type == "text_delta" {
						if _, err := io.WriteString(writer, update.AssistantMessageEvent.Delta); err != nil {
							return err
						}
					}
				}
			default:
				return fmt.Errorf("unsupported print output mode: %q", mode)
			}

			if event.Type == "agent_end" {
				if mode == PrintOutputModeText {
					if _, err := writer.Write([]byte("\n")); err != nil {
						return err
					}
				}
				return nil
			}
		}
	}
}

// RunRPCMode starts `pi --mode rpc` with inherited stdio.
func RunRPCMode(ctx context.Context, opts RunRPCModeOptions) (*exec.Cmd, error) {
	binary := opts.BinaryPath
	if binary == "" {
		binary = "pi"
	}

	args := make([]string, 0, 2+len(opts.AdditionalArgs)+len(opts.ExtraArgs))
	args = append(args, "--mode", "rpc")
	args = append(args, opts.AdditionalArgs...)
	args = append(args, opts.ExtraArgs...)

	cmd := exec.CommandContext(ctx, binary, args...)
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start pi rpc process: %w", err)
	}

	return cmd, nil
}
