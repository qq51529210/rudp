package rudp

import (
	"net"
	"sync"
	"time"
)

type connState int

const (
	connStateClose   connState = iota // c<->s，连接超时/被应用层关闭，发送close消息
	connStateDial                     // c->s，发送dial消息
	connStateAccept                   // s->c，确认连接，发送accept消息
	connStateConnect                  // c->s，发送connect消息，双向确认连接
)

// 创建一个新的Conn变量
func (this *RUDP) newConn(state connState, rAddr *net.UDPAddr) *Conn {
	conn := new(Conn)
	conn.connState = state
	conn.rAddr = rAddr
	conn.lAddr = this.conn.LocalAddr().(*net.UDPAddr)
	//conn.readBuffer.Cond = sync.NewCond(&conn.readBuffer.RWMutex)
	conn.readBuffer.enable = make(chan int)
	conn.SetReadBuffer(this.connReadBuffer)
	//conn.writeBuffer.Cond = sync.NewCond(&conn.writeBuffer.RWMutex)
	conn.writeBuffer.enable = make(chan int)
	conn.SetWriteBuffer(this.connWriteBuffer)
	conn.connectSignal = make(chan struct{})
	conn.closeSignal = make(chan struct{})
	return conn
}

// 保存连接的基本信息，实现可靠性算法
type Conn struct {
	connState                    // 状态
	*readBuffer                  // 读缓存
	*writeBuffer                 // 写缓存
	dataBytes     ioBytes        // 有效数据总字节
	totalBytes    ioBytes        // 读写总字节
	connectSignal chan struct{}  // 作为确定连接的信号
	closeSignal   chan struct{}  // 作为确定连接的信号
	waitQuit      sync.WaitGroup // 等待所有协程退出
	lAddr         *net.UDPAddr   // 本地地址
	rAddr         *net.UDPAddr   // 对端地址
	cToken        uint32         // client连接随机token
	sToken        uint32         // server连接随机token
}

// 返回net.OpError
func (this *Conn) netOpError(op string, err error) error {
	return &net.OpError{
		Op:     op,
		Net:    "udp",
		Source: this.rAddr,
		Addr:   this.lAddr,
		Err:    err,
	}
}

func (this *Conn) SetReadBuffer(n int) error {
	this.readBuffer.Lock()
	this.readBuffer.max = maxInt(1, n/int(this.mss))
	this.readBuffer.Unlock()
	return nil
}

func (this *Conn) SetWriteBuffer(n int) error {
	this.writeBuffer.Lock()
	this.writeBuffer.max = maxInt(1, n/int(this.mss))
	this.writeBuffer.Unlock()
	return nil
}

func (this *Conn) Read(b []byte) (int, error) {
	this.readBuffer.RLock()
	timeout := this.readBuffer.timeout
	this.readBuffer.RUnlock()
	if timeout.IsZero() {
		select {
		case <-this.readBuffer.enable:
			return this.readBuffer.read(b), nil
		case <-this.connectSignal:
			return 0, this.netError("read", errClosed("conn"))
		}
	}
	duration := timeout.Sub(time.Now())
	if duration <= 0 {
		return 0, this.netError("read", errOP("timeout"))
	}
	select {
	case <-this.readBuffer.enable:
		return this.readBuffer.read(b), nil
	case <-this.connectSignal:
		return 0, this.netError("read", errClosed("conn"))
	case <-time.After(duration):
		return 0, this.netError("read", errOP("timeout"))
	}
}

func (this *Conn) Write(b []byte) (int, error) {
	this.writeBuffer.RLock()
	timeout := this.writeBuffer.timeout
	this.writeBuffer.RUnlock()
	if timeout.IsZero() {
		select {
		case <-this.writeBuffer.enable:
			return this.write(b), nil
		case <-this.connectSignal:
			return 0, this.netError("read", errClosed("conn"))
		}
	}
	duration := timeout.Sub(time.Now())
	if duration <= 0 {
		return 0, this.netError("read", errOP("timeout"))
	}
	select {
	case <-this.writeBuffer.enable:
		return this.write(b), nil
	case <-this.connectSignal:
		return 0, this.netError("read", errClosed("conn"))
	case <-time.After(duration):
		return 0, this.netError("write", errOP("timeout"))
	}
}

func (this *Conn) write(b []byte) int {
	this.writeBuffer.Lock()
	this.writeBuffer.Unlock()
}

func (this *Conn) Close() error {
	return nil
}

func (this *Conn) LocalAddr() net.Addr {
	return this.lAddr
}

func (this *Conn) RemoteAddr() net.Addr {
	return this.rAddr
}

func (this *Conn) SetDeadline(t time.Time) error {
	this.rto = t
	this.wto = t
	return nil
}

func (this *Conn) SetReadDeadline(t time.Time) error {
	this.rto = t
	return nil
}

func (this *Conn) SetWriteDeadline(t time.Time) error {
	this.wto = t
	return nil
}

func (this *Conn) checkWriteBuffer(conn *net.UDPConn, now time.Time) error {
}
