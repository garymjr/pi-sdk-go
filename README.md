# pi-sdk-go

Headless Go SDK for Pi Coding Agent over RPC mode (`pi --mode rpc`).

This SDK intentionally avoids any UI/TUI integration. It focuses on process control, typed command wrappers, and event streaming.

## Install

```bash
go get github.com/garymjr/pi-sdk-go
```

## Quick Start

```go
package main

import (
	"context"
	"fmt"
	"log"

	pisdk "github.com/garymjr/pi-sdk-go"
)

func main() {
	ctx := context.Background()

	result, err := pisdk.CreateAgentSession(ctx, pisdk.CreateAgentSessionOptions{
		DisableSessionPersistence: true,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer result.Session.Close()

	unsubscribe := result.Session.Subscribe(func(event pisdk.Event) {
		if event.Type != "message_update" {
			return
		}

		var update pisdk.MessageUpdateEvent
		if err := event.Decode(&update); err != nil {
			return
		}

		if update.AssistantMessageEvent.Type == "text_delta" {
			fmt.Print(update.AssistantMessageEvent.Delta)
		}
	})
	defer unsubscribe()

	if err := result.Session.Prompt(ctx, "List the files in this directory.", nil); err != nil {
		log.Fatal(err)
	}
}
```

## API Surface

Core:
- `CreateAgentSession(...)`
- `(*AgentSession).Subscribe(...)`
- `(*AgentSession).Call(...)` (raw command escape hatch)
- `(*AgentSession).Close()`

Prompting:
- `Prompt`
- `Steer`
- `FollowUp`
- `Abort`

State and model:
- `GetState`
- `GetMessages`
- `SetModel`
- `CycleModel`
- `GetAvailableModels`
- `SetThinkingLevel`
- `CycleThinkingLevel`

Queue and compaction/retry:
- `SetSteeringMode`
- `SetFollowUpMode`
- `Compact`
- `SetAutoCompaction`
- `SetAutoRetry`
- `AbortRetry`

Bash and session utilities:
- `Bash`
- `AbortBash`
- `NewSession`
- `SwitchSession`
- `Fork`
- `GetForkMessages`
- `GetLastAssistantText`
- `SetSessionName`
- `GetSessionStats`
- `ExportHTML`
- `GetCommands`

## Notes

- Event payloads are exposed as raw JSON via `Event.Raw` and can be decoded into typed structs.
- Command failures return `*CommandError`.
- Process-level failures return `ErrSessionClosed` (or wrapped with subprocess error detail).
