package rudp

import (
	"encoding/binary"
	"net"
	"sync"
	"time"
)

type connState int

// Conn的状态
const (
	connStateClose   connState = iota // c<->s，连接超时/被应用层关闭，发送close消息
	connStateDial                     // c->s，发送dial消息
	connStateAccept                   // s->c，确认连接，发送accept消息
	connStateConnect                  // c->s，发送connect消息，双向确认连接
)

// 创建一个新的Conn变量
func (this *RUDP) newConn(state connState, rAddr *net.UDPAddr, mss uint16) *Conn {
	conn := new(Conn)
	conn.state = state
	conn.rAddr = rAddr
	conn.lAddr = this.conn.LocalAddr().(*net.UDPAddr)
	conn.rBuf = newReadBuffer()
	conn.wBuf = newWriteBuffer(mss)
	conn.connectSignal = make(chan struct{})
	conn.closeSignal = make(chan struct{})
	return conn
}

// 保存连接的基本信息，实现可靠性算法
type Conn struct {
	lock          sync.RWMutex
	state         connState      // 状态
	rBuf          *readBuffer    // 读缓存
	wBuf          *writeBuffer   // 写缓存
	dataBytes     rwBytes        // 有效数据总字节
	totalBytes    rwBytes        // 读写总字节
	connectSignal chan struct{}  // 作为确定连接的信号
	closeSignal   chan struct{}  // 关闭的信号
	wait          sync.WaitGroup // 等待所有协程退出
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

// 设置读缓存大小（字节）
func (this *Conn) SetReadBuffer(n int) error {
	this.rBuf.Lock()
	this.rBuf.cap = uint32(n)
	this.rBuf.Unlock()
	return nil
}

// 设置写缓存大小（字节）
func (this *Conn) SetWriteBuffer(n int) error {
	this.wBuf.Lock()
	this.wBuf.cap = uint32(n)
	this.wBuf.Unlock()
	return nil
}

// net.Conn接口
func (this *Conn) Read(b []byte) (int, error) {
	this.rBuf.RLock()
	timeout := this.rBuf.timeout
	this.rBuf.RUnlock()
	// 没有设置超时
	if timeout.IsZero() {
		for {
			select {
			case <-this.rBuf.enable:
				n := this.rBuf.Read(b)
				if n > 0 {
					return n, nil
				}
			case <-this.connectSignal:
				return 0, this.netOpError("read", closeErr("conn"))
			}
		}
	}
	// 已经超时，不需要以下的步骤
	duration := timeout.Sub(time.Now())
	if duration <= 0 {
		return 0, this.netOpError("read", opErr("timeout"))
	}
	for {
		select {
		case <-this.rBuf.enable:
			n := this.rBuf.Read(b)
			if n > 0 {
				return n, nil
			}
		case <-this.connectSignal:
			return 0, this.netOpError("read", closeErr("conn"))
		case <-time.After(duration):
			return 0, this.netOpError("read", opErr("timeout"))
		}
	}
}

// net.Conn接口
func (this *Conn) Write(b []byte) (int, error) {
	this.wBuf.RLock()
	timeout := this.wBuf.timeout
	this.wBuf.RUnlock()
	m := len(b)
	// 没有设置超时
	if timeout.IsZero() {
		for {
			select {
			case <-this.wBuf.enable:
				n := this.wBuf.Write(b)
				b = b[n:]
				if len(b) < 1 {
					return m, nil
				}
			case <-this.connectSignal:
				return 0, this.netOpError("write", closeErr("conn"))
			}
		}
	}
	// 已经超时，不需要以下的步骤
	duration := timeout.Sub(time.Now())
	if duration <= 0 {
		return 0, this.netOpError("write", opErr("timeout"))
	}
	for {
		select {
		case <-this.wBuf.enable:
			n := this.wBuf.Write(b)
			b = b[n:]
			if len(b) < 1 {
				return m, nil
			}
		case <-this.connectSignal:
			return 0, this.netOpError("write", closeErr("conn"))
		case <-time.After(duration):
			return 0, this.netOpError("write", opErr("timeout"))
		}
	}
}

// net.Conn接口
func (this *Conn) Close() error {
	this.lock.Lock()
	if this.state == connStateClose {
		this.lock.Unlock()
		return this.netOpError("close", closeErr("conn"))
	}
	this.state = connStateClose
	this.lock.Unlock()
	close(this.connectSignal)
	this.rBuf.cond.Signal()
	this.wBuf.cond.Signal()
	this.wait.Wait()
	return nil
}

// net.Conn接口
func (this *Conn) LocalAddr() net.Addr {
	return this.lAddr
}

// net.Conn接口
func (this *Conn) RemoteAddr() net.Addr {
	return this.rAddr
}

// net.Conn接口
func (this *Conn) SetDeadline(t time.Time) error {
	this.SetReadDeadline(t)
	this.SetWriteDeadline(t)
	return nil
}

// net.Conn接口
func (this *Conn) SetReadDeadline(t time.Time) error {
	this.rBuf.Lock()
	this.rBuf.timeout = t
	this.rBuf.Unlock()
	return nil
}

// net.Conn接口
func (this *Conn) SetWriteDeadline(t time.Time) error {
	this.wBuf.Lock()
	this.wBuf.timeout = t
	this.wBuf.Unlock()
	return nil
}

// 编码dial消息
func (this *Conn) dialMsg(msg *udpData) {
	msg.b[msgType] = msgDial
	binary.BigEndian.PutUint32(msg.b[msgDialVersion:], msgVersion)
	copy(msg.b[msgDialLocalIP:], this.lAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.b[msgDialLocalPort:], uint16(this.lAddr.Port))
	binary.BigEndian.PutUint32(msg.b[msgDialReadBuffer:], this.rBuf.Left())
	binary.BigEndian.PutUint32(msg.b[msgDialWriteBuffer:], this.wBuf.Left())
	msg.n = msgDialLength
	msg.a = this.rAddr
}

// 编码accept消息
func (this *Conn) acceptMsg(msg *udpData) {
	msg.b[msgType] = msgAccept
	binary.BigEndian.PutUint32(msg.b[msgAcceptCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.b[msgAcceptSToken:], this.sToken)
	copy(msg.b[msgAcceptClientIP:], this.rAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.b[msgAcceptClientPort:], uint16(this.rAddr.Port))
	binary.BigEndian.PutUint32(msg.b[msgAcceptReadBuffer:], this.rBuf.Left())
	binary.BigEndian.PutUint32(msg.b[msgAcceptWriteBuffer:], this.wBuf.Left())
	msg.n = msgAckLength
	msg.a = this.rAddr
}
