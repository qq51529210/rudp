package rudp

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"
)

// Conn接收队列数据包
type readData struct {
	sn   uint32    // 序号
	buf  []byte    // 数据
	idx  int       // 数据下标
	next *readData // 下一个
}

// Conn发送队列数据包
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

// 是客户端还是服务端的Conn
type connCS uint8

const (
	clientConn connCS = 0
	serverConn connCS = 1
)

// Conn的状态
type connState uint8

const (
	connStateClose   connState = iota // 连接超时/被应用层关闭
	connStateDial                     // 客户端发起连接
	connStateAccept                   // 服务端收到连接请求，等待客户端确认
	connStateConnect                  // cs，client和server双向确认连接
	connStateClosing                  // 正在关闭，但是读（被关闭）缓存还有数据没有传输完成
)

type Conn struct {
	cs               connCS         // clientConn/serverConn
	state            connState      // 状态
	lock             sync.RWMutex   // 同步锁
	connected        chan connState // 建立连接的信号
	closed           chan struct{}  // 关闭的信号
	timer            *time.Timer    // 计时器
	lLAddr           *net.UDPAddr   // 本地监听地址
	lIAddr           *net.UDPAddr   // 本地互联网地址
	rLAddr           *net.UDPAddr   // 对方监听地址
	rIAddr           *net.UDPAddr   // 对方互联网地址
	ioBytes          uint64RW       // io读写总字节
	readTime         time.Time      // 上一次读取有效数据的时间
	readLock         sync.RWMutex   // 读缓存锁
	readable         chan int       // 可读通知
	readTimeout      time.Time      // 应用层设置的io读超时
	readQueue        *readData      // 接收队列，第一个数据包，仅仅作为指针
	readQueueLen     uint32         // 接收队列长度
	readQueueMaxLen  uint32         // 接收队列最大长度
	readNextSN       uint32         // 接收队列接收到的连续的数据包最大序号
	readMinSN        uint32         // 接收队列最小序号，接收窗口控制
	readMaxSN        uint32         // 接收队列最大序号，接收窗口控制
	readMSS          uint16         // 接收队列的每个数据包的大小（对方的发送队列数据包大小）
	writeLock        sync.RWMutex   // 写缓存锁
	writeTimeout     time.Time      // 应用层设置的io写超时
	writeable        chan int       // 可写通知
	writeQueueHead   *writeData     // 发送队列，第一个数据包，仅仅作为指针
	writeQueueTail   *writeData     // 发送队列，最后一个数据包
	writeQueueLen    uint32         // 发送队列长度
	writeQueueMaxLen uint32         // 发送队列最大长度
	writeSN          uint32         // 发送队列的最大sn
	writeMSS         uint16         // 发送队列的每个数据包的大小
	writeMax         uint32         // 对方读缓存队列剩余长度，发送窗口控制
	rto              time.Duration  // 超时重发
	rtt              time.Duration  // 实时RTT，用于计算rto
	rttVar           time.Duration  // 平均RTT，用于计算rto
	minRTO           time.Duration  // 最小rto，不要出现"过快"
	maxRTO           time.Duration  // 最大rto，不要出现"假死"
	cToken           uint32         // 客户端token
	sToken           uint32         // 服务端token
	pingId           uint32         // ping消息的id，递增
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

// net.Conn接口
func (this *Conn) Read(b []byte) (int, error) {
	// 是否关闭
	if this.isClose() {
		return 0, this.netOpError("read", closeErr("conn"))
	}
	// 没有设置超时
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
				if n < 0 {
					return 0, io.EOF
				}
			case <-this.closed:
				return 0, this.netOpError("read", closeErr("conn"))
			}
		}
	}
	// 设置了超时
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
	this.writeLock.RLock()
	if this.writeTimeout.IsZero() {
		this.writeLock.RUnlock()
		for {
			select {
			case <-this.writeable:
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
	duration := this.writeTimeout.Sub(time.Now())
	this.writeLock.RUnlock()
	if duration < 0 {
		return 0, this.netOpError("write", opErr("timeout"))
	}
	for {
		select {
		case <-this.writeable:
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

// 设置读缓存大小（字节）
func (this *Conn) SetReadBuffer(n int) error {
	this.readLock.Lock()
	this.readQueueMaxLen = calcMaxLen(uint32(n), this.readMSS)
	this.readLock.Unlock()
	return nil
}

// 设置写缓存大小（字节）
func (this *Conn) SetWriteBuffer(n int) error {
	this.writeLock.Lock()
	this.writeQueueMaxLen = calcMaxLen(uint32(n), this.writeMSS)
	this.writeLock.Unlock()
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

// 是否关闭
func (this *Conn) isClose() bool {
	this.lock.RLock()
	if this.state == connStateClose {
		this.lock.RUnlock()
		return true
	}
	this.lock.RUnlock()
	return false
}

// 返回一个初始化的readData
func (this *Conn) newReadData(sn uint32, buf []byte, next *readData) *readData {
	d := readDataPool.Get().(*readData)
	d.sn = sn
	d.idx = 0
	d.buf = d.buf[:0]
	d.buf = append(d.buf, buf...)
	d.next = next
	this.readQueueLen++
	return d
}

// 在发送队列末尾，添加一个初始化的writeData
func (this *Conn) newWriteData() {
	d := writeDataPool.Get().(*writeData)
	d.sn = this.writeSN
	d.next = nil
	d.first = time.Time{}
	d.buf[msgType] = msgData[this.cs]
	binary.BigEndian.PutUint32(d.buf[msgDataToken:], this.sToken)
	binary.BigEndian.PutUint32(d.buf[msgSN:], d.sn)
	d.len = msgPayload
	this.writeQueueTail.next = d
	this.writeQueueTail = d
	this.writeSN++
	this.writeQueueLen++
}

// 读取连续的数据，返回0表示没有数据，返回-1表示io.EOF
func (this *Conn) read(buf []byte) int {
	// 没有数据
	if this.readQueueLen < 1 {
		return 0
	}
	n, m := 0, 0
	cur := this.readQueue.next
	for cur != nil && cur.sn < this.readNextSN {
		// 有数据包，但是没有数据，表示io.EOF
		if len(cur.buf) == 0 {
			// 前面的数据包都有数据，先返回前面的
			if n > 0 {
				return n
			}
			// 前面没有数据
			this.rmFrontReadData()
			return -1
		}
		// 拷贝数据到buf
		m = copy(buf[n:], cur.buf[cur.idx:])
		n += m
		cur.idx += m
		// 数据包数据拷贝完了，从队列中移除
		if cur.idx == len(cur.buf) {
			this.rmFrontReadData()
			this.readMinSN++
			this.readMaxSN++
		}
		// buf满了
		if n == len(buf) {
			return n
		}
		cur = this.readQueue.next
	}
	return n
}

// 写入数据到发送队列，返回0表示队列满了无法写入
func (this *Conn) write(buf []byte) int {
	// 还能添加多少个数据包
	m := this.writeQueueMaxLen - this.writeQueueLen
	if m <= 0 {
		return 0
	}
	n, i := 0, 0
	// 检查最后一个数据包是否"满数据"
	if this.writeQueueHead != this.writeQueueTail &&
		this.writeQueueTail.len < this.writeMSS &&
		this.writeQueueTail.first.IsZero() {
		i = copy(this.writeQueueTail.buf[this.writeQueueTail.len:this.writeMSS], buf)
		this.writeQueueTail.len += uint16(i)
		n += i
		buf = buf[i:]
	}
	// 新的数据包
	for m > 0 && len(buf) > 0 {
		this.newWriteData()
		i = copy(this.writeQueueTail.buf[msgPayload:this.writeMSS], buf)
		this.writeQueueTail.len += uint16(i)
		m--
		n += i
		buf = buf[i:]
	}
	return n
}

// 写入0数据，表示eof
func (this *Conn) writeEOF() {
	this.newWriteData()
}

// 从队列中移除小于sn的数据包，返回true表示移除成功
func (this *Conn) rmWriteDataBefore(sn uint32) {
	// 检查sn是否在发送窗口范围
	if this.writeQueueTail != this.writeQueueHead &&
		this.writeQueueTail.sn < sn {
		return
	}
	now := time.Now()
	// 遍历发送队列数据包
	cur := this.writeQueueHead.next
	for cur != nil {
		if cur.sn > sn {
			return
		}
		// rto
		this.calcRTO(now.Sub(cur.first))
		next := cur.next
		writeDataPool.Put(cur)
		cur = next
		this.writeQueueLen--
	}
	this.writeQueueHead.next = cur
	this.writeQueueTail = this.writeQueueHead
	// 可写通知
	select {
	case this.writeable <- 1:
	default:
	}
}

// 从队列中移除指定sn的数据包，返回true表示移除成功
func (this *Conn) rmWriteData(sn uint32) {
	// 检查sn是否在发送窗口范围
	if this.writeQueueTail != this.writeQueueHead &&
		this.writeQueueTail.sn < sn {
		return
	}
	// 遍历发送队列数据包
	prev := this.writeQueueHead
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
			if cur == this.writeQueueTail {
				this.writeQueueTail = prev
			}
			writeDataPool.Put(cur)
			this.writeQueueLen--
			// 可写通知
			select {
			case this.writeable <- 1:
			default:
			}
			return
		}
		prev = cur
		cur = prev.next
	}
}

// 移除第一个数据包
func (this *Conn) rmFrontReadData() {
	cur := this.readQueue.next
	this.readQueue.next = cur.next
	readDataPool.Put(cur)
	this.readQueueLen--
}

// 检查发送队列，超时重发
func (this *Conn) writeToUDP(out func(data []byte), now time.Time) {
	// 需要发送的数据包个数
	n := this.writeQueueLen
	// 没有数据
	if n < 1 {
		return
	}
	// 不能超过最大发送个数
	if n > this.writeMax {
		n = this.writeMax
	}
	// 如果对方没有接收空间，发1个
	if n == 0 {
		n = 1
	}
	// 开始遍历发送队列
	prev := this.writeQueueHead
	cur := prev.next
	for cur != nil && n > 0 {
		// 第一次发送
		if cur.first.IsZero() {
			out(cur.buf[:cur.len])
			// 记录时间
			cur.first = now
			cur.last = now
			n--
		} else if now.Sub(cur.last) >= this.rto {
			out(cur.buf[:cur.len])
			// 记录时间
			cur.last = now
			n--
		}
		cur = cur.next
	}
}

// 添加udp数据包，返回true表示在接收范围内，需要返回ack
func (this *Conn) readFromUDP(sn uint32, buf []byte) bool {
	// 是否在接收范围
	if sn < this.readMinSN || sn > this.readMaxSN {
		return false
	}
	// 遍历接收队列
	prev := this.readQueue
	cur := prev.next
	for cur != nil {
		// 重复数据
		if sn == cur.sn {
			return true
		}
		// 插入
		if sn < cur.sn {
			break
		}
		// 检查下一个
		prev = cur
		cur = prev.next
	}
	prev.next = this.newReadData(sn, buf, cur)
	// 检查连续的sn
	if sn == this.readNextSN {
		this.readNextSN++
		cur = prev.next.next
		for cur != nil && cur.sn == this.readNextSN {
			this.readNextSN++
			cur = cur.next
		}
	}
	// 更新有效数据的读时间
	this.readTime = time.Now()
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

func (this *Conn) writeMsgDial(buf []byte, timeout time.Duration) {
	buf[msgType] = msgDial
	binary.BigEndian.PutUint32(buf[msgDialToken:], this.cToken)
	binary.BigEndian.PutUint32(buf[msgDialVersion:], msgVersion)
	copy(buf[msgDialLocalIP:], this.lLAddr.IP.To16())
	binary.BigEndian.PutUint16(buf[msgDialLocalPort:], uint16(this.lLAddr.Port))
	copy(buf[msgDialRemoteIP:], this.rIAddr.IP.To16())
	binary.BigEndian.PutUint16(buf[msgDialRemotePort:], uint16(this.rIAddr.Port))
	binary.BigEndian.PutUint16(buf[msgDialMSS:], this.writeMSS)
	binary.BigEndian.PutUint64(buf[msgDialTimeout:], uint64(timeout))
}

func (this *Conn) readMsgDial(buf []byte, size uint32) {
	this.rLAddr.IP = append(this.rLAddr.IP, buf[msgDialLocalIP:msgDialLocalPort]...)
	this.rLAddr.Port = int(binary.BigEndian.Uint16(buf[msgDialLocalPort:]))
	this.lIAddr.IP = append(this.lIAddr.IP, buf[msgDialRemoteIP:msgDialRemotePort]...)
	this.lIAddr.Port = int(binary.BigEndian.Uint16(buf[msgDialRemotePort:]))
	this.readMSS = binary.BigEndian.Uint16(buf[msgDialMSS:])
	this.readQueueMaxLen = calcMaxLen(size, this.readMSS)
}

func (this *Conn) writeMsgAccept(buf []byte) {
	buf[msgType] = msgAccept
	binary.BigEndian.PutUint32(buf[msgAcceptCToken:], this.cToken)
	binary.BigEndian.PutUint32(buf[msgAcceptSToken:], this.sToken)
	binary.BigEndian.PutUint32(buf[msgAcceptVersion:], msgVersion)
	copy(buf[msgAcceptLocalIP:], this.lLAddr.IP.To16())
	binary.BigEndian.PutUint16(buf[msgAcceptLocalPort:], uint16(this.lLAddr.Port))
	copy(buf[msgAcceptRemoteIP:], this.rIAddr.IP.To16())
	binary.BigEndian.PutUint16(buf[msgAcceptRemotePort:], uint16(this.rIAddr.Port))
	binary.BigEndian.PutUint16(buf[msgAcceptMSS:], this.writeMSS)
}

func (this *Conn) readMsgAccept(buf []byte, size uint32) {
	this.rLAddr.IP = append(this.rLAddr.IP, buf[msgAcceptLocalIP:msgAcceptLocalPort]...)
	this.rLAddr.Port = int(binary.BigEndian.Uint16(buf[msgAcceptLocalPort:]))
	this.lIAddr.IP = append(this.lIAddr.IP, buf[msgAcceptRemoteIP:msgAcceptRemotePort]...)
	this.lIAddr.Port = int(binary.BigEndian.Uint16(buf[msgAcceptRemotePort:]))
	this.readMSS = binary.BigEndian.Uint16(buf[msgAcceptMSS:])
	this.readQueueMaxLen = calcMaxLen(size, this.readMSS)
}

func (this *Conn) writeMsgConnect(buf []byte) {
	buf[msgType] = msgConnect
	binary.BigEndian.PutUint32(buf[msgConnectToken:], this.sToken)
}

func (this *Conn) writeMsgAck(buf []byte) {

}

func (this *Conn) readMsgPong(buf []byte) {
	id := binary.BigEndian.Uint32(buf[msgPongPingId:])
	this.readLock.Lock()
	if this.pingId != id {
		this.readLock.Unlock()
		return
	}
	this.pingId++
	this.readLock.Unlock()

	this.writeLock.Lock()
	this.rmWriteDataBefore(binary.BigEndian.Uint32(buf[msgPongMaxSN:]))
	this.writeMax = binary.BigEndian.Uint32(buf[msgPongRemains:])
	this.writeLock.Unlock()
}

func (this *Conn) writeMsgPong(buf []byte) {
	ping_id := binary.BigEndian.Uint32(buf[msgPingId:])
	buf[msgType] = msgPong
	binary.BigEndian.PutUint32(buf[msgPongToken:], this.sToken)
	binary.BigEndian.PutUint32(buf[msgPongPingId:], ping_id)
	this.readLock.RLock()
	binary.BigEndian.PutUint32(buf[msgPongMaxSN:], this.readNextSN)
	binary.BigEndian.PutUint32(buf[msgPongRemains:], this.readQueueMaxLen-this.readQueueLen)
	this.readLock.RUnlock()
}
