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
	sn    uint32        // 序号
	buf   [maxMSS]byte  // 数据
	len   uint16        // 数据大小
	next  *writeData    // 下一个
	first time.Time     // 第一次被发送的时间，用于计算rtt
	last  time.Time     // 上一次被发送的时间，超时重发判断
	rto   time.Duration // 数据包超时重发，每个数据包都不一样
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

func (c connCS) String() string {
	if c == connClient {
		return "client"
	}
	return "server"
}

const (
	connClient connCS = 0
	connServer connCS = 1
)

// Conn的状态
type connState uint8

const (
	connStateClose   connState = iota // 连接超时/被应用层关闭
	connStateDial                     // 客户端发起连接
	connStateAccept                   // 服务端收到连接请求，等待客户端确认
	connStateConnect                  // cs，client和server双向确认连接
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
	lAckId           uint32
	rAckId           uint32
}

// 返回net.OpError
func (c *Conn) netOpError(op string, err error) error {
	return &net.OpError{
		Op:     op,
		Net:    "udp",
		Source: c.rIAddr,
		Addr:   c.lLAddr,
		Err:    err,
	}
}

// net.Conn接口
func (c *Conn) Read(b []byte) (int, error) {
	// 是否关闭
	if c.isClose() {
		return 0, c.netOpError("read", closeErr("conn"))
	}
	// 没有设置超时
	c.readLock.RLock()
	if c.readTimeout.IsZero() {
		c.readLock.RUnlock()
		for {
			c.readLock.Lock()
			n := c.read(b)
			c.readLock.Unlock()
			if n == 0 {
				select {
				case <-c.readable:
					continue
				case <-c.closed:
					return 0, c.netOpError("read", closeErr("conn"))
				}
			}
			if n > 0 {
				return n, nil
			}
			if n < 0 {
				c.Close()
				return 0, io.EOF
			}
		}
	}
	// 设置了超时
	duration := c.readTimeout.Sub(time.Now())
	c.readLock.RUnlock()
	if duration < 0 {
		return 0, c.netOpError("read", opErr("timeout"))
	}
	for {
		c.readLock.Lock()
		n := c.read(b)
		c.readLock.Unlock()
		if n == 0 {
			select {
			case <-c.readable:
				continue
			case <-c.closed:
				return 0, c.netOpError("read", closeErr("conn"))
			case <-time.After(duration):
				return 0, c.netOpError("read", opErr("timeout"))
			}
		}
		if n > 0 {
			return n, nil
		}
		if n < 0 {
			return 0, io.EOF
		}
	}
}

// net.Conn接口
func (c *Conn) Write(b []byte) (int, error) {
	// 是否关闭
	if c.isClose() {
		return 0, c.netOpError("write", closeErr("conn"))
	}
	// 没有设置超时
	m := 0
	c.writeLock.RLock()
	if c.writeTimeout.IsZero() {
		c.writeLock.RUnlock()
		for {
			c.writeLock.Lock()
			n := c.write(b[m:])
			c.writeLock.Unlock()
			if n == 0 {
				select {
				case <-c.writeable:
					continue
				case <-c.closed:
					return 0, c.netOpError("write", closeErr("conn"))
				}
			}
			m += n
			if m == len(b) {
				return n, nil
			}
		}
	}
	// 设置了超时
	duration := c.writeTimeout.Sub(time.Now())
	c.writeLock.RUnlock()
	if duration < 0 {
		return 0, c.netOpError("write", opErr("timeout"))
	}
	for {
		c.writeLock.Lock()
		n := c.write(b[m:])
		c.writeLock.Unlock()
		if n == 0 {
			select {
			case <-c.writeable:
				continue
			case <-c.closed:
				return 0, c.netOpError("write", closeErr("conn"))
			case <-time.After(duration):
				return 0, c.netOpError("write", opErr("timeout"))
			}
		}
		m += n
		if m == len(b) {
			return n, nil
		}
	}
}

