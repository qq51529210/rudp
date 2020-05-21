package rudp

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"
)

// Conn读队列数据包
type readData struct {
	sn   uint32    // 序号
	buf  []byte    // 数据
	idx  int       // 数据下标
	next *readData // 下一个
}

// Conn写队列数据包
type writeData struct {
	sn    uint32       // 序号
	buf   [maxMSS]byte // 数据
	len   uint16
	next  *writeData // 下一个
	first time.Time  // 第一次被发送的时间，用于计算rtt
	last  time.Time  // 上一次被发送的时间，超时重发判断
}

var (
	readDataPool, writeDataPool sync.Pool
	emptyData                   []byte
)

func init() {
	readDataPool.New = func() interface{} {
		return new(readData)
	}
	writeDataPool.New = func() interface{} {
		return new(writeData)
	}
}

// Conn的状态
const (
	connStateClose   = iota // 连接超时/被应用层关闭
	connStateDial           // 客户端发起连接
	connStateAccept         // 服务端收到连接请求，等待客户端确认
	connStateConnect        // cs，client和server双向确认连接
	connStateClosing        // 正在关闭，但是读（被关闭）缓存还有数据没有传输完成
)

type Conn struct {
	cs               uint8         // clientConn/serverConn
	state            uint8         // 状态
	lock             sync.RWMutex  // 同步锁
	connected        chan int      // 建立连接的信号
	closed           chan struct{} // 关闭的信号
	timer            *time.Timer   // 计时器
	lLAddr           *net.UDPAddr  // 本地监听地址
	lIAddr           *net.UDPAddr  // 本地互联网地址
	rLAddr           *net.UDPAddr  // 对方监听地址
	rIAddr           *net.UDPAddr  // 对方互联网地址
	ioBytes          uint64RW      // io读写总字节
	readTime         time.Time     // 上一次读取有效数据的时间
	readLock         sync.RWMutex  // 读缓存锁
	readable         chan int      // 可读通知
	readTimeout      time.Time     // 应用层设置的io读超时
	readQueue        *readData     // 读队列
	readQueueLen     uint32        // 读队列长度
	readQueueMaxLen  uint32        // 读队列最大长度
	readNextSN       uint32        // 读队列接收到的连续的数据包最大序号
	readMinSN        uint32        // 读队列最小序号，接收窗口控制
	readMaxSN        uint32        // 读队列最大序号，接收窗口控制
	writeLock        sync.RWMutex  // 写缓存锁
	writeTimeout     time.Time     // 应用层设置的io写超时
	writeable        chan int      // 可写通知
	writeQueueHead   *writeData    // 写队列第一个数据包
	writeQueueTail   *writeData    // 写队列最后一个数据包
	writeQueueLen    uint32        // 写队列长度
	writeQueueMaxLen uint32        // 写队列最大长度
	writeSN          uint32        // 写队列的最大sn
	writeMSS         uint16        // 写队列的每个数据包的大小
	writeMax         uint32        // 对方读缓存队列剩余长度，发送窗口控制
	rto              time.Duration // 超时重发
	rtt              time.Duration // 实时RTT，用于计算rto
	rttVar           time.Duration // 平均RTT，用于计算rto
	minRTO           time.Duration // 最小rto，不要出现"过快"
	maxRTO           time.Duration // 最大rto，不要出现"假死"
	cToken           uint32        // 客户端token
	sToken           uint32        // 服务端token
	pingId           uint32        // ping消息的id，递增
}

// 返回net.OpError
func (this *Conn) netOpError(op string, err error) error {
	return &net.OpError{
		Op:     op,
		Net:    "udp",
		Source: this.rIAddr,
		Addr:   this.lLAddr,
		Err:    err,
	}
}

// 释放资源
func (this *Conn) release() {
	// 回收缓存
	this.rBuf.Release()
	this.wBuf.Release()
}

// net.Conn接口
func (this *Conn) Read(b []byte) (int, error) {
	// 是否关闭
	if this.isClose() {
		return 0, this.netOpError("read", closeErr("conn"))
	}
	// 没有设置超时
	this.rBuf.RLock()
	if this.rBuf.timeout.IsZero() {
		this.rBuf.RUnlock()
		for {
			select {
			case <-this.rBuf.enable:
				n := this.read(b)
				if n > 0 {
					return n, nil
				}
				if n < 0 {
					return 0, io.EOF
				}
			case <-this.closed:
				return 0, this.netOpError("read", closeErr("conn"))
			}
		}
	}
	// 设置了超时
	duration := this.rBuf.timeout.Sub(time.Now())
	this.rBuf.RUnlock()
	if duration < 0 {
		return 0, this.netOpError("read", opErr("timeout"))
	}
	for {
		select {
		case <-this.rBuf.enable:
			n := this.read(b)
			if n > 0 {
				return n, nil
			}
			if n < 0 {
				return 0, io.EOF
			}
		case <-this.closed:
			return 0, this.netOpError("read", closeErr("conn"))
		case <-time.After(duration):
			return 0, this.netOpError("read", opErr("timeout"))
		}
	}
}

