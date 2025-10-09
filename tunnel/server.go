package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"rtunnel/logging"
	"rtunnel/protocol"
)

// Server 暴露 WebSocket 端点，将其与目标 TCP 服务之间的数据通过可靠会话转发。
type Server struct {
	ListenAddr string
	TargetAddr string
	CertFile   string
	KeyFile    string

	ctx    context.Context
	cancel context.CancelFunc

	httpServer *http.Server

	sessionsMu sync.Mutex
	sessions   map[string]*protocol.Session

	wg       sync.WaitGroup
	stopOnce sync.Once

	log *slog.Logger
}

func NewServer(target, listen string) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		ListenAddr: listen,
		TargetAddr: target,
		ctx:        ctx,
		cancel:     cancel,
		sessions:   make(map[string]*protocol.Session),
		log:        logging.Logger().With("component", "server"),
	}
}

func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHTTP)

	s.httpServer = &http.Server{
		Addr:              s.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return s.ctx },
	}

	go s.cleanupLoop()

	s.log.Info("server started",
		"listen_addr", s.ListenAddr,
		"target_addr", s.TargetAddr,
		"tls_enabled", s.CertFile != "" && s.KeyFile != "",
	)

	var err error
	if s.CertFile != "" && s.KeyFile != "" {
		err = s.httpServer.ListenAndServeTLS(s.CertFile, s.KeyFile)
	} else {
		err = s.httpServer.ListenAndServe()
	}

	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	var result error
	s.stopOnce.Do(func() {
		s.log.Info("stopping server")
		s.cancel()
		if s.httpServer != nil {
			shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := s.httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				result = err
			}
		}

		s.sessionsMu.Lock()
		for id, session := range s.sessions {
			s.log.Debug("closing session", "session_id", id)
			session.Close()
		}
		s.sessions = make(map[string]*protocol.Session)
		s.sessionsMu.Unlock()

		s.wg.Wait()
	})
	return result
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		s.serveInfo(w, r)
		return
	}

	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		http.Error(w, "missing X-Session-ID header", http.StatusBadRequest)
		return
	}

	upgrader := websocket.Upgrader{
		ReadBufferSize:  32 * 1024,
		WriteBufferSize: 32 * 1024,
		CheckOrigin: func(*http.Request) bool {
			return true
		},
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Error("upgrade failed", "error", err, "remote", r.RemoteAddr)
		return
	}

	if sessionID == "myth-chatbot" {
		s.handleProbe(conn, r.RemoteAddr)
		return
	}

	session, fresh, err := s.getOrCreateSession(sessionID)
	if err != nil {
		s.log.Error("create session failed", "error", err, "session_id", sessionID)
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error()), time.Now().Add(2*time.Second))
		conn.Close()
		return
	}

	session.SetWebSocket(conn)
	if fresh {
		s.log.Info("session started", "session_id", sessionID, "client", r.RemoteAddr)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.watchSession(sessionID, session)
		}()
	} else {
		s.log.Debug("websocket reattached", "session_id", sessionID, "client", r.RemoteAddr)
	}
}

func (s *Server) serveInfo(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "rtunnel server\n")
	fmt.Fprintf(w, "target: %s\n", s.TargetAddr)
}

func (s *Server) handleProbe(ws *websocket.Conn, remote string) {
	defer ws.Close()
	s.log.Debug("probe connection", "remote", remote)
	deadline := time.Now().Add(2 * time.Second)
	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "probe"), deadline)
}

func (s *Server) getOrCreateSession(id string) (*protocol.Session, bool, error) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()

	if session, ok := s.sessions[id]; ok {
		return session, false, nil
	}

	conn, err := net.DialTimeout("tcp", s.TargetAddr, 10*time.Second)
	if err != nil {
		return nil, false, fmt.Errorf("dial target: %w", err)
	}

	logger := s.log.With(
		"session_id", id,
		"target", s.TargetAddr,
	)

	session, err := protocol.NewSession(s.ctx, protocol.SessionConfig{
		ID:      id,
		TCPConn: conn,
		Logger:  logger,
	})
	if err != nil {
		conn.Close()
		return nil, false, err
	}

	s.sessions[id] = session
	return session, true, nil
}

func (s *Server) watchSession(id string, session *protocol.Session) {
	defer func() {
		s.sessionsMu.Lock()
		delete(s.sessions, id)
		s.sessionsMu.Unlock()
		session.Close()
	}()

	for {
		select {
		case <-s.ctx.Done():
			session.Close()
			return
		case <-session.Done():
			if err := session.Err(); err != nil && !errors.Is(err, context.Canceled) {
				s.log.Warn("session ended with error", "session_id", id, "error", err)
			} else {
				s.log.Debug("session closed", "session_id", id)
			}
			return
		case <-session.RequireWS:
			if session.IsClosed() {
				continue
			}
			// Server can only wait for the client to reconnect; record the event once.
			s.log.Debug("session requires websocket", "session_id", id)
		}
	}
}

func (s *Server) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.cleanupStaleSessions()
		}
	}
}

func (s *Server) cleanupStaleSessions() {
	cutoff := time.Now().Add(-10 * time.Minute)

	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()

	for id, session := range s.sessions {
		if session.LastActive().Before(cutoff) {
			s.log.Info("closing inactive session", "session_id", id)
			session.Close()
			delete(s.sessions, id)
		}
	}
}