// net.Conn接口
func (c *Conn) Close() error {
	// 修改状态
	c.lock.Lock()
	if c.state == connStateClose {
		c.lock.Unlock()
		return c.netOpError("close", closeErr("conn"))
	}
	c.state = connStateClose
	c.lock.Unlock()
	// 发送一个空数据
	c.writeEOF()
	return nil
}

// net.Conn接口
func (c *Conn) LocalAddr() net.Addr {
	return c.lLAddr
}

// net.Conn接口
func (c *Conn) RemoteAddr() net.Addr {
	return c.rIAddr
}

// net.Conn接口
func (c *Conn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	c.SetWriteDeadline(t)
	return nil
}

// net.Conn接口
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.readLock.Lock()
	c.readTimeout = t
	c.readLock.Unlock()
	return nil
}

// net.Conn接口
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.writeLock.Lock()
	c.writeTimeout = t
	c.writeLock.Unlock()
	return nil
}

// 设置读缓存大小（字节），传输的速度由双方的rw最小缓存决定
func (c *Conn) SetReadBuffer(n int) error {
	c.readLock.Lock()
	c.readQueueMaxLen = calcMaxLen(uint32(n), c.readMSS)
	c.readLock.Unlock()
	return nil
}

// 设置写缓存大小（字节），传输的速度由双方的rw最小缓存决定
func (c *Conn) SetWriteBuffer(n int) error {
	c.writeLock.Lock()
	c.writeQueueMaxLen = calcMaxLen(uint32(n), c.writeMSS)
	c.writeLock.Unlock()
	return nil
}

// 本地的公网地址
func (c *Conn) LocalInternetAddr() net.Addr {
	return c.lIAddr
}

// 对方的监听地址
func (c *Conn) RemoteListenAddr() net.Addr {
	return c.rLAddr
}

// 本地地址是否nat(Network Address Translation)
func (c *Conn) IsNat() bool {
	return bytes.Equal(c.lIAddr.IP.To16(), c.lLAddr.IP.To16()) &&
		c.lIAddr.Port == c.lLAddr.Port
}

// 对方地址是否在nat(Network Address Translation)
func (c *Conn) IsRemoteNat() bool {
	return bytes.Equal(c.rIAddr.IP.To16(), c.rLAddr.IP.To16()) &&
		c.rIAddr.Port == c.rLAddr.Port
}

// 是否关闭
func (c *Conn) isClose() bool {
	c.lock.RLock()
	if c.state == connStateClose {
		c.lock.RUnlock()
		return true
	}
	c.lock.RUnlock()
	return false
}

// 返回一个初始化的readData
func (c *Conn) newReadData(sn uint32, buf []byte, next *readData) *readData {
	d := readDataPool.Get().(*readData)
	d.sn = sn
	d.idx = 0
	d.buf = d.buf[:0]
	d.buf = append(d.buf, buf...)
	d.next = next
	c.readQueueLen++
	return d
}

// 在发送队列末尾，添加一个初始化的writeData
func (c *Conn) newWriteData() {
	d := writeDataPool.Get().(*writeData)
	d.sn = c.writeSN
	d.next = nil
	d.first = time.Time{}
	d.buf[msgType] = msgData[c.cs]
	binary.BigEndian.PutUint32(d.buf[msgDataToken:], c.sToken)
	binary.BigEndian.PutUint32(d.buf[msgSN:], d.sn)
	d.len = msgPayload
	d.rto = c.rto
	c.writeQueueTail.next = d
	c.writeQueueTail = d
	c.writeSN++
	c.writeQueueLen++
}