// net.Conn接口
func (this *Conn) Write(b []byte) (int, error) {
	// 是否关闭
	if this.isClose() {
		return 0, this.netOpError("write", closeErr("conn"))
	}
	// 没有设置超时
	m := 0
	this.wBuf.RLock()
	if this.wBuf.timeout.IsZero() {
		this.wBuf.RUnlock()
		for {
			select {
			case <-this.wBuf.enable:
				n := this.write(b[m:])
				m += n
				if m == len(b) {
					return n, nil
				}
			case <-this.closed:
				return 0, this.netOpError("write", closeErr("conn"))
			}
		}
	}
	// 设置了超时
	duration := this.wBuf.timeout.Sub(time.Now())
	this.wBuf.RUnlock()
	if duration < 0 {
		return 0, this.netOpError("write", opErr("timeout"))
	}
	for {
		select {
		case <-this.wBuf.enable:
			n := this.write(b[m:])
			m += n
			if m == len(b) {
				return n, nil
			}
		case <-this.closed:
			return 0, this.netOpError("write", closeErr("conn"))
		case <-time.After(duration):
			return 0, this.netOpError("write", opErr("timeout"))
		}
	}
}

// 写入数据到发送队列，返回0表示队列满了无法写入
func (this *Conn) write(b []byte) int {
	return 0
}

// net.Conn接口
func (this *Conn) Close() error {
	// 修改状态
	this.lock.Lock()
	if this.state == connStateClose {
		this.lock.Unlock()
		return this.netOpError("close", closeErr("conn"))
	}
	this.state = connStateClose
	this.lock.Unlock()
	// 发送一个空数据
	this.write(emptyData)
	return nil
}

// net.Conn接口
func (this *Conn) LocalAddr() net.Addr {
	return this.lLAddr
}

