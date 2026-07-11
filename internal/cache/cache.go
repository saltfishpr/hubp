package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type Cache struct {
	dir string
}

type Entry struct {
	Status    int                 `json:"status"`
	Headers   map[string][]string `json:"headers"`
	CreatedAt time.Time           `json:"created_at"`
	Size      int64               `json:"size"`
}

func New(dir string) (*Cache, error) {
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	return &Cache{dir: dir}, nil
}

func (c *Cache) paths(key string) (dataPath, metaPath, parent string) {
	prefix := "00"
	if len(key) >= 2 {
		prefix = key[:2]
	}
	parent = filepath.Join(c.dir, "objects", prefix)
	return filepath.Join(parent, key+".data"), filepath.Join(parent, key+".json"), parent
}

func (c *Cache) Open(key string, ttl time.Duration) (Entry, *os.File, bool, error) {
	dataPath, metaPath, _ := c.paths(key)
	metaData, err := os.ReadFile(metaPath)
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, nil, false, nil
	}
	if err != nil {
		return Entry{}, nil, false, fmt.Errorf("read cache metadata: %w", err)
	}
	var entry Entry
	if err := json.Unmarshal(metaData, &entry); err != nil {
		_ = os.Remove(metaPath)
		return Entry{}, nil, false, nil
	}
	if ttl > 0 && time.Since(entry.CreatedAt) > ttl {
		return Entry{}, nil, false, nil
	}
	file, err := os.Open(dataPath)
	if errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(metaPath)
		return Entry{}, nil, false, nil
	}
	if err != nil {
		return Entry{}, nil, false, fmt.Errorf("open cache data: %w", err)
	}
	info, err := file.Stat()
	if err != nil || info.Size() != entry.Size {
		_ = file.Close()
		_ = os.Remove(dataPath)
		_ = os.Remove(metaPath)
		return Entry{}, nil, false, nil
	}
	return entry, file, true, nil
}

func (c *Cache) NewTemp(key string) (*os.File, error) {
	_, _, parent := c.paths(key)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create cache object directory: %w", err)
	}
	file, err := os.CreateTemp(parent, key+"-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create cache temp file: %w", err)
	}
	return file, nil
}

func (c *Cache) Commit(key string, tempPath string, entry Entry) error {
	dataPath, metaPath, parent := c.paths(key)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create cache object directory: %w", err)
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		return fmt.Errorf("stat cache temp file: %w", err)
	}
	entry.Size = info.Size()
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}

	metaData, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal cache metadata: %w", err)
	}
	metaTemp, err := os.CreateTemp(parent, key+"-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create cache metadata temp file: %w", err)
	}
	metaTempName := metaTemp.Name()
	defer os.Remove(metaTempName)
	if _, err := metaTemp.Write(metaData); err != nil {
		_ = metaTemp.Close()
		return fmt.Errorf("write cache metadata: %w", err)
	}
	if err := metaTemp.Sync(); err != nil {
		_ = metaTemp.Close()
		return fmt.Errorf("sync cache metadata: %w", err)
	}
	if err := metaTemp.Close(); err != nil {
		return fmt.Errorf("close cache metadata: %w", err)
	}

	// Remove old metadata first so a partial replacement can only become a
	// cache miss, never a new data file paired with stale metadata.
	if err := os.Remove(metaPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old cache metadata: %w", err)
	}
	if err := os.Rename(tempPath, dataPath); err != nil {
		return fmt.Errorf("commit cache data: %w", err)
	}
	if err := os.Rename(metaTempName, metaPath); err != nil {
		return fmt.Errorf("commit cache metadata: %w", err)
	}
	return nil
}

func Serve(w http.ResponseWriter, r *http.Request, entry Entry, file *os.File) {
	for key, values := range entry.Headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Hubp-Cache", "HIT")
	age := int64(time.Since(entry.CreatedAt).Seconds())
	if age < 0 {
		age = 0
	}
	w.Header().Set("Age", fmt.Sprintf("%d", age))
	http.ServeContent(w, r, "", entry.CreatedAt, file)
}
