package tunnel

import (
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// stdioConn 实现 net.Conn 接口，将 stdin/stdout 包装为网络连接
type stdioConn struct {
	reader io.Reader
	writer io.Writer
	once   sync.Once
	closed chan struct{}
}

// NewStdioConn 创建一个新的 stdio 连接
func NewStdioConn() net.Conn {
	return &stdioConn{
		reader: os.Stdin,
		writer: os.Stdout,
		closed: make(chan struct{}),
	}
}

func (c *stdioConn) Read(b []byte) (n int, err error) {
	return c.reader.Read(b)
}

func (c *stdioConn) Write(b []byte) (n int, err error) {
	return c.writer.Write(b)
}

func (c *stdioConn) Close() error {
	var err error
	c.once.Do(func() {
		close(c.closed)
		// 不关闭 stdin/stdout，因为它们可能被其他进程使用
	})
	return err
}

func (c *stdioConn) LocalAddr() net.Addr {
	return &stdioAddr{name: "stdin"}
}

func (c *stdioConn) RemoteAddr() net.Addr {
	return &stdioAddr{name: "stdout"}
}

func (c *stdioConn) SetDeadline(t time.Time) error {
	// stdio 不支持设置 deadline，返回 nil 表示忽略
	return nil
}

func (c *stdioConn) SetReadDeadline(t time.Time) error {
	// stdio 不支持设置 read deadline，返回 nil 表示忽略
	return nil
}

func (c *stdioConn) SetWriteDeadline(t time.Time) error {
	// stdio 不支持设置 write deadline，返回 nil 表示忽略
	return nil
}

// stdioAddr 实现 net.Addr 接口
type stdioAddr struct {
	name string
}

func (a *stdioAddr) Network() string {
	return "stdio"
}

func (a *stdioAddr) String() string {
	return a.name
}
