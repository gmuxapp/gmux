package metadata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var MetaDir = "/tmp/gmux-meta"

type SessionMeta struct {
	Version    int      `json:"version"`
	SessionID  string   `json:"session_id"`
	AbducoName string   `json:"abduco_name"`
	Kind       string   `json:"kind"`
	Command    []string `json:"command"`
	Cwd        string   `json:"cwd"`
	State      string   `json:"state"`
	CreatedAt  float64  `json:"created_at"`
	UpdatedAt  float64  `json:"updated_at"`
	Pid        int      `json:"pid,omitempty"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	Error      string   `json:"error,omitempty"`

	// Transport
	SocketPath string `json:"socket_path,omitempty"`

	// pi-specific
	SessionFile       string `json:"session_file,omitempty"`
	SessionFileExists bool   `json:"session_file_exists,omitempty"`
}

func nowUnix() float64 {
	return float64(time.Now().UnixNano()) / float64(time.Second)
}

func metaPath(abducoName string) string {
	return filepath.Join(MetaDir, abducoName+".json")
}

func New(sessionID, abducoName, kind, cwd string, command []string) *SessionMeta {
	now := nowUnix()
	return &SessionMeta{
		Version:    1,
		SessionID:  sessionID,
		AbducoName: abducoName,
		Kind:       kind,
		Command:    command,
		Cwd:        cwd,
		State:      "starting",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func (m *SessionMeta) Write() error {
	if err := os.MkdirAll(MetaDir, 0o755); err != nil {
		return fmt.Errorf("create meta dir: %w", err)
	}

	m.UpdatedAt = nowUnix()

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	path := metaPath(m.AbducoName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	return nil
}

func (m *SessionMeta) SetState(state string) error {
	m.State = state
	return m.Write()
}

func (m *SessionMeta) SetRunning(pid int) error {
	m.State = "running"
	m.Pid = pid
	return m.Write()
}

func (m *SessionMeta) SetExited(exitCode int) error {
	m.State = "exited"
	m.ExitCode = &exitCode
	return m.Write()
}

func (m *SessionMeta) SetError(msg string) error {
	m.State = "error"
	m.Error = msg
	return m.Write()
}

func (m *SessionMeta) Cleanup() error {
	path := metaPath(m.AbducoName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func Read(abducoName string) (*SessionMeta, error) {
	data, err := os.ReadFile(metaPath(abducoName))
	if err != nil {
		return nil, err
	}

	var meta SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse metadata: %w", err)
	}
	return &meta, nil
}

func ListAll() ([]*SessionMeta, error) {
	entries, err := os.ReadDir(MetaDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var result []*SessionMeta
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		abducoName := entry.Name()[:len(entry.Name())-5] // strip .json
		meta, err := Read(abducoName)
		if err != nil {
			continue // skip unreadable
		}
		result = append(result, meta)
	}
	return result, nil
}
