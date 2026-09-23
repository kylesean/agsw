// Package probe 提供拦截探针的网络请求与响应记录能力。
// 负责流量的原样记录与敏感字段脱敏。
package probe

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Record 是一条 JSONL 记录。
type Record struct {
	Time         string              `json:"time"`
	Kind         string              `json:"kind"` // request | upstream | connect | note
	Method       string              `json:"method,omitempty"`
	Host         string              `json:"host,omitempty"`
	Path         string              `json:"path,omitempty"`
	Query        string              `json:"query,omitempty"`
	Proto        string              `json:"proto,omitempty"`
	Headers      map[string][]string `json:"headers,omitempty"`
	Body         string              `json:"body,omitempty"`
	BodyEncoding string              `json:"body_encoding,omitempty"` // text | base64
	BodyLen      int                 `json:"body_len,omitempty"`      // 截断前的完整长度
	Target       string              `json:"target,omitempty"`        // CONNECT 的 host:port
	Status       int                 `json:"status,omitempty"`
	Note         string              `json:"note,omitempty"`
}

// sensitiveHeaders 这些头一律脱敏，避免把 token 打进日志。
// （本项目起因之一就是 secret-tool 把完整 OAuth token 打进了会话输出。）
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"x-goog-api-key":      true,
}

// RedactValue 对凭据值只保留 scheme 与前 6 字符 + 长度。
func RedactValue(v string) string {
	if v == "" {
		return ""
	}
	if i := strings.IndexByte(v, ' '); i > 0 {
		scheme, tok := v[:i], v[i+1:]
		p := tok
		if len(p) > 6 {
			p = p[:6]
		}
		return fmt.Sprintf("%s %s… (len=%d)", scheme, p, len(tok))
	}
	p := v
	if len(p) > 6 {
		p = p[:6]
	}
	return fmt.Sprintf("%s… (len=%d)", p, len(v))
}

func redactHeaders(h http.Header) map[string][]string {
	if h == nil {
		return nil
	}
	out := make(map[string][]string, len(h))
	for k, vals := range h {
		if sensitiveHeaders[strings.ToLower(k)] {
			cp := make([]string, len(vals))
			for i, v := range vals {
				cp[i] = RedactValue(v)
			}
			out[k] = cp
			continue
		}
		out[k] = append([]string(nil), vals...)
	}
	return out
}

func looksText(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return utf8.Valid(b)
}

// BodyField 把请求体转成可记录的字段，超长截断，二进制转 base64。
func BodyField(b []byte, max int) (val, enc string, full int) {
	full = len(b)
	if full == 0 {
		return "", "", 0
	}
	trunc := b
	if max > 0 && len(trunc) > max {
		trunc = trunc[:max]
	}
	if looksText(trunc) {
		return string(trunc), "text", full
	}
	return base64.StdEncoding.EncodeToString(trunc), "base64", full
}

// FromRequest 从 HTTP 请求构造一条已脱敏的记录。
func FromRequest(kind string, r *http.Request, body []byte, bodyMax int) Record {
	val, enc, full := BodyField(body, bodyMax)
	q := ""
	if r.URL != nil {
		q = r.URL.RawQuery
	}
	host := r.Host
	path := r.URL.Path
	return Record{
		Time:         time.Now().UTC().Format(time.RFC3339Nano),
		Kind:         kind,
		Method:       r.Method,
		Host:         host,
		Path:         path,
		Query:        q,
		Proto:        r.Proto,
		Headers:      redactHeaders(r.Header),
		Body:         val,
		BodyEncoding: enc,
		BodyLen:      full,
	}
}

// Recorder 把记录序列化成 JSONL。并发安全。
type Recorder struct {
	mu      sync.Mutex
	w       io.Writer
	bodyMax int
}

// New 创建记录器。bodyMax 是单条记录保留的请求体字节上限。
func New(w io.Writer, bodyMax int) *Recorder {
	return &Recorder{w: w, bodyMax: bodyMax}
}

// BodyMax 返回记录器的请求体上限，供 FromRequest 复用。
func (r *Recorder) BodyMax() int { return r.bodyMax }

// Add 写入一条记录。
func (r *Recorder) Add(rec Record) error {
	if rec.Time == "" {
		rec.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("序列化记录失败: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("写日志失败: %w", err)
	}
	return nil
}
