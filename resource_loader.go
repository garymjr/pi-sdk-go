package pisdk

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultResourceLoader scans project and user directories for Pi resources.
type DefaultResourceLoader struct {
	cwd         string
	agentDir    string
	extensions  []ResourceEntry
	skills      []ResourceEntry
	prompts     []ResourceEntry
	agentsFiles []AgentsFile
}

func NewDefaultResourceLoader(opts DefaultResourceLoaderOptions) (*DefaultResourceLoader, error) {
	cwd := opts.Cwd
	if cwd == "" {
		cwd = "."
	}

	resolvedCwd, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}

	agentDir := opts.AgentDir
	if agentDir == "" {
		agentDir, err = defaultAgentDir()
		if err != nil {
			return nil, err
		}
	} else {
		agentDir, err = expandAndResolvePath(agentDir)
		if err != nil {
			return nil, err
		}
	}

	return &DefaultResourceLoader{
		cwd:      resolvedCwd,
		agentDir: agentDir,
	}, nil
}

func (l *DefaultResourceLoader) Reload() error {
	l.extensions = nil
	l.skills = nil
	l.prompts = nil
	l.agentsFiles = nil

	if err := l.loadExtensions(); err != nil {
		return err
	}
	if err := l.loadSkills(); err != nil {
		return err
	}
	if err := l.loadPrompts(); err != nil {
		return err
	}
	if err := l.loadAgentsFiles(); err != nil {
		return err
	}
	return nil
}

func (l *DefaultResourceLoader) GetExtensions() []ResourceEntry {
	return append([]ResourceEntry(nil), l.extensions...)
}

func (l *DefaultResourceLoader) GetSkills() []ResourceEntry {
	return append([]ResourceEntry(nil), l.skills...)
}

func (l *DefaultResourceLoader) GetPrompts() []ResourceEntry {
	return append([]ResourceEntry(nil), l.prompts...)
}

func (l *DefaultResourceLoader) GetAgentsFiles() []AgentsFile {
	out := make([]AgentsFile, len(l.agentsFiles))
	for i, file := range l.agentsFiles {
		out[i] = AgentsFile{
			Path:    file.Path,
			Content: append([]byte(nil), file.Content...),
		}
	}
	return out
}

func (l *DefaultResourceLoader) loadExtensions() error {
	projectDir := filepath.Join(l.cwd, ".pi", "extensions")
	if err := collectRegularFiles(projectDir, ResourceLocationProject, &l.extensions, ""); err != nil {
		return err
	}

	userDir := filepath.Join(l.agentDir, "extensions")
	if err := collectRegularFiles(userDir, ResourceLocationUser, &l.extensions, ""); err != nil {
		return err
	}
	return nil
}

func (l *DefaultResourceLoader) loadSkills() error {
	projectDir := filepath.Join(l.cwd, ".pi", "skills")
	if err := collectRegularFiles(projectDir, ResourceLocationProject, &l.skills, "SKILL.md"); err != nil {
		return err
	}

	userDir := filepath.Join(l.agentDir, "skills")
	if err := collectRegularFiles(userDir, ResourceLocationUser, &l.skills, "SKILL.md"); err != nil {
		return err
	}
	return nil
}

func (l *DefaultResourceLoader) loadPrompts() error {
	projectDir := filepath.Join(l.cwd, ".pi", "prompts")
	if err := collectRegularFiles(projectDir, ResourceLocationProject, &l.prompts, ".md"); err != nil {
		return err
	}

	userDir := filepath.Join(l.agentDir, "prompts")
	if err := collectRegularFiles(userDir, ResourceLocationUser, &l.prompts, ".md"); err != nil {
		return err
	}
	return nil
}

func (l *DefaultResourceLoader) loadAgentsFiles() error {
	if err := l.maybeAppendAgentsFile(filepath.Join(l.agentDir, "AGENTS.md")); err != nil {
		return err
	}

	cursor := l.cwd
	for {
		if err := l.maybeAppendAgentsFile(filepath.Join(cursor, "AGENTS.md")); err != nil {
			return err
		}

		parent := filepath.Dir(cursor)
		if parent == cursor {
			break
		}
		cursor = parent
	}
	return nil
}

func (l *DefaultResourceLoader) maybeAppendAgentsFile(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	l.agentsFiles = append(l.agentsFiles, AgentsFile{
		Path:    path,
		Content: content,
	})
	return nil
}

func collectRegularFiles(root string, location ResourceLocation, out *[]ResourceEntry, filter string) error {
	stat, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !stat.IsDir() {
		return nil
	}

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}

		if filter != "" {
			if strings.HasPrefix(filter, ".") {
				if !strings.HasSuffix(d.Name(), filter) {
					return nil
				}
			} else if d.Name() != filter {
				return nil
			}
		}

		*out = append(*out, ResourceEntry{Path: path, Location: location})
		return nil
	})
}

func defaultAgentDir() (string, error) {
	if value := os.Getenv("PI_CODING_AGENT_DIR"); value != "" {
		return expandAndResolvePath(value)
	}
	if value := os.Getenv("PI_AGENT_DIR"); value != "" {
		return expandAndResolvePath(value)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pi", "agent"), nil
}

func expandAndResolvePath(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[2:])
	} else if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = home
	}

	return filepath.Abs(path)
}
