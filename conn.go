package rudp

import (
	"bytes"
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
	clientOrServer        uint8         // 是客户端，还是服务端的Conn
	state                 uint8         // 状态
	lock                  sync.RWMutex  // 同步锁，读/写缓存相关数据锁
	cToken, sToken        uint32        // 客户端/服务端产生的token
	closeSignal           chan struct{} // 关闭的信号
	connectSignal         chan struct{} // 建立连接的信号
	ioBytes               uint64RW      // io读写总字节
	time                  time.Time     // 更新的时间
	localListenAddr       *net.UDPAddr  // 本地监听地址
	remoteListenAddr      *net.UDPAddr  // 对方监听地址
	localInternetAddr     *net.UDPAddr  // 本地互联网地址
	remoteInternetAddr    *net.UDPAddr  // 对方互联网地址
	localMss              uint16        // 本地mss
	remoteMss             uint16        // 对方mss
	localMaxBuff          uint32RW      // 本地读写缓存最大容量（字节）
	remoteMaxBuff         uint32RW      // 对方读写缓存最大容量（字节）
	readLock              sync.RWMutex  // 读缓存锁
	writeLock             sync.RWMutex  // 写缓存锁
	readQueue             *readData     // 读缓存队列
	readSN                uint32        // 已确认的连续的最大的数据包序号
	minReadSN, maxReadSN  uint32        // 可以接收的数据包的序号范围，用于判断
	writeQueue            *writeData    // 写缓存队列
	queueLen, queueMaxLen uint32RW      // 本地读写缓存队列长度/最大长度
	pingId                uint32        // ping消息的id，递增
	remoteRemains         uint32        // 对方读缓存剩余的长度
	rto                   time.Duration // 超时重发
	rtt, rttVar           time.Duration // 实时/平均RTT，用于计算rto
	minRTO, maxRTO        time.Duration // 最小rto，不要发送"太快"，最大rto，不要出现"假死"
	ackId                 uint32
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
	close(this.closeSignal)
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

// 本地的公网地址
func (this *Conn) LocalInternetAddr() net.Addr {
	return this.lAddr
}

// 对方的监听地址
func (this *Conn) RemoteListenAddr() net.Addr {
	return this.rAddr
}

// 本地地址是否nat(Network Address Translation)
func (this *Conn) IsNat() bool {
	return bytes.Equal(this.lIntAddr.IP.To16(), this.lNatAddr.IP.To16()) &&
		this.lIntAddr.Port == this.lNatAddr.Port
}

// 对方地址是否在nat(Network Address Translation)
func (this *Conn) IsRemoteNat() bool {
	return bytes.Equal(this.rIntAddr.IP.To16(), this.rNatAddr.IP.To16()) &&
		this.rIntAddr.Port == this.rNatAddr.Port
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

// 根据实时的rtt来计算rto，使用的是tcp那套算法
func (this *Conn) calcRTO(rtt time.Duration) {
	this.rttVar = (3*this.rttVar + this.rtt - rtt) / 4
	this.rtt = (7*this.rtt + rtt) / 8
	this.rto = this.rtt + 4*this.rttVar
	if this.rto > this.maxRTO {
		this.rto = this.maxRTO
	}
	if this.rto < this.minRTO {
		this.rto = this.minRTO
	}
}

// 处理msgData，并编码msgAck到msg中
func (this *Conn) readMsgData(msg *udpData) {
	this.rLock.Lock()
	// 数据包序号
	sn := binary.BigEndian.Uint32(msg.buf[msgDataSN:])
	// 初始化ack消息
	this.writeMsgAck(sn, msg)
	this.rLock.Unlock()
}

// 处理msgAck
func (this *Conn) readMsgAck(msg *udpData) {
	this.rLock.Lock()
	// 数据包序号
	sn := binary.BigEndian.Uint32(msg.buf[msgDataSN:])
	// 初始化ack消息
	this.writeMsgAck(sn, msg)
	this.rLock.Unlock()
}

// 编码msgDial
func (this *Conn) writeMsgDial(msg *udpData) {
	msg.buf[msgType] = msgDial
	binary.BigEndian.PutUint32(msg.buf[msgDialVersion:], msgVersion)
	copy(msg.buf[msgDialLocalIP:], this.lNatAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.buf[msgDialLocalPort:], uint16(this.lNatAddr.Port))
	copy(msg.buf[msgDialRemoteIP:], this.rIntAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.buf[msgDialRemotePort:], uint16(this.rIntAddr.Port))
	binary.BigEndian.PutUint16(msg.buf[msgDialMSS:], this.lMss)
	binary.BigEndian.PutUint32(msg.buf[msgDialReadBuffer:], this.lMaxBuff.r)
	binary.BigEndian.PutUint32(msg.buf[msgDialWriteBuffer:], this.lMaxBuff.w)
}

// 解码码msgDial
func (this *Conn) readMsgDial(msg *udpData) {
	// 对方的监听地址
	this.rNatAddr = new(net.UDPAddr)
	this.rNatAddr.IP = append(this.lIntAddr.IP, msg.buf[msgDialLocalIP:msgDialLocalPort]...)
	this.rNatAddr.Port = int(binary.BigEndian.Uint16(msg.buf[msgDialLocalPort:]))
	// 本地的公网地址
	this.lIntAddr = new(net.UDPAddr)
	this.lIntAddr.IP = append(this.lIntAddr.IP, msg.buf[msgDialRemoteIP:msgDialRemotePort]...)
	this.lIntAddr.Port = int(binary.BigEndian.Uint16(msg.buf[msgDialRemotePort:]))
	// 对方的mss
	this.rMss = binary.BigEndian.Uint16(msg.buf[msgDialMSS:])
	// 对方的读写缓存
	this.rMaxBuff.r = binary.BigEndian.Uint32(msg.buf[msgDialReadBuffer:])
	this.rMaxBuff.w = binary.BigEndian.Uint32(msg.buf[msgDialWriteBuffer:])
}

// 编码msgAccept
func (this *Conn) writeMsgAccept(msg *udpData) {
	msg.buf[msgType] = msgAccept
	binary.BigEndian.PutUint32(msg.buf[msgAcceptCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.buf[msgAcceptSToken:], this.sToken)
	copy(msg.buf[msgAcceptLocalIP:], this.lNatAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.buf[msgAcceptLocalPort:], uint16(this.lNatAddr.Port))
	copy(msg.buf[msgAcceptRemoteIP:], this.rIntAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.buf[msgAcceptRemotePort:], uint16(this.rIntAddr.Port))
	binary.BigEndian.PutUint16(msg.buf[msgAcceptMSS:], this.lMss)
	binary.BigEndian.PutUint32(msg.buf[msgAcceptReadBuffer:], this.lMaxBuff.r)
	binary.BigEndian.PutUint32(msg.buf[msgAcceptWriteBuffer:], this.lMaxBuff.w)
}

// 解码msgAccept
func (this *Conn) readMsgAccept(msg *udpData) {
	// 对方的监听地址
	this.rNatAddr = new(net.UDPAddr)
	this.rNatAddr.IP = append(this.lIntAddr.IP, msg.buf[msgAcceptLocalIP:msgAcceptLocalPort]...)
	this.rNatAddr.Port = int(binary.BigEndian.Uint16(msg.buf[msgAcceptLocalPort:]))
	// 本地的公网地址
	this.lIntAddr = new(net.UDPAddr)
	this.lIntAddr.IP = append(this.lIntAddr.IP, msg.buf[msgAcceptRemoteIP:msgAcceptRemotePort]...)
	this.lIntAddr.Port = int(binary.BigEndian.Uint16(msg.buf[msgAcceptRemotePort:]))
	// 对方的mss
	this.rMss = binary.BigEndian.Uint16(msg.buf[msgAcceptMSS:])
	// 对方的读写缓存
	this.rMaxBuff.r = binary.BigEndian.Uint32(msg.buf[msgAcceptReadBuffer:])
	this.rMaxBuff.w = binary.BigEndian.Uint32(msg.buf[msgAcceptWriteBuffer:])
}

// 编码msgConnect
func (this *Conn) writeMsgConnect(msg *udpData) {
	msg.buf[msgType] = msgConnect
	binary.BigEndian.PutUint32(msg.buf[msgConnectCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.buf[msgConnectSToken:], this.sToken)
}

// 编码msgAck
func (this *Conn) writeMsgAck(sn uint32, msg *udpData) {
	msg.buf[msgType] = msgAck[this.cs]
	binary.BigEndian.PutUint32(msg.buf[msgAckCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.buf[msgAckSToken:], this.sToken)
	binary.BigEndian.PutUint32(msg.buf[msgAckSN:], sn)
	binary.BigEndian.PutUint32(msg.buf[msgAckMaxSN:], this.rBuf.nextSN-1)
	binary.BigEndian.PutUint32(msg.buf[msgAckBuffer:], this.rBuf.maxLen-this.rBuf.len)
	binary.BigEndian.PutUint64(msg.buf[msgAckId:], this.rBuf.ackId)
	this.ackId++
}

// 编码msgPing
func (this *Conn) writeMsgPing(msg *udpData) {
	msg.buf[msgType] = msgAck[this.cs]
	binary.BigEndian.PutUint32(msg.buf[msgAckCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.buf[msgAckSToken:], this.sToken)
	binary.BigEndian.PutUint32(msg.buf[msgAckSN:], sn)
	binary.BigEndian.PutUint32(msg.buf[msgAckMaxSN:], this.rBuf.nextSN-1)
	binary.BigEndian.PutUint32(msg.buf[msgAckBuffer:], this.rBuf.maxLen-this.rBuf.len)
	binary.BigEndian.PutUint64(msg.buf[msgAckId:], this.rBuf.ackId)
	this.ackId++
}

// 解码msgPing
func (this *Conn) readMsgPing(msg *udpData) {
	msg.buf[msgType] = msgAck[this.cs]
	binary.BigEndian.PutUint32(msg.buf[msgAckCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.buf[msgAckSToken:], this.sToken)
	binary.BigEndian.PutUint32(msg.buf[msgAckSN:], sn)
	binary.BigEndian.PutUint32(msg.buf[msgAckMaxSN:], this.rBuf.nextSN-1)
	binary.BigEndian.PutUint32(msg.buf[msgAckBuffer:], this.rBuf.maxLen-this.rBuf.len)
	binary.BigEndian.PutUint64(msg.buf[msgAckId:], this.rBuf.ackId)
	this.ackId++
	this.writeMsgPong(msg)
}

// 编码msgPong
func (this *Conn) writeMsgPong(msg *udpData) {
	msg.buf[msgType] = msgAck[this.cs]
	binary.BigEndian.PutUint32(msg.buf[msgAckCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.buf[msgAckSToken:], this.sToken)
	binary.BigEndian.PutUint32(msg.buf[msgAckSN:], sn)
	binary.BigEndian.PutUint32(msg.buf[msgAckMaxSN:], this.rBuf.nextSN-1)
	binary.BigEndian.PutUint32(msg.buf[msgAckBuffer:], this.rBuf.maxLen-this.rBuf.len)
	binary.BigEndian.PutUint64(msg.buf[msgAckId:], this.rBuf.ackId)
	this.ackId++
}

// 编码msgPong
func (this *Conn) readMsgPong(msg *udpData) {
	msg.buf[msgType] = msgAck[this.cs]
	binary.BigEndian.PutUint32(msg.buf[msgAckCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.buf[msgAckSToken:], this.sToken)
	binary.BigEndian.PutUint32(msg.buf[msgAckSN:], sn)
	binary.BigEndian.PutUint32(msg.buf[msgAckMaxSN:], this.rBuf.nextSN-1)
	binary.BigEndian.PutUint32(msg.buf[msgAckBuffer:], this.rBuf.maxLen-this.rBuf.len)
	binary.BigEndian.PutUint64(msg.buf[msgAckId:], this.rBuf.ackId)
	this.ackId++
}