// net.Conn接口
func (this *Conn) RemoteAddr() net.Addr {
	return this.rIAddr
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

// 设置读缓存大小（字节）
func (this *Conn) SetReadBuffer(n int) error {
	this.rBuf.Lock()
	this.rBuf.maxLen = calcMaxLen(uint32(n), this.rBuf.mss)
	this.rBuf.Unlock()
	return nil
}

// 设置写缓存大小（字节）
func (this *Conn) SetWriteBuffer(n int) error {
	this.wBuf.Lock()
	this.wBuf.maxLen = calcMaxLen(uint32(n), this.wBuf.mss)
	this.wBuf.Unlock()
	return nil
}

// 本地的公网地址
func (this *Conn) LocalInternetAddr() net.Addr {
	return this.lIAddr
}

// 对方的监听地址
func (this *Conn) RemoteListenAddr() net.Addr {
	return this.rLAddr
}

// 本地地址是否nat(Network Address Translation)
func (this *Conn) IsNat() bool {
	return bytes.Equal(this.lIAddr.IP.To16(), this.lLAddr.IP.To16()) &&
		this.lIAddr.Port == this.lLAddr.Port
}

// 对方地址是否在nat(Network Address Translation)
func (this *Conn) IsRemoteNat() bool {
	return bytes.Equal(this.rIAddr.IP.To16(), this.rLAddr.IP.To16()) &&
		this.rIAddr.Port == this.rLAddr.Port
}

func (this *Conn) isClose() bool {
	this.lock.RLock()
	if this.state == connStateClose {
		this.lock.RUnlock()
		return true
	}
	this.lock.RUnlock()
	return false
}

func (this *Conn) newReadData(sn uint32, buf []byte, next *readData) *readData {
	d := readDataPool.Get().(*readData)
	d.sn = sn
	d.idx = 0
	d.buf = d.buf[:0]
	d.buf = append(d.buf, buf...)
	d.next = next
	this.len++
	return d
}

func (this *Conn) newWriteData(msg uint8) {
	d := writeDataPool.Get().(*writeData)
	d.sn = this.sn
	d.next = nil
	d.first = time.Time{}
	d.buf[msgType] = msg
	binary.BigEndian.PutUint32(d.buf[msgCToken:], this.cToken)
	binary.BigEndian.PutUint32(d.buf[msgSToken:], this.sToken)
	binary.BigEndian.PutUint32(d.buf[msgSN:], d.sn)
	d.len = msgPayload
	this.tail.next = d
	this.tail = d
	this.sn++
	this.len++
}

// 写入0数据，表示eof
func (this *Conn) writeEOF(msg uint8) {
	this.newWriteData(msg)
}

// 从队列中移除小于sn的数据包
func (this *Conn) removeWriteQueueBefore(sn uint32) {
	now := time.Now()
	cur := this.head.next
	for cur != nil {
		if cur.sn > sn {
			return
		}
		// rto
		this.calcRTO(now.Sub(cur.first))
		next := cur.next
		writeDataPool.Put(cur)
		cur = next
		this.len--
	}
	this.head.next = cur
	this.tail = this.head
}

// 从队列中移除指定sn的数据包
func (this *Conn) removeWriteQueue(sn uint32) {
	// 大于最后一个
	if this.tail != nil && this.tail.sn < sn {
		return
	}
	prev := this.head
	cur := prev.next
	for cur != nil {
		// 因为是递增有序队列，sn如果小于当前，就没必要继续
		if sn < cur.sn {
			return
		}
		if sn == cur.sn {
			// rto
			this.calcRTO(time.Now().Sub(cur.first))
			// 从队列移除
			prev.next = cur.next
			if cur == this.tail {
				this.tail = prev
			}
			writeDataPool.Put(cur)
			this.len--
			return
		}
		prev = cur
		cur = prev.next
	}

}

// 从队列中移除指定sn的数据包，测试用
func (this *Conn) writeToUDP(out func([]byte), now time.Time) {
	// 需要发送的数据包个数
	n := this.len
	if n < 1 {
		return
	}
	// 不能超过最大发送个数
	if n > this.maxWrite {
		n = this.maxWrite
	}
	// 开始
	prev := this.head
	cur := prev.next
	for cur != nil && n > 0 {
		// 第一次发送
		if cur.first.IsZero() {
			out(cur.buf[:cur.len])
			cur.first = now
			cur.last = now
			n--
		} else if now.Sub(cur.last) >= this.rto {
			out(cur.buf[:cur.len])
			cur.last = now
			n--
		}
		// 下一个数据包
		cur = cur.next
	}
}

// 读取连续的数据，返回0表示没有数据，返回-1表示io.EOF
func (this *Conn) read(b []byte) int {
	return 0
}

// 移除第一个数据包
func (this *Conn) removeReadQueueFront() {
	cur := this.head.next
	this.head.next = cur.next
	readDataPool.Put(cur)
	this.len--
}

// 添加数据包
func (this *Conn) readFromUDP(sn uint32, buf []byte) bool {
	// 是否在接收范围
	if sn < this.minSN || sn > this.maxSN {
		return false
	}
	prev := this.head
	cur := prev.next
	for cur != nil {
		// 重复
		if sn == cur.sn {
			return true
		}
		// 插入cur前
		if sn < cur.sn {
			break
		}
		// 检查下一个
		prev = cur
		cur = prev.next
	}
	prev.next = this.newReadData(sn, buf, cur)
	// 是否连续sn
	if sn == this.nextSN {
		this.nextSN++
		cur = prev.next.next
		for cur != nil {
			if cur.sn == this.nextSN {
				this.nextSN++
				cur = cur.next
				continue
			}
			break
		}
	}
	return true
}

// 根据实时的rtt来计算rto，使用的是tcp那套算法
func (this *Conn) calcRTO(rtt time.Duration) {
	this.rttVar = (3*this.rttVar + this.rtt - rtt) / 4
	this.rtt = (7*this.rtt + rtt) / 8
	this.rto = this.rtt + 4*this.rttVar
	if this.minRTO != 0 && this.rto < this.minRTO {
		this.rto = this.minRTO
	}
	if this.maxRTO != 0 && this.rto > this.maxRTO {
		this.rto = this.maxRTO
	}
}
//
//// 检查发送队列
//func (this *Conn) checkWriteQueue(buf []byte) {
//	//p := this.writeQueue.next
//	//if p == nil {
//	//	return
//	//}
//	//sn := binary.BigEndian.Uint32(buf[msgAckSN:])
//	//max_sn := binary.BigEndian.Uint32(buf[msgAckMaxSN:])
//	//if max_sn >= sn {
//	//	// 从发送队列移除小于max_sn的数据块
//	//	now := time.Now()
//	//	for p != nil {
//	//		if p.sn > sn {
//	//			break
//	//		}
//	//		if p.sn == sn {
//	//			// 计算RTO
//	//			this.calcRTO(now.Sub(p.first))
//	//		}
//	//		// 移除
//	//		n := p.next
//	//		writeDataPool.Put(p)
//	//		p = n
//	//		this.localQueueLen.w--
//	//	}
//	//	this.writeQueue.next = p
//	//	return
//	//}
//	//// 从发送队列移除指定sn的数据块
//	//prev := p
//	//p = prev.next
//	//for p != nil {
//	//	if p.sn > sn {
//	//		break
//	//	}
//	//	if p.sn == sn {
//	//		// 计算RTO
//	//		this.calcRTO(time.Now().Sub(p.first))
//	//		// 移除
//	//		prev.next = p.next
//	//		writeDataPool.Put(p)
//	//		return
//	//	}
//	//	prev = p
//	//	p = prev.next
//	//}
//}
//
//// 解码msgData，并编码msgAck到msg中
//func (this *Conn) readMsgData(buf []byte) {
//	//this.rBuf.Lock()
//	//// 数据包序号
//	//sn := binary.BigEndian.Uint32(buf[msgDataSN:])
//	//// 初始化ack消息
//	//this.writeMsgAck(sn, msg)
//	//this.rBuf.Unlock()
//}
//
//// 编码msgDial
//func (this *Conn) writeMsgDial(buf []byte) {
//	buf[msgType] = msgDial
//	binary.BigEndian.PutUint32(buf[msgDialVersion:], msgVersion)
//	copy(buf[msgDialLocalIP:], this.lLAddr.IP.To16())
//	binary.BigEndian.PutUint16(buf[msgDialLocalPort:], uint16(this.lLAddr.Port))
//	copy(buf[msgDialRemoteIP:], this.rIAddr.IP.To16())
//	binary.BigEndian.PutUint16(buf[msgDialRemotePort:], uint16(this.rIAddr.Port))
//	binary.BigEndian.PutUint16(buf[msgDialMSS:], this.wBuf.mss)
//	binary.BigEndian.PutUint32(buf[msgDialReadQueue:], this.rBuf.maxLen)
//	binary.BigEndian.PutUint32(buf[msgDialWriteQueue:], this.wBuf.maxLen)
//}
//
//// 解码msgDial
//func (this *Conn) readMsgDial(buf []byte) {
//	this.rLAddr = new(net.UDPAddr)
//	this.rLAddr.IP = append(this.rLAddr.IP, buf[msgDialLocalIP:msgDialLocalPort]...)
//	this.rLAddr.Port = int(binary.BigEndian.Uint16(buf[msgDialLocalPort:]))
//	this.lIAddr = new(net.UDPAddr)
//	this.lIAddr.IP = append(this.lIAddr.IP, buf[msgDialRemoteIP:msgDialRemotePort]...)
//	this.lIAddr.Port = int(binary.BigEndian.Uint16(buf[msgDialRemotePort:]))
//	this.rBuf.mss = binary.BigEndian.Uint16(buf[msgDialMSS:])
//	this.remoteQueue.r = binary.BigEndian.Uint32(buf[msgDialReadQueue:])
//	this.remoteQueue.w = binary.BigEndian.Uint32(buf[msgDialWriteQueue:])
//}
//
//// 编码msgAccept
//func (this *Conn) writeMsgAccept(buf []byte) {
//	buf[msgType] = msgAccept
//	binary.BigEndian.PutUint32(buf[msgAcceptCToken:], this.wBuf.cToken)
//	binary.BigEndian.PutUint32(buf[msgAcceptSToken:], this.wBuf.sToken)
//	copy(buf[msgAcceptLocalIP:], this.lLAddr.IP.To16())
//	binary.BigEndian.PutUint16(buf[msgAcceptLocalPort:], uint16(this.lLAddr.Port))
//	copy(buf[msgAcceptRemoteIP:], this.rIAddr.IP.To16())
//	binary.BigEndian.PutUint16(buf[msgAcceptRemotePort:], uint16(this.rIAddr.Port))
//	binary.BigEndian.PutUint16(buf[msgAcceptMSS:], this.rBuf.mss)
//	binary.BigEndian.PutUint32(buf[msgAcceptReadQueue:], this.rBuf.maxLen)
//	binary.BigEndian.PutUint32(buf[msgAcceptWriteQueue:], this.wBuf.maxLen)
//}
//
//// 解码msgAccept
//func (this *Conn) readMsgAccept(buf []byte) {
//	this.rLAddr = new(net.UDPAddr)
//	this.rLAddr.IP = append(this.rLAddr.IP, buf[msgAcceptLocalIP:msgAcceptLocalPort]...)
//	this.rLAddr.Port = int(binary.BigEndian.Uint16(buf[msgAcceptLocalPort:]))
//	this.lIAddr = new(net.UDPAddr)
//	this.lIAddr.IP = append(this.lIAddr.IP, buf[msgAcceptRemoteIP:msgAcceptRemotePort]...)
//	this.lIAddr.Port = int(binary.BigEndian.Uint16(buf[msgAcceptRemotePort:]))
//	this.rBuf.mss = binary.BigEndian.Uint16(buf[msgAcceptMSS:])
//	this.remoteQueue.r = binary.BigEndian.Uint32(buf[msgAcceptReadQueue:])
//	this.remoteQueue.w = binary.BigEndian.Uint32(buf[msgAcceptWriteQueue:])
//}
//
//// 编码msgConnect
//func (this *Conn) writeMsgConnect(buf []byte) {
//	buf[msgType] = msgConnect
//	//binary.BigEndian.PutUint32(buf[msgConnectCToken:], this.wBuf.cToken)
//	//binary.BigEndian.PutUint32(buf[msgConnectSToken:], this.wBuf.sToken)
//}
//
//// 编码msgAck
//func (this *Conn) writeMsgAck(sn uint32, buf []byte) {
//	buf[msgType] = msgAck[this.cs]
//	binary.BigEndian.PutUint32(buf[msgAckCToken:], this.wBuf.cToken)
//	binary.BigEndian.PutUint32(buf[msgAckSToken:], this.wBuf.sToken)
//	binary.BigEndian.PutUint32(buf[msgAckSN:], sn)
//	binary.BigEndian.PutUint32(buf[msgAckMaxSN:], this.rBuf.nextSN)
//	binary.BigEndian.PutUint32(buf[msgAckRemains:], this.rBuf.maxLen-this.rBuf.len)
//	binary.BigEndian.PutUint64(buf[msgAckTime:], uint64(time.Now().Unix()))
//}
//
//// 解码msgAck
//func (this *Conn) readMsgAck(buf []byte) {
//	//this.writeLock.Lock()
//	//ack_id := binary.BigEndian.Uint32(buf[msgAckId:])
//	//this.checkWriteQueue(msg)
//	//if ack_id > this.remoteAckId {
//	//	this.remoteAckId = ack_id
//	//	this.remoteQueueRemains = binary.BigEndian.Uint32(buf[msgAckRemains:])
//	//}
//	//this.writeLock.Unlock()
//}
//
//// 编码msgPing
//func (this *Conn) writeMsgPing(buf []byte) {
//	//buf[msgType] = msgPing
//	//binary.BigEndian.PutUint32(buf[msgPingCToken:], this.cToken)
//	//binary.BigEndian.PutUint32(buf[msgPingSToken:], this.sToken)
//	//binary.BigEndian.PutUint32(buf[msgPingId:], this.pingId)
//}
//
//// 编码msgPong
//func (this *Conn) writeMsgPong(buf []byte, pingId uint32) {
//	//buf[msgType] = msgPong
//	//binary.BigEndian.PutUint32(buf[msgPongCToken:], this.cToken)
//	//binary.BigEndian.PutUint32(buf[msgPongSToken:], this.sToken)
//	//binary.BigEndian.PutUint32(buf[msgPongPingId:], pingId)
//	//this.rBuf.RLock()
//	//binary.BigEndian.PutUint32(buf[msgPongSN:], this.readSN)
//	//binary.BigEndian.PutUint32(buf[msgPongRemains:], this.localQueueMaxLen.r-this.localQueueLen.r)
//	//this.rBuf.RUnlock()
//}
//
//// 解码msgPong
//func (this *Conn) readMsgPong(buf []byte) {
//	//id := binary.BigEndian.Uint32(buf[msgPongPingId:])
//	//this.lock.Lock()
//	//if this.pingId == id {
//	//	this.pingId++
//	//	this.lock.Unlock()
//	//	this.writeLock.Lock()
//	//	this.checkWriteQueue(msg)
//	//	this.remoteQueueRemains = binary.BigEndian.Uint32(buf[msgPongRemains:])
//	//	this.writeLock.Unlock()
//	//	return
//	//}
//	//this.lock.Unlock()
//}
