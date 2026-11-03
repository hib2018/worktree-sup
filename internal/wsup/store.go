package wsup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type Entry struct {
	Branch      string `json:"branch"`
	Summary     string `json:"summary"`
	Base        string `json:"base"`
	Target      string `json:"target"`
	Path        string `json:"path"`
	IssueNumber int    `json:"issue_number,omitempty"`
	IssueURL    string `json:"issue_url,omitempty"`
}
type Metadata struct {
	Entries map[string]Entry `json:"entries"`
}
type Config struct {
	Test []string `json:"test"`
}
type State struct {
	Kind, Stage, Source, Target, SourcePath, TargetPath, SourceHEAD, TargetHEAD, Snapshot string
	Resolution                                                                            *Resolution `json:",omitempty"`
	Entry                                                                                 *Entry      `json:",omitempty"`
}

func (a *App) storage(name string) string { return filepath.Join(a.Repo.Common, "wsup", name) }
func safeStorage(path string) error {
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe wsup metadata directory")
	}
	if info, err = os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("not a regular metadata file: %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func readJSON(path string, target any) error {
	if _, err := os.Lstat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
		return os.ErrNotExist
	}
	if err := safeStorage(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(target); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing JSON in %s", path)
	}
	return nil
}
func writeJSON(path string, value any) error {
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := safeStorage(path); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (a *App) loadMetadata() (Metadata, error) {
	m := Metadata{Entries: map[string]Entry{}}
	err := readJSON(a.storage("metadata.json"), &m)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if m.Entries == nil {
		m.Entries = map[string]Entry{}
	}
	return m, err
}
func (a *App) loadConfig() (Config, error) {
	var c Config
	err := readJSON(a.storage("config.json"), &c)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if len(c.Test) > 0 && c.Test[0] == "" {
		return c, errors.New("empty test executable")
	}
	return c, err
}
func (a *App) saveState(s *State) error { return writeJSON(a.storage("operation.json"), s) }
func (a *App) loadState() (*State, error) {
	var s State
	err := readJSON(a.storage("operation.json"), &s)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch s.Kind {
	case "new", "merge", "remove":
	default:
		return nil, errors.New("invalid operation kind")
	}
	return &s, nil
}
func (a *App) idle() error {
	s, err := a.loadState()
	if err != nil {
		return err
	}
	if s != nil && s.Stage != "completed" && s.Stage != "aborted" {
		return fmt.Errorf("an interrupted operation exists (%s/%s); run wsup resume or wsup abort", s.Kind, s.Stage)
	}
	return nil
}
