package wsup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type fileIdentity struct {
	dev uint64
	ino uint64
}

func ignoredIdentity(root, path string) (fileIdentity, bool, error) {
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return fileIdentity{}, false, nil
	}
	if err != nil {
		return fileIdentity{}, false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, false, errors.New("unsupported filesystem identity")
	}
	return fileIdentity{uint64(stat.Dev), uint64(stat.Ino)}, true, nil
}

// Git's merge strategies do not consistently honor --no-overwrite-ignore.
// Check incoming paths, including filesystem case/Unicode aliases and
// directory/file collisions, independently of Git's overwrite protection.
func (a *App) protectIgnored(s *State) error {
	ignored, err := gitRaw(s.TargetPath, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	if len(ignored) == 0 {
		return nil
	}
	incoming, err := gitRaw(s.TargetPath, "ls-tree", "-r", "--name-only", "-z", s.SourceHEAD)
	if err != nil {
		return err
	}
	occupied := map[string]bool{}
	parents := map[string]bool{}
	occupiedIDs := map[fileIdentity]bool{}
	parentIDs := map[fileIdentity]bool{}
	for _, p := range strings.Split(string(ignored), "\x00") {
		if p == "" {
			continue
		}
		occupied[p] = true
		id, ok, err := ignoredIdentity(s.TargetPath, p)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("ignored file changed during validation")
		}
		occupiedIDs[id] = true
		for i := strings.LastIndexByte(p, '/'); i >= 0; i = strings.LastIndexByte(p, '/') {
			p = p[:i]
			parents[p] = true
			id, ok, err := ignoredIdentity(s.TargetPath, p)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("ignored parent path changed during validation")
			}
			parentIDs[id] = true
		}
	}
	for _, name := range strings.Split(string(incoming), "\x00") {
		if name == "" {
			continue
		}
		p := name
		for {
			id, exists, err := ignoredIdentity(s.TargetPath, p)
			if err != nil {
				return err
			}
			if occupied[p] || exists && occupiedIDs[id] || p == name && (parents[p] || exists && parentIDs[id]) {
				return fmt.Errorf("incoming path collides with an ignored file: %s", name)
			}
			i := strings.LastIndexByte(p, '/')
			if i < 0 {
				break
			}
			p = p[:i]
		}
	}
	return nil
}
