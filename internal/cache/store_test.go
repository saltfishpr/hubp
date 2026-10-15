package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T, max int64) *Store {
	t.Helper()
	s, e := Open(Config{Dir: t.TempDir(), MaxBytes: max, MaxObjectBytes: max})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func put(t *testing.T, s *Store, k, text string) {
	t.Helper()
	f, e := s.Begin(k, "")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Abort()
	if _, e = io.Copy(f, strings.NewReader(text)); e != nil {
		t.Fatal(e)
	}
	if e = f.Commit(context.Background(), http.Header{"Etag": {"\"v1\""}}, time.Hour, int64(len(text))); e != nil {
		t.Fatal(e)
	}
}

func TestCompleteOnlyAndExclusiveDirectory(t *testing.T) {
	s := testStore(t, 1<<20)
	if other, e := Open(s.cfg); e == nil {
		other.Close()
		t.Fatal("directory lock was not exclusive")
	}
	f, e := s.Begin("bad", "")
	if e != nil {
		t.Fatal(e)
	}
	_, _ = f.Write([]byte("short"))
	if e = f.Commit(context.Background(), http.Header{}, time.Hour, 100); e == nil {
		t.Fatal("truncation accepted")
	}
	if hit, _ := s.Lookup(context.Background(), "bad"); hit != nil {
		hit.Close()
		t.Fatal("partial object became visible")
	}
	tmp, _ := os.ReadDir(filepath.Join(s.cfg.Dir, "tmp"))
	if len(tmp) != 0 {
		t.Fatal("part leaked")
	}
	wrong := "sha256:" + strings.Repeat("0", 64)
	f, e = s.Begin("wrong", wrong)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = f.Write([]byte("content"))
	if e = f.Commit(context.Background(), http.Header{}, time.Hour, 7); e == nil {
		t.Fatal("wrong digest accepted")
	}
	put(t, s, "ok", "content")
	hit, e := s.Lookup(context.Background(), "ok")
	if e != nil || hit == nil {
		t.Fatal(e)
	}
	defer hit.Close()
	b, _ := io.ReadAll(hit.File)
	if string(b) != "content" {
		t.Fatal(string(b))
	}
}

func TestLeaseSurvivesEvictionAndDedup(t *testing.T) {
	s := testStore(t, 6)
	put(t, s, "one", "abc")
	put(t, s, "alias", "abc")
	if s.used != 3 {
		t.Fatal("no physical dedup")
	}
	entry, e := s.Lookup(context.Background(), "one")
	if e != nil {
		t.Fatal(e)
	}
	f, e := s.Begin("two", "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Write([]byte("1234")); e == nil {
		f.Abort()
		t.Fatal("evicted leased object or exceeded budget")
	}
	f.Abort()
	b, _ := io.ReadAll(entry.File)
	if string(b) != "abc" {
		t.Fatal("leased file lost")
	}
	entry.Close()
	put(t, s, "two", "1234")
	if s.used > 6 {
		t.Fatal("budget exceeded")
	}
}

func TestRestartRemovesPartOrphanAndMissingMapping(t *testing.T) {
	cfg := Config{Dir: t.TempDir(), MaxBytes: 1 << 20, MaxObjectBytes: 1 << 20}
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	put(t, s, "keep", "keep")
	put(t, s, "missing", "gone")
	h := sha256.Sum256([]byte("gone"))
	if e = os.Remove(s.path(hex.EncodeToString(h[:]))); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(cfg.Dir, "tmp", "left.part"), []byte("part"), 0o600); e != nil {
		t.Fatal(e)
	}
	orphan := strings.Repeat("e", 64)
	if e = os.MkdirAll(filepath.Dir(s.path(orphan)), 0o700); e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(s.path(orphan), []byte("orphan"), 0o600)
	s.Close()
	s, e = Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	good, e := s.Lookup(context.Background(), "keep")
	if e != nil || good == nil {
		t.Fatal(e)
	}
	good.Close()
	if miss, _ := s.Lookup(context.Background(), "missing"); miss != nil {
		miss.Close()
		t.Fatal("dangling mapping survived")
	}
	if _, e = os.Stat(s.path(orphan)); !os.IsNotExist(e) {
		t.Fatal("orphan survived")
	}
	files, _ := os.ReadDir(filepath.Join(cfg.Dir, "tmp"))
	if len(files) != 0 {
		t.Fatal("part survived")
	}
}

func TestFailedIndexCommitRetainsSafeObjectUntilRecovery(t *testing.T) {
	cfg := Config{Dir: t.TempDir(), MaxBytes: 1024, MaxObjectBytes: 1024}
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	put(t, s, "base", "base")
	_, e = s.db.Exec(`CREATE TRIGGER fail_resource BEFORE INSERT ON resources BEGIN SELECT RAISE(FAIL,'forced index failure'); END;`)
	if e != nil {
		t.Fatal(e)
	}
	f, e := s.Begin("failed", "")
	if e != nil {
		t.Fatal(e)
	}
	_, _ = f.Write([]byte("new"))
	if e = f.Commit(context.Background(), http.Header{}, time.Hour, 3); e == nil {
		t.Fatal("SQL failure ignored")
	}
	if !s.writeDisabled || s.temp != 0 || s.used != 7 {
		t.Fatal("failed commit accounting", s.used, s.temp)
	}
	entry, e := s.Lookup(context.Background(), "base")
	if e != nil || entry == nil {
		t.Fatal("existing object damaged", e)
	}
	entry.Close()
	if entry, e = s.Lookup(context.Background(), "failed"); e != nil || entry != nil {
		t.Fatal("uncommitted resource visible", e)
	}
	_, _ = s.db.Exec("DROP TRIGGER fail_resource")
	s.Close()
	s, e = Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if s.used != 4 || s.writeDisabled {
		t.Fatal("orphan recovery failed", s.used)
	}
	put(t, s, "recovered", "new")
}

func TestConcurrentFillReadAndEviction(t *testing.T) {
	s := testStore(t, 1024)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i%6))
			data := strings.Repeat(key, 256)
			for n := 0; n < 8; n++ {
				e, err := s.Lookup(context.Background(), key)
				if err != nil {
					t.Error(err)
					return
				}
				if e != nil {
					b, err := io.ReadAll(e.File)
					e.Close()
					if err != nil || string(b) != data {
						t.Error("leased object changed")
					}
					continue
				}
				f, err := s.Begin(key, "")
				if err != nil {
					continue
				}
				if _, err = f.Write([]byte(data)); err != nil {
					f.Abort()
					continue
				}
				if err = f.Commit(context.Background(), http.Header{}, time.Hour, int64(len(data))); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.fills) != 0 || len(s.leases) != 0 || s.temp != 0 || s.used > 1024 {
		t.Error("resource accounting leaked")
	}
}
