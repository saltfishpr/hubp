package cache

import (
	"io"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCommitOpenAndServeRange(t *testing.T) {
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	temp, err := c.NewTemp("abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(temp, "0123456789"); err != nil {
		t.Fatal(err)
	}
	if err := temp.Close(); err != nil {
		t.Fatal(err)
	}
	entry := Entry{
		Status:    200,
		Headers:   map[string][]string{"Content-Type": {"application/octet-stream"}},
		CreatedAt: time.Now().UTC(),
	}
	if err := c.Commit("abcdef", temp.Name(), entry); err != nil {
		t.Fatal(err)
	}

	got, file, hit, err := c.Open("abcdef", 0)
	if err != nil || !hit {
		t.Fatalf("Open() hit=%v err=%v", hit, err)
	}
	defer file.Close()
	req := httptest.NewRequest("GET", "http://example.test/blob", nil)
	req.Header.Set("Range", "bytes=2-5")
	recorder := httptest.NewRecorder()
	Serve(recorder, req, got, file)
	response := recorder.Result()
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 206 || string(body) != "2345" {
		t.Fatalf("status=%d body=%q", response.StatusCode, body)
	}
}