// 读取连续的数据，返回0表示没有数据，返回-1表示io.EOF
func (c *Conn) read(buf []byte) int {
	// 没有数据
	if c.readQueueLen < 1 {
		return 0
	}
	n, m := 0, 0
	cur := c.readQueue.next
	for cur != nil && cur.sn < c.readNextSN {
		// 有数据包，但是没有数据，表示io.EOF
		if len(cur.buf) == 0 {
			// 前面的数据包都有数据，先返回前面的
			if n > 0 {
				return n
			}
			// 前面没有数据
			c.rmFrontReadData()
			return -1
		}
		// 拷贝数据到buf
		m = copy(buf[n:], cur.buf[cur.idx:])
		n += m
		cur.idx += m
		// 数据包数据拷贝完了，从队列中移除
		if cur.idx == len(cur.buf) {
			c.rmFrontReadData()
			c.readMinSN++
			c.readMaxSN++
		}
		// buf满了
		if n == len(buf) {
			return n
		}
		cur = c.readQueue.next
	}
	return n
}

// 写入数据到发送队列，返回0表示队列满了无法写入
func (c *Conn) write(buf []byte) int {
	// 还能添加多少个数据包
	m := c.writeQueueMaxLen - c.writeQueueLen
	if m <= 0 {
		return 0
	}
	n, i := 0, 0
	// 检查最后一个数据包是否"满数据"
	if c.writeQueueHead != c.writeQueueTail &&
		c.writeQueueTail.len < c.writeMSS &&
		c.writeQueueTail.first.IsZero() {
		i = copy(c.writeQueueTail.buf[c.writeQueueTail.len:c.writeMSS], buf)
		c.writeQueueTail.len += uint16(i)
		n += i
		buf = buf[i:]
	}
	// 新的数据包
	for m > 0 && len(buf) > 0 {
		c.newWriteData()
		i = copy(c.writeQueueTail.buf[msgPayload:c.writeMSS], buf)
		c.writeQueueTail.len += uint16(i)
		m--
		n += i
		buf = buf[i:]
	}
	return n
}

// 写入0数据，表示eof
func (c *Conn) writeEOF() {
	c.writeLock.Lock()
	c.newWriteData()
	c.writeLock.Unlock()
}

// 从队列中移除小于sn的数据包，返回true表示移除成功
func (c *Conn) removeWriteDataBefore(sn, remains uint32) bool {
	// 检查sn是否在发送窗口范围
	if c.writeQueueTail != c.writeQueueHead &&
		c.writeQueueTail.sn < sn {
		return false
	}
	// 遍历发送队列数据包
	cur := c.writeQueueHead.next
	if cur != nil && cur.sn > sn {
		return false
	}
	now := time.Now()
	for cur != nil {
		if cur.sn >= sn {
			break
		}
		// rto
		c.calcRTO(now.Sub(cur.first))
		next := cur.next
		writeDataPool.Put(cur)
		cur = next
		c.writeQueueLen--
	}
	c.writeQueueHead.next = cur
	if cur == nil {
		c.writeQueueTail = c.writeQueueHead
	}
	c.writeMax = remains
	return true
}

// 从队列中移除指定sn的数据包，返回true表示移除成功
func (c *Conn) removeWriteData(sn, remains uint32) bool {
	// 检查sn是否在发送窗口范围
	if c.writeQueueTail != c.writeQueueHead &&
		c.writeQueueTail.sn < sn {
		return false
	}
	// 遍历发送队列数据包
	prev := c.writeQueueHead
	cur := prev.next
	for cur != nil {
		// 因为是递增有序队列，sn如果小于当前，就没必要继续
		if sn < cur.sn {
			return false
		}
		if sn == cur.sn {
			// rto
			c.calcRTO(time.Now().Sub(cur.first))
			// 从队列移除
			prev.next = cur.next
			if cur == c.writeQueueTail {
				c.writeQueueTail = prev
			}
			writeDataPool.Put(cur)
			c.writeQueueLen--
			c.writeMax = remains
			return true
		}
		prev = cur
		cur = prev.next
	}
	return false
}

// 移除第一个数据包
func (c *Conn) rmFrontReadData() {
	cur := c.readQueue.next
	c.readQueue.next = cur.next
	readDataPool.Put(cur)
	c.readQueueLen--
}

