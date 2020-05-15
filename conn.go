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
	clientOrServer uint8         // 是客户端，还是服务端的Conn
	state          uint8         // 状态
	lock           sync.RWMutex  // 同步锁，读/写缓存相关数据锁
	cToken, sToken uint32        // 客户端/服务端产生的token
	closeSignal    chan struct{} // 关闭的信号
	connectSignal  chan int      // 建立连接的信号
	ioBytes        uint64RW      // io读写总字节
	time           time.Time     // 更新的时间

	localMss           uint16       // 本地mss
	localListenAddr    *net.UDPAddr // 本地监听地址
	localInternetAddr  *net.UDPAddr // 本地互联网地址
	localQueueLen      uint32RW     // 本地读写队列长度（窗口）
	localQueueMaxLen   uint32RW     // 本地读写队列最大长度（窗口）
	localAckId         uint32       // 本地的ack id
	remoteMss          uint16       // 对方mss
	remoteListenAddr   *net.UDPAddr // 对方监听地址
	remoteInternetAddr *net.UDPAddr // 对方互联网地址
	remoteQueueMaxLen  uint32RW     // 对方读写缓存最大容量（字节）
	remoteQueueRemains uint32       // 对方读缓存剩余的长度，ack会更新
	remoteAckId        uint32       // 对方的ack id

	readLock    sync.RWMutex // 读缓存锁
	readable    chan int     // 可读通知
	readTimeout time.Time    // 应用层设置的io读超时
	readQueue   *readData    // 读缓存队列
	readSN      uint32       // 接收到的连续的数据包最大序号

	writeLock    sync.RWMutex // 写缓存锁
	writeTimeout time.Time    // 应用层设置的io写超时
	writeable    chan int     // 可写通知
	writeQueue   *writeData   // 写缓存队列
	writeSN      uint32       // 已确认的连续的最大的数据包序号

	pingId         uint32        // ping消息的id，递增
	rto            time.Duration // 超时重发
	rtt, rttVar    time.Duration // 实时/平均RTT，用于计算rto
	minRTO, maxRTO time.Duration // 最小rto，不要发送"太快"，最大rto，不要出现"假死"
}

// 返回net.OpError
func (this *Conn) netOpError(op string, err error) error {
	return &net.OpError{
		Op:     op,
		Net:    "udp",
		Source: this.remoteInternetAddr,
		Addr:   this.localListenAddr,
		Err:    err,
	}
}

// 设置读缓存大小（字节）
func (this *Conn) SetReadBuffer(n int) error {
	this.readLock.Lock()
	this.localQueueMaxLen.r = this.calcMaxLen(n, this.localMss)
	this.readLock.Unlock()
	return nil
}

// 设置写缓存大小（字节）
func (this *Conn) SetWriteBuffer(n int) error {
	this.writeLock.Lock()
	this.localQueueMaxLen.w = this.calcMaxLen(n, this.localMss)
	this.writeLock.Unlock()
	return nil
}

func (this *Conn) calcMaxLen(bytes int, mss uint16) uint32 {
	mss = uint16(minInt(maxInt(int(mss), minMSS), maxMSS))
	// 计算数据块队列的长度
	if uint32(bytes) < uint32(mss) {
		return 1
	}
	// 长度=字节/mss
	max := uint32(bytes) / uint32(mss)
	// 不能整除，+1
	if uint32(bytes)%uint32(mss) != 0 {
		max++
	}
	return max
}

// net.Conn接口
func (this *Conn) Read(b []byte) (int, error) {
	this.readLock.RLock()
	if this.readTimeout.IsZero() {
		this.readLock.RUnlock()
		for {
			select {
			case <-this.readable:
				n := this.read(b)
				if n > 0 {
					return n, nil
				}
			case <-this.closeSignal:
				return 0, this.netOpError("read", closeErr("conn"))
			}
		}
	} else {
		duration := this.readTimeout.Sub(time.Now())
		this.readLock.RUnlock()
		if duration < 0 {
			return 0, this.netOpError("read", opErr("timeout"))
		}
		for {
			select {
			case <-this.readable:
				n := this.read(b)
				if n > 0 {
					return n, nil
				}
			case <-this.closeSignal:
				return 0, this.netOpError("read", closeErr("conn"))
			case <-time.After(duration):
				return 0, this.netOpError("read", opErr("timeout"))
			}
		}
	}
}

