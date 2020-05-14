package rudp

import (
	"encoding/binary"
	"net"
	"sync"
	"time"
)

// Conn的状态
const (
	connStateClose   uint8 = iota // c<->s，连接超时/被应用层关闭，发送close消息
	connStateDial                 // c->s，发送dial消息
	connStateAccept               // s->c，确认连接，发送accept消息
	connStateConnect              // c->s，发送connect消息，双向确认连接
)

type Conn struct {
	lock, rLock, wLock sync.RWMutex  // 同步锁，读/写缓存相关数据锁
	cs                 uint8         // 是客户端，还是服务端的Conn
	state              uint8         // 状态
	lNatAddr, rNatAddr *net.UDPAddr  // 本地和对方，udp地址
	lIntAddr, rIntAddr *net.UDPAddr  // 本地和对方，公网地址
	ioBytes            uint64RW      // io读写总字节
	time               time.Time     // 更新的时间
	closeSignal        chan struct{} // 退出的信号
	connectSignal      chan struct{} // 作为确定连接的信号
	rMss, lMss         uint16        // 本地和对方，udp消息大小
	pingId             uint32        // ping消息的id，递增
	cToken, sToken     uint32        // 客户端/服务端产生的token
	lBuff              uint32RW      // 本地读写缓存队列实时容量（字节）
	lMaxBuff, rMaxBuff uint32RW      // 本地和对方的读写缓存队列最大容量（字节）
	rto                time.Duration // 超时重发
	rtt                time.Duration // 实时RTT，用于计算rto
	rttVar             time.Duration // 平均RTT，用于计算rto
	rQue               *readData     // 读缓存队列
	wQue               *writeData    // 写缓存队列
	rRemains           uint32        // 对方读缓存剩余的长度
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

// 编码msgDial
func (this *Conn) initMsgDial(msg *udpData) {
	msg.buf[msgType] = msgDial
	binary.BigEndian.PutUint32(msg.buf[msgDialVersion:], msgVersion)
	copy(msg.buf[msgDialLocalIP:], this.lAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.buf[msgDialLocalPort:], uint16(this.lAddr.Port))
	copy(msg.buf[msgDialRemoteIP:], this.rAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.buf[msgDialRemotePort:], uint16(this.rAddr.Port))
	binary.BigEndian.PutUint16(msg.buf[msgDialMSS:], this.lMss)
	binary.BigEndian.PutUint32(msg.buf[msgDialReadBuffer:], this.lMaxBuff.r)
	binary.BigEndian.PutUint32(msg.buf[msgDialWriteBuffer:], this.lMaxBuff.w)
}

// 编码msgAccept
func (this *Conn) initMsgAccept(msg *udpData) {
	msg.buf[msgType] = msgAccept
	binary.BigEndian.PutUint32(msg.buf[msgAcceptCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.buf[msgAcceptSToken:], this.sToken)
	copy(msg.buf[msgAcceptLocalIP:], this.lAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.buf[msgAcceptLocalPort:], uint16(this.lAddr.Port))
	copy(msg.buf[msgAcceptRemoteIP:], this.rAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.buf[msgAcceptRemotePort:], uint16(this.rAddr.Port))
	binary.BigEndian.PutUint16(msg.buf[msgAcceptMSS:], this.lMss)
	binary.BigEndian.PutUint32(msg.buf[msgAcceptReadBuffer:], this.lMaxBuff.r)
	binary.BigEndian.PutUint32(msg.buf[msgAcceptWriteBuffer:], this.lMaxBuff.w)
}
