package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"rtunnel/logging"
	"rtunnel/protocol"
)

// Client 负责监听本地 TCP 端口，并为每条入站连接与远端服务器建立可靠隧道。
type Client struct {
	remoteURL  string
	listenAddr string
	insecure   bool

	ctx    context.Context
	cancel context.CancelFunc

	dialer   *websocket.Dialer
	listener net.Listener

	sessionsMu sync.Mutex
	sessions   map[string]*protocol.Session

	wg       sync.WaitGroup
	stopOnce sync.Once
	stopped  atomic.Bool

	log *slog.Logger
}

func NewClient(remoteURL, listenAddr string, insecure bool) *Client {
	ctx, cancel := context.WithCancel(context.Background())

	dialer := &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  15 * time.Second,
		EnableCompression: false,
	}
	if insecure {
		dialer.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	return &Client{
		remoteURL:  remoteURL,
		listenAddr: listenAddr,
		insecure:   insecure,
		ctx:        ctx,
		cancel:     cancel,
		dialer:     dialer,
		sessions:   make(map[string]*protocol.Session),
		log:        logging.Logger().With("component", "client"),
	}
}

func (c *Client) Start() error {
	listener, err := net.Listen("tcp", c.listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", c.listenAddr, err)
	}
	c.listener = listener

	if err := c.probeRemote(); err != nil {
		_ = listener.Close()
		return fmt.Errorf("probe remote %s: %w", c.remoteURL, err)
	}

	c.log.Info("client started",
		"local_addr", listener.Addr().String(),
		"remote_url", c.remoteURL,
		"insecure", c.insecure,
	)

	for {
		conn, err := listener.Accept()
		if err != nil {
			if c.stopped.Load() {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return fmt.Errorf("accept: %w", err)
		}

		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.handleConnection(conn)
		}()
	}
}

// StartStdio 启动 stdio 模式的客户端，直接使用 stdin/stdout 作为连接
func (c *Client) StartStdio() error {
	if err := c.probeRemote(); err != nil {
		return fmt.Errorf("probe remote %s: %w", c.remoteURL, err)
	}

	c.log.Info("client started (stdio mode)",
		"remote_url", c.remoteURL,
		"insecure", c.insecure,
	)

	conn := NewStdioConn()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.handleConnection(conn)
	}()

	// 等待会话结束
	c.wg.Wait()
	return nil
}

func (c *Client) Stop() {
	c.stopOnce.Do(func() {
		c.stopped.Store(true)
		c.log.Info("stopping client")
		c.cancel()
		if c.listener != nil {
			_ = c.listener.Close()
		}

		c.sessionsMu.Lock()
		for id, session := range c.sessions {
			c.log.Debug("closing session", "session_id", id)
			session.Close()
		}
		c.sessions = make(map[string]*protocol.Session)
		c.sessionsMu.Unlock()

		c.wg.Wait()
	})
}

// handleConnection 针对单个本地连接创建可靠会话，并跟踪生命周期
func (c *Client) handleConnection(conn net.Conn) {
	defer conn.Close()

	sessionID := uuid.NewString()
	logger := c.log.With(
		"session_id", sessionID,
		"peer", conn.RemoteAddr().String(),
	)

	session, err := protocol.NewSession(c.ctx, protocol.SessionConfig{
		ID:      sessionID,
		TCPConn: conn,
		Logger:  logger,
	})
	if err != nil {
		logger.Error("create session failed", "error", err)
		return
	}

	c.trackSession(sessionID, session)
	defer c.untrackSession(sessionID, session)

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.manageWebSocket(sessionID, session, logger)
	}()

	<-session.Done()
	if err := session.Err(); err != nil && !errors.Is(err, context.Canceled) {
		logger.Warn("session ended with error", "error", err)
	} else {
		logger.Debug("session closed")
	}
}

// manageWebSocket 根据会话的 RequireWS 信号拨号新的 WebSocket 并注入
func (c *Client) manageWebSocket(id string, session *protocol.Session, logger *slog.Logger) {
	firstDial := true
	for {
		select {
		case <-c.ctx.Done():
			session.Close()
			return
		case <-session.Done():
			return
		case <-session.RequireWS:
			if session.IsClosed() {
				return
			}
			for {
				if c.stopped.Load() {
					session.Close()
					return
				}
				select {
				case <-c.ctx.Done():
					session.Close()
					return
				case <-session.Done():
					return
				default:
				}
				ws, err := c.dialWebSocket(id, firstDial)
				if err != nil {
					logger.Warn("dial websocket failed", "error", err)
					select {
					case <-time.After(time.Second):
					case <-c.ctx.Done():
						session.Close()
						return
					}
					continue
				}
				if session.IsClosed() {
					_ = ws.Close()
					return
				}
				if firstDial {
					firstDial = false
				}
				session.SetWebSocket(ws)
				logger.Debug("websocket established")
				break
			}
		}
	}
}

func (c *Client) trackSession(id string, s *protocol.Session) {
	c.sessionsMu.Lock()
	c.sessions[id] = s
	c.sessionsMu.Unlock()
}

func (c *Client) untrackSession(id string, s *protocol.Session) {
	c.sessionsMu.Lock()
	delete(c.sessions, id)
	c.sessionsMu.Unlock()
	s.Close()
}

func (c *Client) dialWebSocket(sessionID string, firstDial bool) (*websocket.Conn, error) {
	header := http.Header{
		"X-Session-ID": {sessionID},
	}
	header.Set("X-Session-First", strconv.FormatBool(firstDial))
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()

	ws, resp, err := c.dialer.DialContext(ctx, c.remoteURL, header)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("status %d: %w", resp.StatusCode, err)
		}
		return nil, err
	}
	return ws, nil
}

func (c *Client) probeRemote() error {
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()

	ws, resp, err := c.dialer.DialContext(ctx, c.remoteURL, http.Header{
		"X-Session-ID": {"myth-chatbot"},
	})
	if err != nil {
		if resp != nil {
			return fmt.Errorf("status %d: %w", resp.StatusCode, err)
		}
		return err
	}
	defer ws.Close()

	deadline := time.Now().Add(2 * time.Second)
	return ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "probe"), deadline)
}
