package transfer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"hubp/internal/upstream"
)

type FetchFunc func(context.Context, http.Header) (*http.Response, error)

type Service struct {
	Cache        CacheStore
	WriteTimeout time.Duration
}

var buffers = sync.Pool{New: func() any { return make([]byte, 64<<10) }}

// CopyHeaders 将清理后的响应头复制到 dst，并复制各字段值以避免共享切片。
func CopyHeaders(dst, src http.Header) {
	for k, v := range upstream.CleanHeaders(src) {
		dst[k] = append([]string(nil), v...)
	}
}

// Error 记录上游错误并返回 502 响应；并发容量耗尽时返回 503。
func Error(w http.ResponseWriter, err error) {
	slog.Warn("upstream request failed", "error", err)
	status := http.StatusBadGateway
	if errors.Is(err, upstream.ErrBusy) {
		status = http.StatusServiceUnavailable
	}
	http.Error(w, http.StatusText(status), status)
}

// Pipe 调用 fetch 获取上游响应并流式转发，不读取或写入缓存。
func (s *Service) Pipe(w http.ResponseWriter, r *http.Request, fetch FetchFunc) {
	resp, err := fetch(r.Context(), nil)
	if err != nil {
		Error(w, err)
		return
	}
	defer resp.Body.Close()
	s.write(w, r, resp, nil)
}

func (s *Service) write(w http.ResponseWriter, r *http.Request, resp *http.Response, tap func([]byte)) {
	CopyHeaders(w.Header(), resp.Header)
	// A download must never silently escape to an origin or authentication endpoint.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "" {
		w.Header().Del("Location")
		http.Error(w, "unresolved upstream redirect", http.StatusBadGateway)
		return
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead || resp.StatusCode == http.StatusNotModified || resp.StatusCode == http.StatusNoContent {
		return
	}
	if err := s.copy(w, resp.Body, tap); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (s *Service) copy(w http.ResponseWriter, src io.Reader, tap func([]byte)) error {
	buf := buffers.Get().([]byte)
	defer buffers.Put(buf)
	rc := http.NewResponseController(w)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if s.WriteTimeout > 0 {
				_ = rc.SetWriteDeadline(time.Now().Add(s.WriteTimeout))
			}
			written, err := w.Write(buf[:n])
			if err != nil {
				return err
			}
			if written != n {
				return io.ErrShortWrite
			}
			if tap != nil {
				tap(buf[:n])
			}
			// Git and slow streaming APIs must not wait for the entire response to finish.
			if err = rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
