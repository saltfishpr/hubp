// Package cache stores public complete objects. File publication precedes index publication.
package cache

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrCapacity  = errors.New("cache capacity unavailable")
	ErrFilling   = errors.New("resource is already filling")
	ErrIntegrity = errors.New("cache integrity check failed")
)

type Config struct {
	Dir                      string
	MaxBytes, MaxObjectBytes int64
	MinFreeBytes             uint64
}
type Store struct {
	mu            sync.Mutex
	db            *sql.DB
	cfg           Config
	lock          *os.File
	used, temp    int64
	fills         map[string]bool
	leases        map[string]int
	touches       map[string]int64
	closed        bool
	writeDisabled bool
}
type Entry struct {
	Key, Hash string
	Size      int64
	Headers   http.Header
	Expires   time.Time
	Stored    time.Time
	File      *os.File
	store     *Store
	once      sync.Once
}

// Close 关闭缓存文件并释放读取租约；重复调用不会重复释放资源。
func (e *Entry) Close() {
	e.once.Do(func() {
		e.File.Close()
		e.store.mu.Lock()
		e.store.leases[e.Hash]--
		if e.store.leases[e.Hash] == 0 {
			delete(e.store.leases, e.Hash)
		}
		e.store.mu.Unlock()
	})
}

// Fresh 判断缓存条目是否尚未过期。
func (e *Entry) Fresh() bool { return time.Now().Before(e.Expires) }

