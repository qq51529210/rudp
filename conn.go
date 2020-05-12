package rudp

import (
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
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

type cs byte

const (
	csC cs = 0
	csS cs = 1
)

// 创建一个新的Conn变量
func (this *RUDP) newConn(state connState, rAddr *net.UDPAddr, mss uint16, cs cs) *Conn {
	conn := new(Conn)
	conn.cs = cs
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
	state         connState     // 状态
	rBuf          *readBuffer   // 读缓存
	wBuf          *writeBuffer  // 写缓存
	dataBytes     rwBytes       // 有效数据总字节
	totalBytes    rwBytes       // 读写总字节
	connectSignal chan struct{} // 作为确定连接的信号
	closeSignal   chan struct{} // 关闭的信号
	lAddr         *net.UDPAddr  // 本地地址
	rAddr         *net.UDPAddr  // 对端地址
	cToken        uint32        // client连接随机token
	sToken        uint32        // server连接随机token
	cs            cs            // 是客户端或者服务端的Conn
	pingId        uint32        // ping消息的id
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
				n := this.read(b)
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
			n := this.read(b)
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
				n := this.write(b)
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
			n := this.write(b)
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

// 写数据
func (this *Conn) write(buf []byte) (n int) {
	// 还能添加多少数据块
	m := this.wBuf.Left()
	if m < 1 {
		return
	}
	// 没有数据
	if this.wBuf.data == nil {
		d := this.wBuf.GetData(msgData[this.cs], this.sToken, buf)
		// 头
		this.wBuf.data = d
		// 尾=头
		this.wBuf.tail = this.wBuf.data
		m--
		n += d.data.n
		buf = buf[d.data.n:]
	}
	// 循环
	for m > 0 && len(buf) > 0 {
		d := this.wBuf.GetData(msgData[this.cs], this.sToken, buf)
		// 添加到尾部
		this.wBuf.tail.next = d
		this.wBuf.tail = this.wBuf.tail.next
		m--
		n += d.data.n
		buf = buf[d.data.n:]
	}
	return
}

// 读数据
func (this *Conn) read(buf []byte) (n int) {
	p := this.rBuf.data
	i := 0
	// 队列中，小于sn的都是可读的
	for p != nil && p.sn < this.rBuf.nextSN {
		// 拷贝
		i = copy(buf[n:], p.buf[p.idx:])
		n += i
		p.idx += i
		// 数据块读完了，移除，回收
		if len(p.buf) == p.idx {
			next := p.next
			this.rBuf.PutData(p)
			p = next
			this.rBuf.len--
		}
		// 缓存读满了
		if n == len(buf) {
			return
		}
	}
	return
}

// 添加数据块
func (this *Conn) addData(msg *udpData) {

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

// 编码connect消息
func (this *Conn) connectMsg(msg *udpData) {
	msg.b[msgType] = msgConnect
	binary.BigEndian.PutUint32(msg.b[msgConnectToken:], this.sToken)
	msg.n = msgConnectLength
	msg.a = this.rAddr
}

// 编码ack消息
func (this *Conn) ackMsg(sn uint32, msg *udpData) {
	msg.b[msgType] = msgAck[this.cs]
	binary.BigEndian.PutUint32(msg.b[msgAckToken:], this.sToken)
	binary.BigEndian.PutUint32(msg.b[msgAckSN:], sn)
	binary.BigEndian.PutUint32(msg.b[msgAckMaxSN:], this.rBuf.nextSN-1)
	binary.BigEndian.PutUint32(msg.b[msgAckBuffer:], this.rBuf.Left())
	binary.BigEndian.PutUint64(msg.b[msgAckId:], this.rBuf.ackId)
	this.rBuf.ackId++
	msg.n = msgAckLength
	msg.a = this.rAddr
}

// 编码close消息
func (this *Conn) closeMsg(msg *udpData) {
	msg.b[msgType] = msgClose[this.cs]
	binary.BigEndian.PutUint32(msg.b[msgCloseToken:], this.sToken)
	msg.n = msgCloseLength
	msg.a = this.rAddr
}

// 编码ping消息
func (this *Conn) pingMsg(msg *udpData) {
	id := atomic.AddUint32(&this.pingId, 1)
	msg.b[msgType] = msgPing
	binary.BigEndian.PutUint32(msg.b[msgPingToken:], this.sToken)
	binary.BigEndian.PutUint32(msg.b[msgPingId:], id)
	msg.n = msgPingLength
	msg.a = this.rAddr
}

// 编码pong消息
func (this *Conn) pongMsg(msg *udpData, id uint32) {
	msg.b[msgType] = msgPong
	binary.BigEndian.PutUint32(msg.b[msgPongToken:], this.sToken)
	binary.BigEndian.PutUint32(msg.b[msgPongId:], id)
	binary.BigEndian.PutUint32(msg.b[msgPongBuffer:], this.rBuf.cap-this.rBuf.len)
	msg.n = msgPongLength
	msg.a = this.rAddr
}
