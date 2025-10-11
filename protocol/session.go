package protocol

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sync/errgroup"

	"rtunnel/logging"
)

const (
	defaultWindowSize     = 128
	defaultReadBufferSize = 16 * 1024
	defaultMaxRecvBuffer  = 1024
	defaultPingInterval   = 30 * time.Second
	defaultIdleTimeout    = 90 * time.Second
	defaultInitialRTO     = time.Second
)

var (
	ErrSessionClosed = errors.New("session closed")
)

type SessionConfig struct {
	// ID 为会话标识（对应客户端/服务端共享的 X-Session-ID）
	ID string
	// TCPConn 为需要可靠传输的本地 TCP 连接
	TCPConn net.Conn
	// InitialWebSocket 用于提供第一条可用的 WebSocket（可为空）
	InitialWebSocket *websocket.Conn
	// Logger 可选的结构化日志
	Logger *slog.Logger

	// WindowSize 为滑动窗口可并发未确认的最大数据包数量
	WindowSize int
	// ReadBufferSize 为单次从 TCP 读取的数据块大小
	ReadBufferSize int
	// MaxRecvBuffer 为乱序缓存的最大数据包数量
	MaxRecvBuffer int
	// PingInterval/PingTimeout 用于心跳与空闲探测
	PingInterval time.Duration
	IdleTimeout  time.Duration
}

func (c *SessionConfig) setDefaults() {
	if c.WindowSize <= 0 {
		c.WindowSize = defaultWindowSize
	}
	if c.ReadBufferSize <= 0 {
		c.ReadBufferSize = defaultReadBufferSize
	}
	if c.MaxRecvBuffer <= 0 {
		c.MaxRecvBuffer = defaultMaxRecvBuffer
	}
	if c.PingInterval <= 0 {
		c.PingInterval = defaultPingInterval
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = defaultIdleTimeout
	}
}

func (c SessionConfig) validate() error {
	if c.ID == "" {
		return errors.New("session ID required")
	}
	if c.TCPConn == nil {
		return errors.New("tcp connection required")
	}
	return nil
}

type Session struct {
	// 下面的字段全部通过 context / 锁 管理，保证并发安全
	id string

	ctx    context.Context
	cancel context.CancelFunc

	log *slog.Logger

	tcp net.Conn

	RequireWS chan struct{}
	Closed    chan struct{}

	wsMu sync.Mutex
	// ws holds the active WebSocket connection; wsChanged is swapped on every change to wake waiters.
	ws        *websocket.Conn
	wsChanged chan struct{}

	stateMu    sync.Mutex
	sendSeq    uint64
	ackedSeq   uint64
	expected   uint64
	sendWindow map[uint64]*Packet
	recvBuffer map[uint64][]byte
	lastActive time.Time

	windowSlots chan struct{}
	outgoing    chan *Packet
	ackQueue    chan uint64
	resendCh    chan struct{}
	writerClose chan struct{}
	writerDone  chan struct{}

	srtt   time.Duration
	rttvar time.Duration
	rto    time.Duration
	timer  *time.Timer

	config SessionConfig

	errMu sync.Mutex
	err   error

	closed atomic.Bool
}

// NewSession 创建一个可靠传输会话，将 TCP 字节流封装为可靠的自定义报文，并通过 WebSocket 发送。
// - parent 为上游 context，关闭后会结束整个会话
// - cfg 需要提供会话 ID 以及本地 TCP 连接，其他参数可选
func NewSession(parent context.Context, cfg SessionConfig) (*Session, error) {
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)

	logger := cfg.Logger
	if logger == nil {
		logger = logging.Logger()
	}
	logger = logger.With("session_id", cfg.ID)

	s := &Session{
		id:          cfg.ID,
		ctx:         ctx,
		cancel:      cancel,
		log:         logger,
		tcp:         cfg.TCPConn,
		RequireWS:   make(chan struct{}, 1),
		Closed:      make(chan struct{}),
		sendWindow:  make(map[uint64]*Packet),
		recvBuffer:  make(map[uint64][]byte),
		windowSlots: make(chan struct{}, cfg.WindowSize),
		outgoing:    make(chan *Packet, cfg.WindowSize),
		ackQueue:    make(chan uint64, cfg.WindowSize),
		resendCh:    make(chan struct{}, 1),
		writerClose: make(chan struct{}),
		writerDone:  make(chan struct{}),
		sendSeq:     1,
		expected:    1,
		srtt:        100 * time.Millisecond,
		rttvar:      50 * time.Millisecond,
		rto:         defaultInitialRTO,
		lastActive:  time.Now(),
		config:      cfg,
	}
	// Initialize change notifier channel once and delegate setup to SetWebSocket
	s.wsChanged = make(chan struct{})
	s.SetWebSocket(cfg.InitialWebSocket)

	go s.run()
	return s, nil
}

