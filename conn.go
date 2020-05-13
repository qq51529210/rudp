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
func (this *RUDP) newConn(cs cs, state connState, rAddr *net.UDPAddr) *Conn {
	conn := new(Conn)
	conn.cs = cs
	conn.state = state
	conn.rAddr = rAddr
	conn.lAddr = this.conn.LocalAddr().(*net.UDPAddr)
	conn.connectSignal = make(chan struct{})
	conn.closeSignal = make(chan struct{})
	conn.rBuf.enable = make(chan int, 1)
	conn.rBuf.head = new(readData)
	conn.wBuf.enable = make(chan int, 1)
	conn.wBuf.head = new(writeData)
	conn.wBuf.mss = DetectMSS(rAddr)
	return conn
}

// 保存连接的基本信息，实现可靠性算法
type Conn struct {
	lock          sync.RWMutex
	cs            cs            // 是客户端或者服务端的Conn
	state         connState     // 状态
	connectSignal chan struct{} // 作为确定连接的信号
	closeSignal   chan struct{} // 关闭的信号
	dataBytes     rwBytes       // 有效读写的总字节
	totalBytes    rwBytes       // 总读写字节
	rBuf          readBuffer    // 读缓存
	wBuf          writeBuffer   // 写缓存
	lAddr         *net.UDPAddr  // 本地地址
	rAddr         *net.UDPAddr  // 对端地址
	cToken        uint32        // client连接随机token
	sToken        uint32        // server连接随机token
	rReadBuf      uint32        // 对方的读缓存大小，目前没用处
	rWriteBuf     uint32        // 对方的写缓存大小，目前没用处
	pingId        uint32        // ping消息的id，递增
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
	this.rBuf.maxLen = calcBufferMaxLen(uint32(n), this.rBuf.mss)
	this.rBuf.Unlock()
	return nil
}

// 设置写缓存大小（字节）
func (this *Conn) SetWriteBuffer(n int) error {
	this.wBuf.Lock()
	this.wBuf.maxLen = calcBufferMaxLen(uint32(n), this.wBuf.mss)
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
	this.wBuf.Lock()
	// 还能添加多少数据块
	m := this.wBuf.maxLen - this.wBuf.len
	if m < 1 {
		this.wBuf.Unlock()
		return
	}
	// 没有数据
	p := this.wBuf.head
	// 循环
	for m > 0 && len(buf) > 0 {
		d := _writeDataPool.Get().(*writeData)
		// 基本信息
		d.sn = this.wBuf.sn
		d.first = time.Time{}
		d.last = time.Time{}
		// 直接编码data消息
		d.data.b[msgType] = msgData[this.cs]
		binary.BigEndian.PutUint32(p.data.b[msgDataToken:], this.sToken)
		binary.BigEndian.PutUint32(p.data.b[msgDataSN:], p.sn)
		d.data.n = copy(p.data.b[msgDataPayload:this.wBuf.mss], buf)
		// 递增
		this.wBuf.sn++
		this.wBuf.len++
		// 添加到尾部
		p.next = d
		p = p.next
		// 剩余
		m--
		buf = buf[d.data.n:]
		// 字节
		n += d.data.n
	}
	this.wBuf.tail = p
	this.wBuf.Unlock()
	return
}

// 读数据
func (this *Conn) read(buf []byte) (n int) {
	p := this.rBuf.head.next
	this.rBuf.Lock()
	i := 0
	// 小于nextSN的都是可读的
	for p != nil && p.sn < this.rBuf.nextSN {
		// 拷贝i个字节
		i = copy(buf[n:], p.buf[p.idx:])
		n += i
		p.idx += i
		// 数据块读完了，移除，回收
		if len(p.buf) == p.idx {
			p = p.next
			this.rBuf.len--
			// 窗口移动
			this.rBuf.minSN++
			this.rBuf.maxSN++
			_readDataPool.Put(p)
		}
		// 缓存读满了
		if n == len(buf) {
			break
		}
	}
	this.rBuf.head.next = p
	this.rBuf.Unlock()
	return
}

// 编码dial消息
func (this *Conn) initMsgDial(msg *udpData) {
	msg.b[msgType] = msgDial
	binary.BigEndian.PutUint32(msg.b[msgDialVersion:], msgVersion)
	copy(msg.b[msgDialLocalIP:], this.lAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.b[msgDialLocalPort:], uint16(this.lAddr.Port))
	binary.BigEndian.PutUint16(msg.b[msgDialMSS:], this.wBuf.mss)
	binary.BigEndian.PutUint32(msg.b[msgDialReadBuffer:], this.rBuf.maxLen-this.rBuf.len)
	binary.BigEndian.PutUint32(msg.b[msgDialWriteBuffer:], this.wBuf.maxLen-this.rBuf.len)
}

// 编码accept消息
func (this *Conn) acceptMsg(msg *udpData) {
	msg.b[msgType] = msgAccept
	binary.BigEndian.PutUint32(msg.b[msgAcceptCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.b[msgAcceptSToken:], this.sToken)
	copy(msg.b[msgAcceptClientIP:], this.rAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.b[msgAcceptClientPort:], uint16(this.rAddr.Port))
	binary.BigEndian.PutUint16(msg.b[msgAcceptMSS:], this.wBuf.mss)
	binary.BigEndian.PutUint32(msg.b[msgAcceptReadBuffer:], this.rBuf.maxLen-this.rBuf.len)
	binary.BigEndian.PutUint32(msg.b[msgAcceptWriteBuffer:], this.wBuf.maxLen-this.rBuf.len)
	msg.n = msgAckLength
	msg.a = this.rAddr
}

// 编码ack消息
func (this *Conn) ackMsg(sn uint32, msg *udpData) {
	msg.b[msgType] = msgAck[this.cs]
	binary.BigEndian.PutUint32(msg.b[msgAckToken:], this.sToken)
	binary.BigEndian.PutUint32(msg.b[msgAckSN:], sn)
	binary.BigEndian.PutUint32(msg.b[msgAckMaxSN:], this.rBuf.nextSN-1)
	binary.BigEndian.PutUint32(msg.b[msgAckBuffer:], this.rBuf.maxLen-this.rBuf.len)
	binary.BigEndian.PutUint64(msg.b[msgAckId:], this.rBuf.ackId)
	this.rBuf.ackId++
	msg.n = msgAckLength
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
	binary.BigEndian.PutUint32(msg.b[msgPongBuffer:], this.rBuf.maxLen-this.rBuf.len)
}
