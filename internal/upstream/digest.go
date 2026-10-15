package upstream

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"strings"
)

var ErrDigest = errors.New("upstream digest mismatch")

// Verify 为非 HEAD 的 200 响应包装响应体，在读取到 EOF 时校验 sha256 或 sha512 摘要。
// digest 为空时不校验；摘要不匹配的错误由后续的 Body.Read 返回。
func Verify(resp *http.Response, digest string) error {
	if resp.StatusCode != http.StatusOK || resp.Request.Method == http.MethodHead || digest == "" {
		return nil
	}
	alg, want, ok := strings.Cut(digest, ":")
	if !ok {
		return errors.New("invalid content digest")
	}
	var h hash.Hash
	switch alg {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return errors.New("unsupported content digest")
	}
	b, err := hex.DecodeString(want)
	if err != nil || len(b) != h.Size() {
		return errors.New("invalid content digest")
	}
	resp.Body = &verifiedBody{ReadCloser: resp.Body, h: h, want: strings.ToLower(want)}
	return nil
}

type verifiedBody struct {
	io.ReadCloser
	h        hash.Hash
	want     string
	finished bool
}

// Read 读取并累计内容摘要，在首次读到 EOF 时检查摘要是否匹配。
func (v *verifiedBody) Read(p []byte) (int, error) {
	n, err := v.ReadCloser.Read(p)
	v.h.Write(p[:n])
	if err == io.EOF && !v.finished {
		v.finished = true
		if hex.EncodeToString(v.h.Sum(nil)) != v.want {
			return n, ErrDigest
		}
	}
	return n, err
}
