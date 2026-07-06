package remote

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"

	"github.com/gorilla/websocket"
)

type codexWSReadCloser struct {
	reader io.ReadCloser
	closer func() error
}

func (c *codexWSReadCloser) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func (c *codexWSReadCloser) Close() error {
	if c.closer != nil {
		return c.closer()
	}
	if c.reader != nil {
		return c.reader.Close()
	}
	return nil
}

type codexWSWriteCloser struct {
	mu        sync.Mutex
	conn      *websocket.Conn
	pipeClose func() error
	closeOnce sync.Once
}

func (c *codexWSWriteCloser) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return 0, io.ErrClosedPipe
	}

	payload := bytes.TrimSpace(p)
	if len(payload) == 0 {
		return len(p), nil
	}
	if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *codexWSWriteCloser) Close() error {
	var result error
	c.closeOnce.Do(func() {
		c.mu.Lock()
		conn := c.conn
		c.conn = nil
		c.mu.Unlock()
		if conn != nil {
			result = conn.Close()
		}
		if c.pipeClose != nil {
			if err := c.pipeClose(); result == nil {
				result = err
			}
		}
	})
	return result
}

func allocateCodexWSURL() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer listener.Close()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || addr.Port == 0 {
		return "", fmt.Errorf("failed to allocate codex app-server port")
	}
	return fmt.Sprintf("ws://127.0.0.1:%d", addr.Port), nil
}

func dialCodexWS(wsURL string) (*websocket.Conn, error) {
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 2 * time.Second
	dialer.NetDialContext = (&net.Dialer{
		Timeout:   2 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext

	var lastErr error
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, _, err := dialer.Dial(strings.TrimSpace(wsURL), nil)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(150 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("codex app-server unavailable")
	}
	return nil, lastErr
}

func (s *Service) startCodexWSBridge(cmd *exec.Cmd, conn *websocket.Conn) (io.WriteCloser, io.ReadCloser) {
	pipeReader, pipeWriter := io.Pipe()
	writer := &codexWSWriteCloser{
		conn: conn,
		pipeClose: func() error {
			return pipeWriter.Close()
		},
	}
	reader := &codexWSReadCloser{
		reader: pipeReader,
		closer: func() error {
			closeErr := writer.Close()
			readErr := pipeReader.Close()
			if closeErr != nil {
				return closeErr
			}
			return readErr
		},
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer pipeWriter.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				if s.isCurrentCommand(cmd) {
					applog.Errorf("[Remote] codex websocket read error: %v", err)
				}
				return
			}
			if len(data) == 0 {
				continue
			}
			if !bytes.HasSuffix(data, []byte{'\n'}) {
				data = append(data, '\n')
			}
			if _, err := pipeWriter.Write(data); err != nil {
				return
			}
		}
	}()

	return writer, reader
}
