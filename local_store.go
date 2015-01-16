package tuf

import (
	"bytes"
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/flynn/go-tuf/data"
)

func MemoryStore(meta map[string]json.RawMessage, files map[string][]byte) LocalStore {
	if meta == nil {
		meta = make(map[string]json.RawMessage)
	}
	return &memoryStore{
		meta:  meta,
		files: files,
		keys:  make(map[string][]*data.Key),
	}
}

type memoryStore struct {
	meta  map[string]json.RawMessage
	files map[string][]byte
	keys  map[string][]*data.Key
}

func (m *memoryStore) GetMeta() (map[string]json.RawMessage, error) {
	return m.meta, nil
}

func (m *memoryStore) SetMeta(name string, meta json.RawMessage) error {
	m.meta[name] = meta
	return nil
}

func (m *memoryStore) GetStagedTarget(path string) (io.ReadCloser, error) {
	data, ok := m.files[path]
	if !ok {
		return nil, ErrFileNotFound{path}
	}
	return ioutil.NopCloser(bytes.NewReader(data)), nil
}

func (m *memoryStore) Commit(map[string]json.RawMessage, bool, map[string]data.Hashes) error {
	return nil
}

func (m *memoryStore) GetKeys(role string) ([]*data.Key, error) {
	return m.keys[role], nil
}

func (m *memoryStore) SaveKey(role string, key *data.Key) error {
	if _, ok := m.keys[role]; !ok {
		m.keys[role] = make([]*data.Key, 0)
	}
	m.keys[role] = append(m.keys[role], key)
	return nil
}

func (m *memoryStore) Clean() error {
	return nil
}

func FileSystemStore(dir string) LocalStore {
	return &fileSystemStore{dir}
}

type fileSystemStore struct {
	dir string
}

func (f *fileSystemStore) repoDir() string {
	return filepath.Join(f.dir, "repository")
}

func (f *fileSystemStore) stagedDir() string {
	return filepath.Join(f.dir, "staged")
}

func (f *fileSystemStore) GetMeta() (map[string]json.RawMessage, error) {
	meta := make(map[string]json.RawMessage)
	var err error
	notExists := func(path string) bool {
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	}
	for _, name := range topLevelManifests {
		path := filepath.Join(f.stagedDir(), name)
		if notExists(path) {
			path = filepath.Join(f.repoDir(), name)
			if notExists(path) {
				continue
			}
		}
		meta[name], err = ioutil.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}
	return meta, nil
}

func (f *fileSystemStore) SetMeta(name string, meta json.RawMessage) error {
	if err := f.createDirs(); err != nil {
		return err
	}
	if err := ioutil.WriteFile(filepath.Join(f.stagedDir(), name), meta, 0644); err != nil {
		return err
	}
	return nil
}

func (f *fileSystemStore) createDirs() error {
	for _, dir := range []string{"keys", "repository", "staged/targets"} {
		if err := os.MkdirAll(filepath.Join(f.dir, dir), 0755); err != nil {
			return err
		}
	}
	return nil
}

func (f *fileSystemStore) GetStagedTarget(path string) (io.ReadCloser, error) {
	path = filepath.Join(f.stagedDir(), "targets", path)
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrFileNotFound{path}
		}
		return nil, err
	}
	return file, nil
}

func (f *fileSystemStore) createRepoFile(path string) (*os.File, error) {
	dst := filepath.Join(f.repoDir(), path)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return nil, err
	}
	return os.Create(dst)
}

func hashedPaths(path string, hashes data.Hashes) []string {
	paths := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		hashedPath := filepath.Join(filepath.Dir(path), hash.String()+"."+filepath.Base(path))
		paths = append(paths, hashedPath)
	}
	return paths
}

func (f *fileSystemStore) Commit(meta map[string]json.RawMessage, consistentSnapshot bool, hashes map[string]data.Hashes) error {
	shouldCopyHashed := func(path string) bool {
		return consistentSnapshot && path != "timestamp.json"
	}
	shouldCopyUnhashed := func(path string) bool {
		return !consistentSnapshot || path == "root.json" || path == "timestamp.json"
	}
	copyToRepo := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(f.stagedDir(), path)
		if err != nil {
			return err
		}
		var paths []string
		if shouldCopyHashed(rel) {
			paths = append(paths, hashedPaths(rel, hashes[rel])...)
		}
		if shouldCopyUnhashed(rel) {
			paths = append(paths, rel)
		}
		var files []io.Writer
		for _, path := range paths {
			file, err := f.createRepoFile(path)
			if err != nil {
				return err
			}
			defer file.Close()
			files = append(files, file)
		}
		staged, err := os.Open(path)
		if err != nil {
			return err
		}
		defer staged.Close()
		if _, err = io.Copy(io.MultiWriter(files...), staged); err != nil {
			return err
		}
		return nil
	}
	isTarget := func(path string) bool {
		return strings.HasPrefix(path, "targets")
	}
	needsRemoval := func(path string) bool {
		if consistentSnapshot {
			// strip out the hash
			name := strings.SplitN(filepath.Base(path), ".", 2)
			if name[1] == "" {
				return false
			}
			path = filepath.Join(filepath.Dir(path), name[1])
		}
		_, ok := hashes[path]
		return !ok
	}
	removeFile := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(f.repoDir(), path)
		if err != nil {
			return err
		}
		if !info.IsDir() && isTarget(rel) && needsRemoval(rel) {
			if err := os.Remove(path); err != nil {
				// TODO: log / handle error
			}
			// TODO: remove empty directory
		}
		return nil
	}
	if err := filepath.Walk(f.stagedDir(), copyToRepo); err != nil {
		return err
	}
	if err := filepath.Walk(f.repoDir(), removeFile); err != nil {
		return err
	}
	return f.Clean()
}

func (f *fileSystemStore) GetKeys(role string) ([]*data.Key, error) {
	files, err := ioutil.ReadDir(filepath.Join(f.dir, "keys"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	signingKeys := make([]*data.Key, 0, len(files))
	for _, file := range files {
		if !strings.HasPrefix(file.Name(), role) {
			continue
		}
		s, err := os.Open(filepath.Join(f.dir, "keys", file.Name()))
		if err != nil {
			return nil, err
		}
		key := &data.Key{}
		if err := json.NewDecoder(s).Decode(key); err != nil {
			return nil, err
		}
		signingKeys = append(signingKeys, key)
	}
	return signingKeys, nil
}

func (f *fileSystemStore) SaveKey(role string, key *data.Key) error {
	if err := f.createDirs(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(key, "", "  ")
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(filepath.Join(f.dir, "keys", role+"-"+key.ID()+".json"), append(data, '\n'), 0600); err != nil {
		return err
	}
	return nil
}

func (f *fileSystemStore) Clean() error {
	if err := os.RemoveAll(f.stagedDir()); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(f.stagedDir(), "targets"), 0755)
}
