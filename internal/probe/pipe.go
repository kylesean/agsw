package probe

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// bufReader 包一层 bufio.Reader，便于把 CONNECT 握手后残留的字节
// 原样倒给上游，避免丢掉 TLS ClientHello 的一部分。
type bufReader struct {
	*bufio.Reader
}

func newBufReader(c net.Conn) *bufReader {
	return &bufReader{bufio.NewReaderSize(c, 64<<10)}
}

func (b *bufReader) ReadRequest() (*http.Request, error) {
	return http.ReadRequest(b.Reader)
}

func (b *bufReader) BufferedBytes() []byte {
	n := b.Buffered()
	if n == 0 {
		return nil
	}
	buf := make([]byte, n)
	_, _ = b.Reader.Read(buf)
	return buf
}

// pipe 双向搬运，任一方向 EOF 或 ctx 取消即结束。
func pipe(ctx context.Context, a, b net.Conn) {
	done := make(chan struct{})
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}
	defer closeBoth()

	var closeDone sync.Once
	notifyDone := func() {
		closeDone.Do(func() {
			close(done)
		})
	}

	go func() {
		defer notifyDone()
		_, _ = io.Copy(b, a)
		// 半关闭，让对端收到 EOF。
		if tc, ok := b.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	go func() {
		defer notifyDone()
		_, _ = io.Copy(a, b)
		if tc, ok := a.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	select {
	case <-ctx.Done():
	case <-done:
	}
	// 给另一方向一点时间收尾。
	time.Sleep(50 * time.Millisecond)
}