func (s *Session) ID() string {
	return s.id
}

// run 启动所有工作 goroutine，并在任意一处失败时关闭整个会话
func (s *Session) run() {
	defer s.Close()

	group, ctx := errgroup.WithContext(s.ctx)
	group.Go(func() error { return s.tcpReader(ctx) })
	group.Go(func() error { return s.wsWriter(ctx) })
	group.Go(func() error { return s.wsReader(ctx) })
	group.Go(func() error { return s.keepAlive(ctx) })

	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		s.log.Debug("session loop ended", "error", err)
		s.setError(err)
	}
}

func (s *Session) LastActive() time.Time {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.lastActive
}

func (s *Session) SetWebSocket(ws *websocket.Conn) {
	if s.closed.Load() {
		if ws != nil {
			_ = ws.Close()
		}
		return
	}

	if ws != nil {
		s.attachWebSocket(ws)
	}

	s.wsMu.Lock()
	prev := s.ws
	s.ws = ws
	s.notifyWebSocketChangeLocked()
	s.wsMu.Unlock()

	if prev != nil && prev != ws {
		_ = prev.Close()
	}

	if ws == nil {
		s.requestNewWebSocket()
	} else {
		s.triggerResend()
	}
}

func (s *Session) attachWebSocket(ws *websocket.Conn) {
	ws.SetReadLimit(int64(s.config.ReadBufferSize) * 4)
	ws.SetPongHandler(func(appData string) error {
		s.onPong([]byte(appData))
		return nil
	})
}

func (s *Session) requestNewWebSocket() {
	if s.closed.Load() {
		return
	}
	// Non-blocking notification for the dialer loop to create or replace the WebSocket.
	select {
	case s.RequireWS <- struct{}{}:
	default:
	}
}

// waitForWebSocket blocks until a WebSocket is available or either context is canceled.
func (s *Session) waitForWebSocket(ctx context.Context) (*websocket.Conn, error) {
	for {
		s.wsMu.Lock()
		ws := s.ws
		waitCh := s.wsChanged
		closing := s.closed.Load()
		s.wsMu.Unlock()

		if ws != nil {
			return ws, nil
		}
		if closing {
			return nil, ErrSessionClosed
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.ctx.Done():
			return nil, ErrSessionClosed
		case <-waitCh:
		}
	}
}

func (s *Session) invalidateWebSocket(ws *websocket.Conn, err error) {
	if ws == nil {
		return
	}
	s.wsMu.Lock()
	if s.ws == ws {
		s.ws = nil
		s.notifyWebSocketChangeLocked()
	}
	s.wsMu.Unlock()
	_ = ws.Close()

	s.log.Debug("websocket closed", "error", err)
	s.requestNewWebSocket()
}

// tcpReader 负责从本地 TCP 读取数据，转成 DATA 包并放入发送窗口
func (s *Session) tcpReader(ctx context.Context) error {
	buf := make([]byte, s.config.ReadBufferSize)
	for {
		n, err := s.tcp.Read(buf)
		if err != nil {
			if !isExpectedNetErr(err) {
				err = fmt.Errorf("tcp read: %w", err)
			}
			// Close session to immediately unblock wsReader/wsWriter, then return.
			s.Close()
			return err
		}

		data := make([]byte, n)
		copy(data, buf[:n])

		select {
		case <-ctx.Done():
			return ctx.Err()
		case s.windowSlots <- struct{}{}:
		}

		s.stateMu.Lock()
		seq := s.sendSeq
		s.sendSeq++
		packet := NewDataPacket(seq, data)
		s.sendWindow[seq] = packet
		if len(s.sendWindow) == 1 {
			s.startRTOTimerLocked()
		}
		s.lastActive = time.Now()
		s.stateMu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case s.outgoing <- packet:
		}
	}
}

// wsWriter 串行负责以下事件：
// - 发送 TCP 方向的数据包
// - 回发 ACK
// - 定时发送 Ping
// - 根据 RTO 触发重传
func (s *Session) wsWriter(ctx context.Context) error {
	defer close(s.writerDone)

	heartbeat := time.NewTicker(s.config.PingInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ack := <-s.ackQueue:
			packet := NewAckPacket(ack)
			if err := s.writePacket(ctx, packet); err != nil {
				return err
			}
		case pkt := <-s.outgoing:
			if err := s.writePacket(ctx, pkt); err != nil {
				return err
			}
		case <-s.resendCh:
			if err := s.resendOutstanding(ctx); err != nil {
				return err
			}
		case <-heartbeat.C:
			if err := s.writePing(ctx); err != nil {
				return err
			}
		case <-s.writerClose:
			if err := s.writePacket(ctx, NewFinPacket()); err != nil &&
				err != ErrSessionClosed &&
				!errors.Is(err, context.Canceled) &&
				!isExpectedNetErr(err) {
				s.log.Debug("failed to send FIN", "error", err)
			}
			return nil
		}
	}
}