// 检查发送队列，超时重发
func (c *Conn) writeToUDP(out func(data []byte), now time.Time) {
	// 需要发送的数据包个数
	n := c.writeQueueLen
	// 没有数据
	if n < 1 {
		return
	}
	// 不能超过最大发送个数
	if n > c.writeMax {
		n = c.writeMax
	}
	// 如果对方没有接收空间，发1个
	if n == 0 {
		n = 1
	}
	// 开始遍历发送队列
	prev := c.writeQueueHead
	cur := prev.next
	for cur != nil && n > 0 {
		// 第一次发送
		if cur.first.IsZero() {
			out(cur.buf[:cur.len])
			// 记录时间
			cur.first = now
			cur.last = now
			n--
		} else if now.Sub(cur.last) >= cur.rto {
			out(cur.buf[:cur.len])
			// 记录时间
			cur.last = now
			n--
			// rto加大
			cur.rto += c.rto / 2
			if c.maxRTO != 0 && cur.rto > c.maxRTO {
				cur.rto = c.maxRTO
			}
		}
		cur = cur.next
	}
}

// 添加udp数据包，返回true表示在接收范围内，需要返回ack
func (c *Conn) readFromUDP(sn uint32, buf []byte) bool {
	// 是否在接收范围
	if sn < c.readMinSN || sn >= c.readMaxSN {
		return false
	}
	// 遍历接收队列
	prev := c.readQueue
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
	prev.next = c.newReadData(sn, buf, cur)
	// 检查连续的sn
	cur = prev.next
	for cur != nil && cur.sn == c.readNextSN {
		c.readNextSN++
		cur = cur.next
	}
	// 更新有效数据的读时间
	c.readTime = time.Now()
	return true
}

// 根据实时的rtt来计算rto，使用的是tcp那套算法
func (c *Conn) calcRTO(rtt time.Duration) {
	c.rttVar = (3*c.rttVar + c.rtt - rtt) / 4
	c.rtt = (7*c.rtt + rtt) / 8
	c.rto = c.rtt + 4*c.rttVar
	if c.minRTO != 0 && c.rto < c.minRTO {
		c.rto = c.minRTO
	}
	if c.maxRTO != 0 && c.rto > c.maxRTO {
		c.rto = c.maxRTO
	}
}

// 格式化msgDial
func (c *Conn) writeMsgDial(buf []byte, timeout time.Duration) {
	buf[msgType] = msgDial
	binary.BigEndian.PutUint32(buf[msgDialToken:], c.cToken)
	binary.BigEndian.PutUint32(buf[msgDialVersion:], msgVersion)
	copy(buf[msgDialLocalIP:], c.lLAddr.IP.To16())
	binary.BigEndian.PutUint16(buf[msgDialLocalPort:], uint16(c.lLAddr.Port))
	copy(buf[msgDialRemoteIP:], c.rIAddr.IP.To16())
	binary.BigEndian.PutUint16(buf[msgDialRemotePort:], uint16(c.rIAddr.Port))
	binary.BigEndian.PutUint16(buf[msgDialMSS:], c.writeMSS)
	binary.BigEndian.PutUint64(buf[msgDialTimeout:], uint64(timeout))
}

// 读取msgDial字段
func (c *Conn) readMsgDial(buf []byte, size uint32) {
	c.rLAddr.IP = append(c.rLAddr.IP, buf[msgDialLocalIP:msgDialLocalPort]...)
	c.rLAddr.Port = int(binary.BigEndian.Uint16(buf[msgDialLocalPort:]))
	c.lIAddr.IP = append(c.lIAddr.IP, buf[msgDialRemoteIP:msgDialRemotePort]...)
	c.lIAddr.Port = int(binary.BigEndian.Uint16(buf[msgDialRemotePort:]))
	c.readMSS = binary.BigEndian.Uint16(buf[msgDialMSS:])
	c.readQueueMaxLen = calcMaxLen(size, c.readMSS)
	c.readMaxSN = c.readQueueMaxLen
}

