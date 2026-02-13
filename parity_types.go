package pisdk

import "errors"

var (
	// ErrUnsupportedInRPCMode matches Zig behavior for APIs not exposed over RPC.
	ErrUnsupportedInRPCMode = errors.New("unsupported in rpc mode")
)

// ToolSpec describes a Pi tool name with an optional cwd scope.
type ToolSpec struct {
	Name string `json:"name"`
	Cwd  string `json:"cwd,omitempty"`
}

var (
	ReadTool  = ToolSpec{Name: "read"}
	BashTool  = ToolSpec{Name: "bash"}
	EditTool  = ToolSpec{Name: "edit"}
	WriteTool = ToolSpec{Name: "write"}
	GrepTool  = ToolSpec{Name: "grep"}
	FindTool  = ToolSpec{Name: "find"}
	LsTool    = ToolSpec{Name: "ls"}

	CodingTools   = []ToolSpec{ReadTool, BashTool, EditTool, WriteTool}
	ReadOnlyTools = []ToolSpec{ReadTool, GrepTool, FindTool, LsTool}
)

func CreateReadTool(cwd string) ToolSpec {
	return ToolSpec{Name: "read", Cwd: cwd}
}

func CreateBashTool(cwd string) ToolSpec {
	return ToolSpec{Name: "bash", Cwd: cwd}
}

func CreateEditTool(cwd string) ToolSpec {
	return ToolSpec{Name: "edit", Cwd: cwd}
}

func CreateWriteTool(cwd string) ToolSpec {
	return ToolSpec{Name: "write", Cwd: cwd}
}

func CreateGrepTool(cwd string) ToolSpec {
	return ToolSpec{Name: "grep", Cwd: cwd}
}

func CreateFindTool(cwd string) ToolSpec {
	return ToolSpec{Name: "find", Cwd: cwd}
}

func CreateLsTool(cwd string) ToolSpec {
	return ToolSpec{Name: "ls", Cwd: cwd}
}

func CreateCodingTools(cwd string) []ToolSpec {
	return []ToolSpec{
		CreateReadTool(cwd),
		CreateBashTool(cwd),
		CreateEditTool(cwd),
		CreateWriteTool(cwd),
	}
}

func CreateReadOnlyTools(cwd string) []ToolSpec {
	return []ToolSpec{
		CreateReadTool(cwd),
		CreateGrepTool(cwd),
		CreateFindTool(cwd),
		CreateLsTool(cwd),
	}
}

// SessionManagerMode controls session persistence strategy.
type SessionManagerMode string

const (
	SessionManagerModeInMemory   SessionManagerMode = "in_memory"
	SessionManagerModePersistent SessionManagerMode = "persistent"
)

// SessionManager configures in-memory or persistent sessions.
type SessionManager struct {
	Mode       SessionManagerMode
	SessionDir string
}

func InMemorySessionManager() *SessionManager {
	return &SessionManager{Mode: SessionManagerModeInMemory}
}

func PersistentSessionManager(sessionDir string) *SessionManager {
	return &SessionManager{Mode: SessionManagerModePersistent, SessionDir: sessionDir}
}

// Settings mirrors the Zig SDK settings payload applied after session startup.
type Settings struct {
	AutoCompactionEnabled *bool
	AutoRetryEnabled      *bool
	ThinkingLevel         *ThinkingLevel
	SteeringMode          *QueueMode
	FollowUpMode          *QueueMode
}

// SettingsManager holds startup settings.
type SettingsManager struct {
	Settings Settings
}

func InMemorySettingsManager(settings Settings) *SettingsManager {
	return &SettingsManager{Settings: settings}
}

// ResourceLocation is the source location of an extension/skill/prompt entry.
type ResourceLocation string

const (
	ResourceLocationProject ResourceLocation = "project"
	ResourceLocationUser    ResourceLocation = "user"
)

// ResourceEntry tracks a discovered resource path.
type ResourceEntry struct {
	Path     string           `json:"path"`
	Location ResourceLocation `json:"location"`
}

// AgentsFile is a loaded AGENTS.md file.
type AgentsFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

// LoadExtensionsResult mirrors Zig's createAgentSession extension metadata.
type LoadExtensionsResult struct {
	Extensions []ResourceEntry `json:"extensions"`
	Errors     []string        `json:"errors,omitempty"`
	Runtime    *string         `json:"runtime,omitempty"`
}

// ResourceLoader allows callers to provide custom extension discovery.
type ResourceLoader interface {
	GetExtensions() []ResourceEntry
}

// DefaultResourceLoaderOptions configure filesystem-backed resource discovery.
type DefaultResourceLoaderOptions struct {
	Cwd      string
	AgentDir string
}

// PrintOutputMode controls output mode for RunPrintMode.
type PrintOutputMode string

const (
	PrintOutputModeText PrintOutputMode = "text"
	PrintOutputModeJSON PrintOutputMode = "json"
)

// RunPrintModeOptions configure prompt execution in print mode.
type RunPrintModeOptions struct {
	Mode           PrintOutputMode
	InitialMessage string
	InitialImages  []ImageContent
	Messages       []string
}

// RunRPCModeOptions configure direct `pi --mode rpc` spawning.
type RunRPCModeOptions struct {
	BinaryPath     string
	Cwd            string
	AdditionalArgs []string
	ExtraArgs      []string
}