func (s *Session) writePacket(ctx context.Context, pkt *Packet) error {
	payload := pkt.Serialize()
	for {
		ws, err := s.waitForWebSocket(ctx)
		if err != nil {
			return err
		}
		if err := ws.WriteMessage(websocket.BinaryMessage, payload); err != nil {
			s.invalidateWebSocket(ws, err)
			continue
		}
		return nil
	}
}

func (s *Session) writePing(ctx context.Context) error {
	ts := make([]byte, 8)
	binary.BigEndian.PutUint64(ts, uint64(time.Now().UnixNano()))

	for {
		ws, err := s.waitForWebSocket(ctx)
		if err != nil {
			return err
		}
		if err := ws.WriteControl(websocket.PingMessage, ts, time.Now().Add(5*time.Second)); err != nil {
			s.invalidateWebSocket(ws, err)
			continue
		}
		return nil
	}
}

// resendOutstanding 会按照顺序重发所有未确认的数据包
func (s *Session) resendOutstanding(ctx context.Context) error {
	s.stateMu.Lock()
	if len(s.sendWindow) == 0 {
		s.stateMu.Unlock()
		return nil
	}
	sequences := make([]uint64, 0, len(s.sendWindow))
	for seq := range s.sendWindow {
		sequences = append(sequences, seq)
	}
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	packets := make([]*Packet, 0, len(sequences))
	for _, seq := range sequences {
		packets = append(packets, s.sendWindow[seq])
	}
	s.stateMu.Unlock()

	for _, pkt := range packets {
		if err := s.writePacket(ctx, pkt); err != nil {
			return err
		}
	}
	s.stateMu.Lock()
	s.startRTOTimerLocked()
	s.stateMu.Unlock()
	return nil
}

// wsReader 负责从 WebSocket 读取 PACKET，并根据类型分发处理
func (s *Session) wsReader(ctx context.Context) error {
	for {
		ws, err := s.waitForWebSocket(ctx)
		if err != nil {
			return err
		}
		mt, data, err := ws.ReadMessage()
		if err != nil {
			if !isExpectedNetErr(err) && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				err = fmt.Errorf("ws read: %w", err)
			}
			s.invalidateWebSocket(ws, err)
			continue
		}
		if mt != websocket.BinaryMessage {
			continue
		}

		packet, err := Deserialize(data)
		if err != nil {
			s.log.Warn("invalid packet", "error", err)
			continue
		}
		s.handleInboundPacket(packet)
	}
}

// keepAlive 根据空闲时间触发重传（防止网络中断后长时间无响应）
func (s *Session) keepAlive(ctx context.Context) error {
	ticker := time.NewTicker(s.config.IdleTimeout / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.stateMu.Lock()
			inactive := time.Since(s.lastActive)
			s.stateMu.Unlock()
			if inactive > s.config.IdleTimeout {
				s.triggerResend()
			}
		}
	}
}

// handleInboundPacket 根据报文类型路由到对应处理逻辑
func (s *Session) handleInboundPacket(pkt *Packet) {
	switch pkt.Flag {
	case PacketData:
		s.handleDataPacket(pkt)
	case PacketAck:
		s.handleAckPacket(pkt)
	case PacketFin:
		s.log.Debug("remote requested close")
		s.Close()
	}
}

// handleDataPacket 处理 DATA：
// - 与期望序号一致：写入 TCP，并检查缓冲区是否有连续数据
// - 大于期望：放入乱序缓冲
// - 小于期望：立即回 ACK 提醒对端
func (s *Session) handleDataPacket(pkt *Packet) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	s.lastActive = time.Now()

	if pkt.Seq < s.expected {
		select {
		case s.ackQueue <- s.expected - 1:
		default:
		}
		return
	}

	if pkt.Seq == s.expected {
		if err := s.writeToTCP(pkt.Data); err != nil {
			s.setError(err)
			s.cancel()
			return
		}
		s.expected++

		for {
			data, ok := s.recvBuffer[s.expected]
			if !ok {
				break
			}
			if err := s.writeToTCP(data); err != nil {
				s.setError(err)
				s.cancel()
				return
			}
			delete(s.recvBuffer, s.expected)
			s.expected++
		}
	} else {
		s.recvBuffer[pkt.Seq] = pkt.Data
		if len(s.recvBuffer) > s.config.MaxRecvBuffer {
			s.setError(errors.New("receive buffer overflow"))
			s.cancel()
			return
		}
	}

	select {
	case s.ackQueue <- s.expected - 1:
	default:
	}
}