// Open 初始化缓存目录和索引，独占锁定目录并清理上次异常退出遗留的数据。
// 使用结束后应调用 Close 释放数据库连接和目录锁。
func Open(cfg Config) (*Store, error) {
	if cfg.MaxBytes <= 0 || cfg.MaxObjectBytes <= 0 || cfg.MaxObjectBytes > cfg.MaxBytes {
		return nil, errors.New("invalid cache capacity")
	}
	abs, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	cfg.Dir = abs
	for _, dir := range []string{cfg.Dir, filepath.Join(cfg.Dir, "tmp"), filepath.Join(cfg.Dir, "objects")} {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	lock, err := os.OpenFile(filepath.Join(cfg.Dir, "instance.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("cache directory is in use")
	}
	s := &Store{cfg: cfg, lock: lock, fills: map[string]bool{}, leases: map[string]int{}, touches: map[string]int64{}}
	db, err := sql.Open("sqlite", filepath.Join(cfg.Dir, "meta.db"))
	if err != nil {
		s.unlock()
		return nil, err
	}
	s.db = db
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY);
 INSERT OR IGNORE INTO schema_migrations VALUES(1);
 CREATE TABLE IF NOT EXISTS objects(hash TEXT PRIMARY KEY,size INTEGER NOT NULL CHECK(size>=0));
 CREATE TABLE IF NOT EXISTS resources(key TEXT PRIMARY KEY,hash TEXT NOT NULL,headers BLOB NOT NULL,expires INTEGER NOT NULL,accessed INTEGER NOT NULL,stored INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS resources_accessed ON resources(accessed);
 CREATE INDEX IF NOT EXISTS resources_hash ON resources(hash);`)
	if err == nil {
		err = s.recover()
	}
	if err != nil {
		db.Close()
		s.unlock()
		return nil, err
	}
	return s, nil
}

func (s *Store) path(h string) string { return filepath.Join(s.cfg.Dir, "objects", h[:2], h[2:4], h) }

func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	_, e := hex.DecodeString(h)
	return e == nil && h == strings.ToLower(h)
}

func (s *Store) recover() error {
	tmp, err := os.ReadDir(filepath.Join(s.cfg.Dir, "tmp"))
	if err != nil {
		return err
	}
	for _, f := range tmp {
		if !f.IsDir() {
			if err = os.Remove(filepath.Join(s.cfg.Dir, "tmp", f.Name())); err != nil {
				return err
			}
		}
	}
	rows, err := s.db.Query("SELECT hash,size FROM objects")
	if err != nil {
		return err
	}
	type obj struct {
		h string
		n int64
	}
	var objects []obj
	for rows.Next() {
		var o obj
		if err = rows.Scan(&o.h, &o.n); err != nil {
			rows.Close()
			return err
		}
		objects = append(objects, o)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	known := map[string]bool{}
	for _, o := range objects {
		var st os.FileInfo
		if validHash(o.h) {
			st, err = os.Stat(s.path(o.h))
		} else {
			err = errors.New("invalid object hash")
		}
		if err != nil || !st.Mode().IsRegular() || st.Size() != o.n {
			if _, err = s.db.Exec("DELETE FROM resources WHERE hash=?", o.h); err != nil {
				return err
			}
			if _, err = s.db.Exec("DELETE FROM objects WHERE hash=?", o.h); err != nil {
				return err
			}
			continue
		}
		known[o.h] = true
		s.used += o.n
	}
	// Objects left by a crash between rename and SQL commit are never cache hits.
	err = filepath.WalkDir(filepath.Join(s.cfg.Dir, "objects"), func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		if !known[d.Name()] {
			return os.Remove(p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.collectLocked()
}

// Lookup 查找缓存条目并取得读取租约，未命中时返回 nil, nil；返回的条目可能已过期。
// 调用方必须调用条目的 Close 方法释放文件和租约。
func (s *Store) Lookup(ctx context.Context, key string) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("cache closed")
	}
	var h string
	var size, expires, stored int64
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT r.hash,o.size,r.headers,r.expires,r.stored FROM resources r JOIN objects o ON o.hash=r.hash WHERE r.key=?", key).Scan(&h, &size, &raw, &expires, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !validHash(h) {
		return nil, errors.New("invalid cache object")
	}
	f, err := os.Open(s.path(h))
	if err != nil {
		s.db.Exec("DELETE FROM resources WHERE key=?", key)
		return nil, nil
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != size {
		f.Close()
		s.db.Exec("DELETE FROM resources WHERE key=?", key)
		return nil, nil
	}
	hdr := http.Header{}
	if err = json.Unmarshal(raw, &hdr); err != nil {
		f.Close()
		return nil, err
	}
	s.leases[h]++
	s.touches[key] = time.Now().Unix()
	return &Entry{Key: key, Hash: h, Size: size, Headers: hdr, Expires: time.Unix(0, expires), Stored: time.Unix(0, stored), File: f, store: s}, nil
}

// Invalidate 删除指定键的缓存索引，关联文件由后续维护或容量回收清理。
func (s *Store) Invalidate(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, "DELETE FROM resources WHERE key=?", key)
	return err
}

// Refresh 更新已有缓存条目的响应头、存储时间和过期时间，不修改文件内容。
func (s *Store) Refresh(ctx context.Context, key string, headers http.Header, ttl time.Duration) error {
	raw, err := json.Marshal(headers)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.ExecContext(ctx, "UPDATE resources SET headers=?,expires=?,accessed=?,stored=? WHERE key=?", raw, time.Now().Add(ttl).UnixNano(), time.Now().Unix(), time.Now().UnixNano(), key)
	return err
}

type Fill struct {
	store          *Store
	key            string
	file           *os.File
	sha            hash.Hash
	expected       hash.Hash
	expectedHex    string
	size, reserved int64
	done           bool
}

// Begin 为指定键创建缓存写入任务，可通过 digest 指定 sha256 或 sha512 校验摘要。
// 同一键仅允许一个写入任务；调用方应调用 Commit 提交或 Abort 放弃。
func (s *Store) Begin(key, digest string) (*Fill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.writeDisabled {
		return nil, errors.New("cache closed")
	}
	if s.fills[key] {
		return nil, ErrFilling
	}
	var expected hash.Hash
	var expectedHex string
	if digest != "" {
		alg, h, ok := strings.Cut(digest, ":")
		if !ok {
			return nil, errors.New("invalid digest")
		}
		switch alg {
		case "sha256":
			expected = sha256.New()
		case "sha512":
			expected = sha512.New()
		default:
			return nil, errors.New("unsupported digest")
		}
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != expected.Size() {
			return nil, errors.New("invalid digest")
		}
		expectedHex = strings.ToLower(h)
	}
	if err := s.makeRoomLocked(0); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Join(s.cfg.Dir, "tmp"), "*.part")
	if err != nil {
		return nil, err
	}
	s.fills[key] = true
	return &Fill{store: s, key: key, file: f, sha: sha256.New(), expected: expected, expectedHex: expectedHex}, nil
}

// Write 在缓存容量限制内写入临时文件，并累计内容大小和摘要。
func (f *Fill) Write(p []byte) (int, error) {
	if f.done {
		return 0, errors.New("cache fill is closed")
	}
	s := f.store
	s.mu.Lock()
	if f.size+int64(len(p)) > s.cfg.MaxObjectBytes {
		s.mu.Unlock()
		return 0, ErrCapacity
	}
	err := s.makeRoomLocked(int64(len(p)))
	if err == nil {
		s.temp += int64(len(p))
		f.reserved += int64(len(p))
	}
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	n, err := f.file.Write(p)
	f.size += int64(n)
	f.sha.Write(p[:n])
	if f.expected != nil {
		f.expected.Write(p[:n])
	}
	return n, err
}

// Abort 放弃缓存写入，删除临时文件并释放预留容量；任务结束后调用无效。
func (f *Fill) Abort() {
	if f.done {
		return
	}
	f.done = true
	f.file.Close()
	os.Remove(f.file.Name())
	s := f.store
	s.mu.Lock()
	s.temp -= f.reserved
	delete(s.fills, f.key)
	s.mu.Unlock()
}

// Commit 校验内容长度和预期摘要后，先发布完整文件，再提交缓存索引；length 为负时不校验长度。
// 完整内容的提交不受 ctx 取消影响，另设 5 秒超时；失败后也会结束写入任务。
func (f *Fill) Commit(ctx context.Context, headers http.Header, ttl time.Duration, length int64) error {
	if f.done {
		return errors.New("cache fill is closed")
	}
	if length >= 0 && length != f.size {
		f.Abort()
		return fmt.Errorf("%w: length mismatch", ErrIntegrity)
	}
	if f.expected != nil && hex.EncodeToString(f.expected.Sum(nil)) != f.expectedHex {
		f.Abort()
		return fmt.Errorf("%w: digest mismatch", ErrIntegrity)
	}
	raw, err := json.Marshal(headers)
	if err != nil {
		f.Abort()
		return err
	}
	if err = f.file.Sync(); err != nil {
		f.Abort()
		return err
	}
	if err = f.file.Close(); err != nil {
		f.Abort()
		return err
	}
	// The payload is complete. Finish its short commit even if the reader has
	// just disconnected; never keep downloading in the background.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	h := hex.EncodeToString(f.sha.Sum(nil))
	s := f.store
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { f.done = true; s.temp -= f.reserved; delete(s.fills, f.key); os.Remove(f.file.Name()) }()
	path := s.path(h)
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var oldSize int64
	err = s.db.QueryRowContext(ctx, "SELECT size FROM objects WHERE hash=?", h).Scan(&oldSize)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	exists := err == nil
	if exists && oldSize != f.size {
		return errors.New("cached object metadata conflict")
	}
	if !exists {
		if err = os.Rename(f.file.Name(), path); err != nil {
			return err
		}
		s.used += f.size
		d, e := os.Open(filepath.Dir(path))
		if e != nil {
			s.writeDisabled = true
			return e
		}
		e = d.Sync()
		d.Close()
		if e != nil {
			s.writeDisabled = true
			return e
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.writeDisabled = true
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO objects(hash,size) VALUES(?,?)", h, f.size); err != nil {
		s.writeDisabled = true
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO resources(key,hash,headers,expires,accessed,stored) VALUES(?,?,?,?,?,?)
 ON CONFLICT(key) DO UPDATE SET hash=excluded.hash,headers=excluded.headers,expires=excluded.expires,accessed=excluded.accessed,stored=excluded.stored`, f.key, h, raw, time.Now().Add(ttl).UnixNano(), time.Now().Unix(), time.Now().UnixNano())
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		// SQL commit errors can be ambiguous. Retain the installed file so a
		// committed index can never point to an object we subsequently unlink.
		// Count it in the budget and reconcile on restart.
		s.writeDisabled = true
		return err
	}
	return nil
}