// net.Conn接口
func (this *Conn) Write(b []byte) (int, error) {
	this.writeLock.RLock()
	if this.writeTimeout.IsZero() {
		this.writeLock.RUnlock()
		for {
			select {
			case <-this.writeable:
				n := this.write(b)
				if n > 0 {
					return n, nil
				}
			case <-this.closeSignal:
				return 0, this.netOpError("write", closeErr("conn"))
			}
		}
	} else {
		duration := this.writeTimeout.Sub(time.Now())
		this.writeLock.RUnlock()
		if duration < 0 {
			return 0, this.netOpError("write", opErr("timeout"))
		}
		for {
			select {
			case <-this.writeable:
				n := this.write(b)
				if n > 0 {
					return n, nil
				}
			case <-this.closeSignal:
				return 0, this.netOpError("write", closeErr("conn"))
			case <-time.After(duration):
				return 0, this.netOpError("write", opErr("timeout"))
			}
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
	close(this.connectSignal)
	close(this.readable)
	close(this.writeable)
	// 释放回收资源
	p1 := this.readQueue
	if p1 != nil {
		n := p1.next
		readDataPool.Put(p1)
		p1 = n
	}
	p2 := this.writeQueue
	if p2 != nil {
		n := p2.next
		writeDataPool.Put(p2)
		p2 = n
	}
	return nil
}

// net.Conn接口
func (this *Conn) LocalAddr() net.Addr {
	return this.localListenAddr
}

// net.Conn接口
func (this *Conn) RemoteAddr() net.Addr {
	return this.remoteInternetAddr
}

// net.Conn接口
func (this *Conn) SetDeadline(t time.Time) error {
	this.SetReadDeadline(t)
	this.SetWriteDeadline(t)
	return nil
}

// net.Conn接口
func (this *Conn) SetReadDeadline(t time.Time) error {
	this.readLock.Lock()
	this.readTimeout = t
	this.readLock.Unlock()
	return nil
}

// net.Conn接口
func (this *Conn) SetWriteDeadline(t time.Time) error {
	this.writeLock.Lock()
	this.writeTimeout = t
	this.writeLock.Unlock()
	return nil
}

// 本地的公网地址
func (this *Conn) LocalInternetAddr() net.Addr {
	return this.localInternetAddr
}

// 对方的监听地址
func (this *Conn) RemoteListenAddr() net.Addr {
	return this.remoteInternetAddr
}

// 本地地址是否nat(Network Address Translation)
func (this *Conn) IsNat() bool {
	return bytes.Equal(this.localInternetAddr.IP.To16(), this.localListenAddr.IP.To16()) &&
		this.localInternetAddr.Port == this.localListenAddr.Port
}

// 对方地址是否在nat(Network Address Translation)
func (this *Conn) IsRemoteNat() bool {
	return bytes.Equal(this.remoteInternetAddr.IP.To16(), this.remoteListenAddr.IP.To16()) &&
		this.remoteInternetAddr.Port == this.remoteListenAddr.Port
}

// 写数据
func (this *Conn) write(buf []byte) (n int) {
	this.writeLock.Lock()
	// 还能添加多少数据块
	m := this.localQueueMaxLen.w - this.localQueueLen.w
	if m < 1 {
		this.writeLock.Unlock()
		return
	}
	p := this.writeQueue.next
	for m > 0 && len(buf) > 0 {
		d := writeDataPool.Get().(*writeData)
		// 基本信息
		d.sn = this.writeSN
		d.first = time.Time{}
		d.last = time.Time{}
		// 直接编码data消息
		d.data.buf[msgType] = msgData[this.clientOrServer]
		binary.BigEndian.PutUint32(p.data.buf[msgDataCToken:], this.cToken)
		binary.BigEndian.PutUint32(p.data.buf[msgDataSToken:], this.sToken)
		binary.BigEndian.PutUint32(p.data.buf[msgDataSN:], p.sn)
		d.data.len = copy(p.data.buf[msgDataPayload:this.localMss], buf)
		// 递增
		this.writeSN++
		this.localQueueLen.w++
		// 添加到尾部
		p.next = d
		p = p.next
		// 剩余
		m--
		buf = buf[d.data.len:]
		// 字节
		n += d.data.len
	}
	this.writeLock.Unlock()
	return
}

// 读数据
func (this *Conn) read(buf []byte) (n int) {
	this.readLock.Lock()
	p := this.readQueue.next
	i := 0
	// 小于nextSN的都是可读的
	for p != nil && p.sn <= this.readSN {
		// 拷贝i个字节
		i = copy(buf[n:], p.buf[p.idx:])
		n += i
		p.idx += i
		// 数据块读完了，移除，回收
		if len(p.buf) == p.idx {
			n := p.next
			this.localQueueLen.r--
			readDataPool.Put(p)
			p = n
		}
		// 缓存读满了
		if n == len(buf) {
			break
		}
	}
	this.readQueue.next = p
	this.readLock.Unlock()
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

// 检查发送队列
func (this *Conn) checkWriteQueue(msg *udpData) {
	p := this.writeQueue.next
	if p == nil {
		return
	}
	sn := binary.BigEndian.Uint32(msg.buf[msgAckSN:])
	max_sn := binary.BigEndian.Uint32(msg.buf[msgAckMaxSN:])
	if max_sn >= sn {
		// 从发送队列移除小于max_sn的数据块
		now := time.Now()
		for p != nil {
			if p.sn > sn {
				break
			}
			if p.sn == sn {
				// 计算RTO
				this.calcRTO(now.Sub(p.first))
			}
			// 移除
			n := p.next
			writeDataPool.Put(p)
			p = n
			this.localQueueLen.w--
		}
		this.writeQueue.next = p
		return
	}
	// 从发送队列移除指定sn的数据块
	prev := p
	p = prev.next
	for p != nil {
		if p.sn > sn {
			break
		}
		if p.sn == sn {
			// 计算RTO
			this.calcRTO(time.Now().Sub(p.first))
			// 移除
			prev.next = p.next
			writeDataPool.Put(p)
			return
		}
		prev = p
		p = prev.next
	}
}

// 解码msgData，并编码msgAck到msg中
func (this *Conn) readMsgData(msg *udpData) {
	this.readLock.Lock()
	// 数据包序号
	sn := binary.BigEndian.Uint32(msg.buf[msgDataSN:])
	// 初始化ack消息
	this.writeMsgAck(sn, msg)
	this.readLock.Unlock()
}

// 编码msgDial
func (this *Conn) writeMsgDial(msg *udpData) {
	msg.buf[msgType] = msgDial
	binary.BigEndian.PutUint32(msg.buf[msgDialVersion:], msgVersion)
	copy(msg.buf[msgDialLocalIP:], this.localListenAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.buf[msgDialLocalPort:], uint16(this.localListenAddr.Port))
	copy(msg.buf[msgDialRemoteIP:], this.remoteInternetAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.buf[msgDialRemotePort:], uint16(this.remoteInternetAddr.Port))
	binary.BigEndian.PutUint16(msg.buf[msgDialMSS:], this.localMss)
	binary.BigEndian.PutUint32(msg.buf[msgDialReadQueue:], this.localQueueMaxLen.r)
	binary.BigEndian.PutUint32(msg.buf[msgDialWriteQueue:], this.localQueueMaxLen.w)
}

// 解码msgDial
func (this *Conn) readMsgDial(msg *udpData) {
	this.remoteListenAddr = new(net.UDPAddr)
	this.remoteListenAddr.IP = append(this.remoteListenAddr.IP, msg.buf[msgDialLocalIP:msgDialLocalPort]...)
	this.remoteListenAddr.Port = int(binary.BigEndian.Uint16(msg.buf[msgDialLocalPort:]))
	this.localInternetAddr = new(net.UDPAddr)
	this.localInternetAddr.IP = append(this.localInternetAddr.IP, msg.buf[msgDialRemoteIP:msgDialRemotePort]...)
	this.localInternetAddr.Port = int(binary.BigEndian.Uint16(msg.buf[msgDialRemotePort:]))
	this.remoteMss = binary.BigEndian.Uint16(msg.buf[msgDialMSS:])
	this.remoteQueueMaxLen.r = binary.BigEndian.Uint32(msg.buf[msgDialReadQueue:])
	this.remoteQueueMaxLen.w = binary.BigEndian.Uint32(msg.buf[msgDialWriteQueue:])
}

// 编码msgAccept
func (this *Conn) writeMsgAccept(msg *udpData) {
	msg.buf[msgType] = msgAccept
	binary.BigEndian.PutUint32(msg.buf[msgAcceptCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.buf[msgAcceptSToken:], this.sToken)
	copy(msg.buf[msgAcceptLocalIP:], this.localListenAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.buf[msgAcceptLocalPort:], uint16(this.localListenAddr.Port))
	copy(msg.buf[msgAcceptRemoteIP:], this.remoteInternetAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.buf[msgAcceptRemotePort:], uint16(this.remoteInternetAddr.Port))
	binary.BigEndian.PutUint16(msg.buf[msgAcceptMSS:], this.localMss)
	binary.BigEndian.PutUint32(msg.buf[msgAcceptReadQueue:], this.localQueueMaxLen.r)
	binary.BigEndian.PutUint32(msg.buf[msgAcceptWriteQueue:], this.localQueueMaxLen.w)
}

// 解码msgAccept
func (this *Conn) readMsgAccept(msg *udpData) {
	this.remoteListenAddr = new(net.UDPAddr)
	this.remoteListenAddr.IP = append(this.remoteListenAddr.IP, msg.buf[msgAcceptLocalIP:msgAcceptLocalPort]...)
	this.remoteListenAddr.Port = int(binary.BigEndian.Uint16(msg.buf[msgAcceptLocalPort:]))
	this.localInternetAddr = new(net.UDPAddr)
	this.localInternetAddr.IP = append(this.localInternetAddr.IP, msg.buf[msgAcceptRemoteIP:msgAcceptRemotePort]...)
	this.localInternetAddr.Port = int(binary.BigEndian.Uint16(msg.buf[msgAcceptRemotePort:]))
	this.remoteMss = binary.BigEndian.Uint16(msg.buf[msgAcceptMSS:])
	this.remoteQueueMaxLen.r = binary.BigEndian.Uint32(msg.buf[msgAcceptReadQueue:])
	this.remoteQueueMaxLen.w = binary.BigEndian.Uint32(msg.buf[msgAcceptWriteQueue:])
}

// 编码msgConnect
func (this *Conn) writeMsgConnect(msg *udpData) {
	msg.buf[msgType] = msgConnect
	binary.BigEndian.PutUint32(msg.buf[msgConnectCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.buf[msgConnectSToken:], this.sToken)
}

// 编码msgAck
func (this *Conn) writeMsgAck(sn uint32, msg *udpData) {
	msg.buf[msgType] = msgAck[this.clientOrServer]
	binary.BigEndian.PutUint32(msg.buf[msgAckCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.buf[msgAckSToken:], this.sToken)
	binary.BigEndian.PutUint32(msg.buf[msgAckSN:], sn)
	binary.BigEndian.PutUint32(msg.buf[msgAckMaxSN:], this.readSN)
	binary.BigEndian.PutUint32(msg.buf[msgAckRemains:], this.localQueueMaxLen.r-this.localQueueLen.r)
	binary.BigEndian.PutUint32(msg.buf[msgAckId:], this.localAckId)
}

// 解码msgAck
func (this *Conn) readMsgAck(msg *udpData) {
	this.writeLock.Lock()
	ack_id := binary.BigEndian.Uint32(msg.buf[msgAckId:])
	this.checkWriteQueue(msg)
	if ack_id > this.remoteAckId {
		this.remoteAckId = ack_id
		this.remoteQueueRemains = binary.BigEndian.Uint32(msg.buf[msgAckRemains:])
	}
	this.writeLock.Unlock()
}

// 编码msgPing
func (this *Conn) writeMsgPing(msg *udpData) {
	msg.buf[msgType] = msgPing
	binary.BigEndian.PutUint32(msg.buf[msgPingCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.buf[msgPingSToken:], this.sToken)
	binary.BigEndian.PutUint32(msg.buf[msgPingId:], this.pingId)
}

// 编码msgPong
func (this *Conn) writeMsgPong(msg *udpData, pingId uint32) {
	msg.buf[msgType] = msgPong
	binary.BigEndian.PutUint32(msg.buf[msgPongCToken:], this.cToken)
	binary.BigEndian.PutUint32(msg.buf[msgPongSToken:], this.sToken)
	binary.BigEndian.PutUint32(msg.buf[msgPongPingId:], pingId)
	this.readLock.RLock()
	binary.BigEndian.PutUint32(msg.buf[msgPongSN:], this.readSN)
	binary.BigEndian.PutUint32(msg.buf[msgPongRemains:], this.localQueueMaxLen.r-this.localQueueLen.r)
	this.readLock.RUnlock()
}

// 解码msgPong
func (this *Conn) readMsgPong(msg *udpData) {
	id := binary.BigEndian.Uint32(msg.buf[msgPongPingId:])
	this.lock.Lock()
	if this.pingId == id {
		this.pingId++
		this.lock.Unlock()
		this.writeLock.Lock()
		this.checkWriteQueue(msg)
		this.remoteQueueRemains = binary.BigEndian.Uint32(msg.buf[msgPongRemains:])
		this.writeLock.Unlock()
		return
	}
	this.lock.Unlock()
}