// handleAckPacket 滑动发送窗口，释放并行度并更新 RTO
func (s *Session) handleAckPacket(pkt *Packet) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if pkt.Ack <= s.ackedSeq {
		s.triggerResend()
		return
	}

	for seq := s.ackedSeq + 1; seq <= pkt.Ack; seq++ {
		if _, ok := s.sendWindow[seq]; ok {
			delete(s.sendWindow, seq)
			select {
			case <-s.windowSlots:
			default:
			}
		}
	}

	s.ackedSeq = pkt.Ack
	if len(s.sendWindow) == 0 {
		s.stopRTOTimerLocked()
	} else {
		s.startRTOTimerLocked()
	}
}

func (s *Session) writeToTCP(data []byte) error {
	s.stateMu.Unlock()
	defer s.stateMu.Lock()

	if _, err := s.tcp.Write(data); err != nil {
		if !isExpectedNetErr(err) {
			return fmt.Errorf("tcp write: %w", err)
		}
		return err
	}
	return nil
}

func (s *Session) onPong(payload []byte) {
	if len(payload) != 8 {
		return
	}
	sent := int64(binary.BigEndian.Uint64(payload))
	rtt := time.Since(time.Unix(0, sent))
	if rtt <= 0 {
		return
	}

	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	const (
		alpha = 0.125
		beta  = 0.25
	)

	if s.srtt == 0 {
		s.srtt = rtt
		s.rttvar = rtt / 2
	} else {
		s.rttvar = time.Duration((1-beta)*float64(s.rttvar) + beta*mathAbsDuration(s.srtt-rtt))
		s.srtt = time.Duration((1-alpha)*float64(s.srtt) + alpha*float64(rtt))
	}
	s.rto = s.srtt + 4*s.rttvar
	if s.rto < 200*time.Millisecond {
		s.rto = 200 * time.Millisecond
	}
	if s.rto > 60*time.Second {
		s.rto = 60 * time.Second
	}
	s.lastActive = time.Now()
}

func (s *Session) startRTOTimerLocked() {
	if s.timer != nil {
		s.timer.Stop()
	}
	duration := s.rto
	if duration <= 0 {
		duration = defaultInitialRTO
	}
	s.timer = time.AfterFunc(duration, func() {
		s.triggerResend()
	})
}

func (s *Session) stopRTOTimerLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

func (s *Session) triggerResend() {
	select {
	case s.resendCh <- struct{}{}:
	default:
	}
}

func (s *Session) setError(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
}

func (s *Session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// IsClosed reports whether Close has been invoked.
func (s *Session) IsClosed() bool {
	return s.closed.Load()
}

func (s *Session) Close() {
	if s.closed.Load() {
		return
	}
	if !s.closed.CompareAndSwap(false, true) {
		return
	}

	if s.writerClose != nil {
		close(s.writerClose)
	}
	if s.writerDone != nil {
		select {
		case <-s.writerDone:
		case <-time.After(2 * time.Second):
			s.log.Warn("wsWriter did not shut down in time")
		}
	}

	s.cancel()

	select {
	case <-s.Closed:
	default:
		close(s.Closed)
	}

	ws := s.getAndClearWebSocket()
	if ws != nil {
		_ = ws.Close()
	}
	_ = s.tcp.Close()

	s.stateMu.Lock()
	s.stopRTOTimerLocked()
	s.stateMu.Unlock()
}

func (s *Session) getAndClearWebSocket() *websocket.Conn {
	s.wsMu.Lock()
	ws := s.ws
	s.ws = nil
	s.notifyWebSocketChangeLocked()
	s.wsMu.Unlock()
	return ws
}

func (s *Session) Done() <-chan struct{} {
	return s.Closed
}

func isExpectedNetErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return strings.Contains(err.Error(), "use of closed network connection")
}

func mathAbsDuration(d time.Duration) float64 {
	if d < 0 {
		return float64(-d)
	}
	return float64(d)
}

// notifyWebSocketChangeLocked closes the current change notification channel and creates a new one.
// Callers must hold wsMu while invoking it.
func (s *Session) notifyWebSocketChangeLocked() {
	if s.wsChanged != nil {
		close(s.wsChanged)
	}
	s.wsChanged = make(chan struct{})
}