func (s *Store) flushTouchesLocked() {
	if len(s.touches) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, "UPDATE resources SET accessed=? WHERE key=?")
	if err != nil {
		return
	}
	defer stmt.Close()
	for key, at := range s.touches {
		if _, err = stmt.ExecContext(ctx, at, key); err != nil {
			return
		}
	}
	if err = tx.Commit(); err == nil {
		clear(s.touches)
	}
}

func (s *Store) collectLocked() error {
	rows, err := s.db.Query("SELECT hash,size FROM objects WHERE NOT EXISTS(SELECT 1 FROM resources WHERE resources.hash=objects.hash)")
	if err != nil {
		return err
	}
	type item struct {
		h string
		n int64
	}
	var items []item
	for rows.Next() {
		var x item
		if err = rows.Scan(&x.h, &x.n); err != nil {
			rows.Close()
			return err
		}
		items = append(items, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, x := range items {
		if s.leases[x.h] > 0 {
			continue
		}
		if !validHash(x.h) {
			return errors.New("invalid indexed hash")
		}
		if err = os.Remove(s.path(x.h)); err != nil && !os.IsNotExist(err) {
			return err
		}
		if _, err = s.db.Exec("DELETE FROM objects WHERE hash=?", x.h); err != nil {
			return err
		}
		s.used -= x.n
	}
	return nil
}

func (s *Store) makeRoomLocked(extra int64) error {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(s.cfg.Dir, &fs); err != nil {
		return err
	}
	if fs.Bavail*uint64(fs.Bsize) < s.cfg.MinFreeBytes+uint64(extra) {
		return ErrCapacity
	}
	if s.used+s.temp+extra <= s.cfg.MaxBytes {
		return nil
	}
	s.flushTouchesLocked()
	rows, err := s.db.Query("SELECT key,hash FROM resources ORDER BY accessed ASC")
	if err != nil {
		return err
	}
	type pair struct{ k, h string }
	var victims []pair
	for rows.Next() {
		var p pair
		if err = rows.Scan(&p.k, &p.h); err != nil {
			rows.Close()
			return err
		}
		victims = append(victims, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if err = s.collectLocked(); err != nil {
		return err
	}
	for _, v := range victims {
		if s.used+s.temp+extra <= s.cfg.MaxBytes {
			return nil
		}
		if s.leases[v.h] > 0 {
			continue
		}
		if _, err = s.db.Exec("DELETE FROM resources WHERE key=?", v.k); err != nil {
			return err
		}
		if err = s.collectLocked(); err != nil {
			return err
		}
	}
	if s.used+s.temp+extra > s.cfg.MaxBytes {
		return ErrCapacity
	}
	return nil
}

// Maintain 定期持久化访问时间并回收无引用且未被读取的对象，阻塞至 ctx 被取消。
func (s *Store) Maintain(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			if !s.closed {
				s.flushTouchesLocked()
				s.collectLocked()
			}
			s.mu.Unlock()
		}
	}
}

func (s *Store) unlock() {
	if s.lock != nil {
		syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		s.lock.Close()
	}
}

// Close 持久化访问时间、关闭索引数据库并释放目录锁；重复调用返回 nil。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.flushTouchesLocked()
	err := s.db.Close()
	s.unlock()
	return err
}

// Key 对各组成部分进行无歧义编码并生成 SHA-256 缓存键；不得传入带签名的 CDN URL。
func Key(parts ...string) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

var _ io.Writer = (*Fill)(nil)