// 格式化msgDial
func (c *Conn) writeMsgAccept(buf []byte) {
	buf[msgType] = msgAccept
	binary.BigEndian.PutUint32(buf[msgAcceptCToken:], c.cToken)
	binary.BigEndian.PutUint32(buf[msgAcceptSToken:], c.sToken)
	binary.BigEndian.PutUint32(buf[msgAcceptVersion:], msgVersion)
	copy(buf[msgAcceptLocalIP:], c.lLAddr.IP.To16())
	binary.BigEndian.PutUint16(buf[msgAcceptLocalPort:], uint16(c.lLAddr.Port))
	copy(buf[msgAcceptRemoteIP:], c.rIAddr.IP.To16())
	binary.BigEndian.PutUint16(buf[msgAcceptRemotePort:], uint16(c.rIAddr.Port))
	binary.BigEndian.PutUint16(buf[msgAcceptMSS:], c.writeMSS)
}

// 读取msgAccept字段
func (c *Conn) readMsgAccept(buf []byte, size uint32) {
	c.rLAddr.IP = append(c.rLAddr.IP, buf[msgAcceptLocalIP:msgAcceptLocalPort]...)
	c.rLAddr.Port = int(binary.BigEndian.Uint16(buf[msgAcceptLocalPort:]))
	c.lIAddr.IP = append(c.lIAddr.IP, buf[msgAcceptRemoteIP:msgAcceptRemotePort]...)
	c.lIAddr.Port = int(binary.BigEndian.Uint16(buf[msgAcceptRemotePort:]))
	c.readMSS = binary.BigEndian.Uint16(buf[msgAcceptMSS:])
	c.readQueueMaxLen = calcMaxLen(size, c.readMSS)
	c.readMaxSN = c.readQueueMaxLen
}

// 格式化msgConnect
func (c *Conn) writeMsgConnect(buf []byte) {
	buf[msgType] = msgConnect
	binary.BigEndian.PutUint32(buf[msgConnectToken:], c.sToken)
}

// 格式化msgAck
func (c *Conn) writeMsgAck(buf []byte, sn uint32) {
	buf[msgType] = msgAck[c.cs]
	binary.BigEndian.PutUint32(buf[msgAckToken:], c.sToken)
	binary.BigEndian.PutUint32(buf[msgAckSN:], sn)
	binary.BigEndian.PutUint32(buf[msgAckMaxSN:], c.readNextSN-1)
	binary.BigEndian.PutUint32(buf[msgAckRemains:], c.readQueueMaxLen-c.readQueueLen)
	binary.BigEndian.PutUint32(buf[msgAckId:], c.lAckId)
	c.lAckId++
}

// 读取msgPong字段
func (c *Conn) readMsgPong(buf []byte) bool {
	id := binary.BigEndian.Uint32(buf[msgPongPingId:])
	c.readLock.Lock()
	if c.pingId != id {
		c.readLock.Unlock()
		return false
	}
	c.pingId++
	c.readTime = time.Now()
	c.readLock.Unlock()

	c.writeLock.Lock()
	ok := c.removeWriteDataBefore(binary.BigEndian.Uint32(buf[msgPongMaxSN:]),
		binary.BigEndian.Uint32(buf[msgPongRemains:]))
	c.writeLock.Unlock()
	return ok
}

// 格式化msgPong
func (c *Conn) writeMsgPong(buf []byte) {
	pingId := binary.BigEndian.Uint32(buf[msgPingId:])
	buf[msgType] = msgPong
	binary.BigEndian.PutUint32(buf[msgPongToken:], c.sToken)
	binary.BigEndian.PutUint32(buf[msgPongPingId:], pingId)
	c.readLock.RLock()
	binary.BigEndian.PutUint32(buf[msgPongMaxSN:], c.readNextSN-1)
	binary.BigEndian.PutUint32(buf[msgPongRemains:], c.readQueueMaxLen-c.readQueueLen)
	c.readLock.RUnlock()
}
