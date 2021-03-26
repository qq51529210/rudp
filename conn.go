package rudp

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"
)

const (
	dialState     = 1 << iota // 正在建立连接
	connectState              // 已经确认连接
	closedState               // 主动关闭连接
	shutdownState             // 被动关闭连接
	invalidState              // 无效的连接
)

const (
	connMaxReadQueue  = 0xffff
	connMaxWriteQueue = 0xffff
)

var (
	readDataPool    sync.Pool                 // readData pool
	writeDataPool   sync.Pool                 // writeData pool
	connWriteQueue  = uint16(128)             // 默认的conn的发送队列长度
	connReadQueue   = uint16(128)             // 默认的conn的接收队列长度
	connHandleQueue = uint16(256)             // 默认的conn的处理队列长度
	connMinRTO      = 100 * time.Millisecond  // 最小超时重传，毫秒
	connMaxRTO      = 1000 * time.Millisecond // 最大超时重传，毫秒
)

func init() {
	readDataPool.New = func() interface{} {
		return new(readData)
	}
	writeDataPool.New = func() interface{} {
		return new(writeData)
	}
}

// 设置默认的conn的segment处理队列长度
func SetConnHandleQueue(n uint16) {
	if n == 0 {
		connHandleQueue = 1024
	} else {
		connHandleQueue = n
	}
}

// net.timeout的接口
type timeoutError struct{}

func (e timeoutError) Error() string { return "timeout" }
func (e timeoutError) Timeout() bool { return true }

// readQueue的数据块
type readData struct {
	sn   uint32       // 序号，24位
	buf  [maxMSS]byte // 数据
	idx  uint16       // 有效数据的起始，因为read有可能一次读不完数据
	len  uint16       // 数据大小
	next *readData    // 下一个
}

// writeQueue的数据块
type writeData struct {
	sn    uint32        // 序号，24位
	buf   [maxMSS]byte  // 数据
	len   uint16        // 数据大小，因为write有可能写不完数据块
	next  *writeData    // 下一个
	first time.Time     // 第一次被发送的时间，用于计算rto
	last  time.Time     // 上一次被发送的时间，超时重发判断
	rto   time.Duration // 超时重发
}

type Conn struct {
	from               byte          // 0:client，1:server
	token              uint32        // 连接token
	timestamp          uint64        // 连接的时间戳
	state              byte          // 状态
	lock               sync.RWMutex  // 同步锁
	stateSignal        chan byte     // 状态改变的信号
	handleQueue        chan *segment // 待处理的数据缓存
	timer              *time.Timer   // 计时器
	localListenAddr    *net.UDPAddr  // local listen address
	remoteListenAddr   *net.UDPAddr  // remote listen address
	localInternetAddr  *net.UDPAddr  // local internet address
	remoteInternetAddr *net.UDPAddr  // remote internet address
	readAckSN          uint64        // 接收的ack序号，如果大于0xffffffff表示溢出
	writeAckSN         uint64        // 发送的ack序号，递增，最新的ack序号更新remoteReadLen
	readBytes          uint64        // 接收的总字节
	writeBytes         uint64        // 发送的总字节
	readLock           sync.RWMutex  // 接收队列数据锁
	writeLock          sync.RWMutex  // 发送队列数据锁
	readTimeout        time.Time     // 读取接收队列超时
	writeTimeout       time.Time     // 写入发送队列超时
	readSignle         chan byte     // 接收队列可读信号
	writeSignle        chan byte     // 发送队列可写信号
	readLen            uint16        // 接收队列长度
	writeLen           uint16        // 发送队列长度
	readCap            uint16        // 接收队列最大长度
	writeCap           uint16        // 发送队列最大长度
	readSN             uint32        // 接收队列的连续数据最大sn
	writeSN            uint32        // 发送队列的下一个sn
	remoteWriteMSS     uint16        // 接收队列的mss，remote发送队列的mss
	writeMSS           uint16        // 发送队列的mss
	readDataHead       *readData     // 可读的数据头
	readDataTail       *readData     // 可读的数据尾
	readHead           *readData     // 接收队列
	writeHead          *writeData    // 发送队列
	writeTail          *writeData    // 发送队列，添加时直接添加到末尾
	remoteReadLen      uint16        // 发送队列，窗口控制，最多能发多少个segment
	rto                time.Duration // 超时重发
	rtt                time.Duration // 实时RTT，用于计算rto
	avrRTO             time.Duration // 平均RTT，用于计算rto
	minRTO             time.Duration // 最小rto，防止发送"过快"
	maxRTO             time.Duration // 最大rto，防止发送"假死"
	buff               [maxMSS]byte  // 发送dialSegment和closeSegment的缓存
}

// net.Conn接口
func (c *Conn) Read(b []byte) (int, error) {
	n := 0
	// 没有设置超时
	c.readLock.RLock()
	if c.readTimeout.IsZero() {
		c.readLock.RUnlock()
		for {
			c.readLock.Lock()
			n = c.read(b)
			c.readLock.Unlock()
			if n == 0 {
				// 被关闭连接
				if c.state&shutdownState != 0 {
					return 0, io.EOF
				}
				// 连接状态
				if c.state == connectState {
					select {
					case <-c.readSignle:
						continue
					case <-c.stateSignal:
						// 继续读完缓存中的数据
						continue
					}
				}
				// 主动关闭，或无效
				return 0, c.netOpError("read", errConnClosed)
			}
			return n, nil
		}
	}
	// 设置了超时
	duration := time.Until(c.readTimeout)
	c.readLock.RUnlock()
	if duration <= 0 {
		return 0, c.netOpError("read", new(timeoutError))
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		c.readLock.Lock()
		n = c.read(b)
		c.readLock.Unlock()
		if n == 0 {
			// 被关闭连接
			if c.state&shutdownState != 0 {
				return 0, io.EOF
			}
			// 连接状态
			if c.state == connectState {
				select {
				case <-timer.C:
					return 0, c.netOpError("read", new(timeoutError))
				case <-c.readSignle:
					continue
				case <-c.stateSignal:
					// 继续读完缓存中的数据
					continue
				}
			}
			// 主动关闭，或无效
			return 0, c.netOpError("read", errConnClosed)
		}
		return n, nil
	}
}

// net.Conn接口
func (c *Conn) Write(b []byte) (int, error) {
	// 没有设置超时
	m, n := 0, 0
	c.writeLock.RLock()
	if c.writeTimeout.IsZero() {
		c.writeLock.RUnlock()
		for {
			if c.state != connectState {
				return 0, c.netOpError("write", errConnClosed)
			}
			c.writeLock.Lock()
			n = c.write(b[m:])
			c.writeLock.Unlock()
			if n == 0 {
				select {
				case <-c.writeSignle:
					continue
				case <-c.stateSignal:
					return 0, c.netOpError("write", errConnClosed)
				}
			}
			m += n
			if m == len(b) {
				return n, nil
			}
		}
	}
	// 设置了超时
	duration := time.Until(c.writeTimeout)
	c.writeLock.RUnlock()
	if duration <= 0 {
		return 0, c.netOpError("write", new(timeoutError))
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		if c.state != connectState {
			return 0, c.netOpError("write", errConnClosed)
		}
		c.writeLock.Lock()
		n = c.write(b[m:])
		c.writeLock.Unlock()
		if n == 0 {
			select {
			case <-timer.C:
				return 0, c.netOpError("write", new(timeoutError))
			case <-c.writeSignle:
				continue
			case <-c.stateSignal:
				return 0, c.netOpError("write", errConnClosed)
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
	if c.state != connectState {
		c.lock.Unlock()
		return c.netOpError("close", errConnClosed)
	}
	c.state |= shutdownState
	c.lock.Unlock()
	// closeSegment
	c.buff[segmentType] = closeSegment | c.from
	binary.BigEndian.PutUint32(c.buff[segmentToken:], c.token)
	putUint24(c.buff[closeSegmentSN:], c.writeSN)
	binary.BigEndian.PutUint64(c.buff[closeSegmentTimestamp:], c.timestamp)
	return nil
}

// net.Conn接口
func (c *Conn) LocalAddr() net.Addr {
	return c.localListenAddr
}

// net.Conn接口
func (c *Conn) RemoteAddr() net.Addr {
	return c.remoteInternetAddr
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

// local的公网地址
func (c *Conn) LocalInternetAddr() net.Addr {
	return c.localInternetAddr
}

// remote的监听地址
func (c *Conn) RemoteListenAddr() net.Addr {
	return c.remoteListenAddr
}

// local地址是否nat(Network Address Translation)
func (c *Conn) IsNat() bool {
	ip1 := c.localListenAddr.IP.To16()
	ip2 := c.localInternetAddr.IP.To16()
	return ip1.Equal(ip2) && c.localListenAddr.Port == c.localInternetAddr.Port
}

// remote地址是否在nat(Network Address Translation)
func (c *Conn) IsRemoteNat() bool {
	ip1 := c.remoteListenAddr.IP.To16()
	ip2 := c.remoteInternetAddr.IP.To16()
	return ip1.Equal(ip2) && c.remoteListenAddr.Port == c.remoteInternetAddr.Port
}

// 设置最小rto
func (c *Conn) SetMinRTO(rto time.Duration) {
	if rto < 1 {
		c.minRTO = connMinRTO
	} else {
		c.minRTO = rto
	}
	if c.minRTO > c.maxRTO {
		c.maxRTO = c.minRTO
	}
	if c.minRTO > c.rto {
		c.rto = c.minRTO
	}
}

// 设置最大rto
func (c *Conn) SetMaxRTO(rto time.Duration) {
	if rto < 1 {
		c.maxRTO = connMaxRTO
	} else {
		c.maxRTO = rto
	}
}

// 设置接收缓存大小
func (c *Conn) SetReadBuffer(n int) {
	m := 0
	if n <= int(c.remoteWriteMSS) {
		m = 1
	} else {
		m = n / int(c.remoteWriteMSS)
		if n%int(c.remoteWriteMSS) != 0 {
			m++
		}
	}
	if m > connMaxReadQueue {
		m = connMaxReadQueue
	}
	c.readCap = uint16(m)
}

// 设置发送缓存大小
func (c *Conn) SetWriteBuffer(n int) {
	m := 0
	if n <= int(c.writeMSS) {
		m = 1
	} else {
		m = n / int(c.writeMSS)
		if n%int(c.writeMSS) != 0 {
			m++
		}
	}
	if m > connMaxWriteQueue {
		m = connMaxWriteQueue
	}
	c.writeCap = uint16(m)
}

// 返回net.OpError
func (c *Conn) netOpError(op string, err error) error {
	return &net.OpError{
		Op:     op,
		Net:    "udp",
		Source: c.remoteInternetAddr,
		Addr:   c.localListenAddr,
		Err:    err,
	}
}

// 计算rto
func (c *Conn) calculateRTO(rtt time.Duration) {
	c.avrRTO = (3*c.avrRTO + c.rtt - rtt) / 4
	c.rtt = (7*c.rtt + rtt) / 8
	c.rto = c.rtt + 4*c.avrRTO
	if c.minRTO != 0 && c.rto < c.minRTO {
		c.rto = c.minRTO
	}
	if c.maxRTO != 0 && c.rto > c.maxRTO {
		c.rto = c.maxRTO
	}
}

// 初始化rto
func (c *Conn) initRTO(rtt time.Duration) {
	c.rtt = rtt
	c.rto = rtt / 2
	c.avrRTO = c.rto
	c.minRTO = connMinRTO
	c.maxRTO = connMaxRTO
}

// 返回一个*readData
func (c *Conn) newReadData(sn uint32, data []byte, next *readData) *readData {
	d := readDataPool.Get().(*readData)
	d.idx = 0
	d.sn = sn
	d.len = uint16(copy(d.buf[0:], data))
	d.next = next
	c.readLen++
	return d
}

// 返回一个*writeData
func (c *Conn) newWriteData() *writeData {
	d := writeDataPool.Get().(*writeData)
	d.sn = c.writeSN
	c.writeSN++
	// 24位溢出
	if c.writeSN > maxDataSN {
		c.writeSN = 0
	}
	d.next = nil
	d.first = time.Time{}
	d.buf[segmentType] = c.from | dataSegment
	binary.BigEndian.PutUint32(d.buf[segmentToken:], c.token)
	putUint24(d.buf[dataSegmentSN:], d.sn)
	d.len = dataSegmentPayload
	c.writeLen++
	return d
}

// 读取连续的数据，返回0表示没有数据，返回-1表示io.EOF
func (c *Conn) read(b []byte) int {
	if c.readDataHead == nil {
		return 0
	}
	n, m := 0, 0
	for {
		// 拷贝数据到buf
		m = copy(b[n:], c.readDataHead.buf[c.readDataHead.idx:c.readDataHead.len])
		n += m
		c.readDataHead.idx += uint16(m)
		// 数据块数据拷贝完了，移除
		if c.readDataHead.idx >= c.readDataHead.len {
			d := c.readDataHead
			c.readDataHead = c.readDataHead.next
			readDataPool.Put(d)
			c.readLen--
		}
		// 没有数据，或者buf满了
		if c.readDataHead == nil {
			return n
		}
		if n == len(b) {
			return n
		}
	}
}

// 尝试添加一个数据块，成功返回true
func (c *Conn) addReadData(sn uint32, data []byte) bool {
	// 正好是下一个sn
	if sn == c.readSN {
		// 直接添加到可读的队列
		if c.readDataHead == nil {
			c.readDataHead = c.newReadData(sn, data, nil)
			c.readDataTail = c.readDataHead
		} else {
			c.readDataTail.next = c.newReadData(sn, data, nil)
			c.readDataTail = c.readDataTail.next
		}
		c.readSN++
		if c.readSN > maxDataSN {
			c.readSN = 0
		}
		// 检查接收队列的sn，是否与readSN接上
		for c.readHead != nil && c.readHead.sn == c.readSN {
			// 添加到可读队列
			c.readDataTail.next = c.readHead
			// 下一个
			c.readHead = c.readHead.next
			c.readSN++
			if c.readSN > maxDataSN {
				c.readSN = 0
			}
		}
		return true
	}
	// 是否在接收范围
	maxSN := c.readSN + uint32(c.readCap)
	if maxSN > maxDataSN {
		// readSN...head...maxDataSN...maxSN
		maxSN -= maxDataSN
		// ...maxSN readSN...head...maxDataSN
		if sn > maxSN {
			// ...maxSN readSN...sn...head...maxDataSN
			if sn < c.readSN {
				return false
			}
		} else {
			// ...sn...maxSN readSN...head...maxDataSN
			if c.readHead == nil {
				c.readHead = c.newReadData(sn, data, nil)
			} else {
				p := c.readHead
				// readSN...head...maxDataSN
				for p.next != nil && p.next.sn > p.sn {
					p = p.next
				}
				// ...sn...maxSN
				for p.next != nil {
					if sn == p.next.sn {
						return false
					}
					if sn < p.next.sn {
						break
					}
					p = p.next
				}
				p.next = c.newReadData(sn, data, p.next)
			}
			return true
		}
	} else {
		// readSN...head...sn...maxSN
		if sn < c.readSN || sn > maxSN {
			return false
		}
	}
	// readSN...head...sn...maxSN
	if c.readHead == nil {
		c.readHead = c.newReadData(sn, data, nil)
	} else {
		if sn == c.readHead.sn {
			return false
		}
		if sn < c.readHead.sn {
			c.readHead = c.newReadData(sn, data, c.readHead)
		} else {
			p := c.readHead
			for p.next != nil {
				if sn == p.next.sn {
					return false
				}
				if sn < p.next.sn {
					break
				}
				p = p.next
			}
			p.next = c.newReadData(sn, data, p.next)
		}
	}
	return true
}

// 写入数据，返回0表示队列满了无法写入
func (c *Conn) write(data []byte) int {
	// 还能添加多少个数据包
	maxAdd := c.writeCap - c.writeLen
	if maxAdd <= 0 {
		return 0
	}
	n, m := 0, 0
	if c.writeHead == nil {
		// 队列中没有数据
		c.writeHead = c.newWriteData()
		c.writeTail = c.writeHead
		m = copy(c.writeTail.buf[c.writeTail.len:c.writeMSS], data)
		c.writeTail.len += uint16(m)
		n += m
		data = data[m:]
		maxAdd--
	} else {
		// 检查最后一个数据包是否"满数据"，有可以写的空间，而且没有被发送过
		if c.writeTail.len < c.writeMSS && c.writeTail.first.IsZero() {
			m = copy(c.writeTail.buf[c.writeTail.len:c.writeMSS], data)
			c.writeTail.len += uint16(m)
			n += m
			data = data[m:]
		}
	}
	// 新的数据包
	for maxAdd > 0 && len(data) > 0 {
		c.writeTail.next = c.newWriteData()
		c.writeTail = c.writeTail.next
		m = copy(c.writeTail.buf[c.writeTail.len:c.writeMSS], data)
		c.writeTail.len += uint16(m)
		n += m
		data = data[m:]
		maxAdd--
	}
	return n
}

// 移除发送队列小于sn的数据包
func (c *Conn) removeWriteDataBefore(maxSN uint32) {
	for {
		if c.writeHead.sn > maxSN {
			return
		}
		n := c.writeHead
		c.writeHead = c.writeHead.next
		writeDataPool.Put(n)
		c.writeLen--
		if c.writeHead == nil {
			return
		}
	}
}

// 移除发送队列指定sn的数据包
func (c *Conn) removeWriteDataAt(sn uint32) {
	// 第一个
	if sn == c.writeHead.sn {
		c.calculateRTO(time.Until(c.writeHead.first))
		n := c.writeHead
		c.writeHead = c.writeHead.next
		writeDataPool.Put(n)
		c.writeLen--
		return
	}
	// 剩下的
	p := c.writeHead
	for p.next != nil {
		if sn < p.next.sn {
			return
		}
		if sn == p.next.sn {
			c.calculateRTO(time.Until(p.next.first))
			n := p.next
			p.next = n.next
			writeDataPool.Put(n)
			c.writeLen--
			if p.next == nil {
				c.writeTail = p
			}
			return
		}
		p = p.next
	}
}

// 移除发送队列的数据包
func (c *Conn) removeWriteData(sn, maxSN uint32) bool {
	// 检查sn是否在发送窗口范围
	if c.writeHead == nil {
		return false
	}
	if c.writeHead.sn <= c.writeTail.sn {
		// head...sn...tail
		if sn < c.writeHead.sn || sn > c.writeTail.sn {
			return false
		}
		writeLen := c.writeLen
		// head...maxSN...tail
		if maxSN >= c.writeHead.sn && maxSN <= c.writeTail.sn {
			c.removeWriteDataBefore(maxSN)
		}
		// head...maxSN...sn...tail
		if sn > maxSN && c.writeHead != nil {
			c.removeWriteDataAt(sn)
		}
		return writeLen != c.writeLen
	} else {
		// ...tail head...maxDataSN
		if sn < c.writeHead.sn && sn > c.writeTail.sn {
			return false
		}
		writeLen := c.writeLen
		if maxSN <= c.writeTail.sn {
			// ...maxSN...tail head...maxDataSN
			if c.writeHead.next != nil {
				p := c.writeHead
				// head...maxDataSN
				for {
					if p.next.sn < p.sn {
						break
					}
					n := p.next
					p.next = p.next.next
					writeDataPool.Put(n)
					c.writeLen--
				}
				// head...maxSN...tail
				if maxSN >= c.writeHead.sn && maxSN <= c.writeTail.sn {
					c.removeWriteDataBefore(maxSN)
				}
				// ...maxSN...sn...tail head...maxDataSN
				if sn > maxSN && sn < c.writeTail.sn && c.writeHead != nil {
					c.removeWriteDataAt(sn)
				}
			}
		} else {
			// ...tail head...maxSN...maxDataSN
			c.removeWriteDataBefore(maxSN)
			if c.writeHead != nil {
				if sn >= c.writeHead.sn {
					// ...tail head...maxSN...sn...maxDataSN
					if sn > maxSN {
						c.removeWriteDataAt(sn)
					}
				} else {
					// ...sn...tail head...maxSN...maxDataSN
					p := c.writeHead
					// p...sn...tail
					for p.next != nil {
						if p.next.sn < p.sn {
							break
						}
						p = p.next
					}
					for p.next != nil {
						if sn < p.next.sn {
							return writeLen != c.writeLen
						}
						if sn == p.next.sn {
							c.calculateRTO(time.Until(p.next.first))
							n := p.next
							p.next = n.next
							writeDataPool.Put(n)
							c.writeLen--
							if p.next == nil {
								c.writeTail = p
							}
							break
						}
						p = p.next
					}
				}
			}
		}
		return writeLen != c.writeLen
	}
}
func (c *Conn) releaseReadData() {
	p := c.readHead
	for p != nil {
		d := p
		p = p.next
		readDataPool.Put(d)
	}
}

func (c *Conn) releaseWriteData() {
	p := c.writeHead
	for p != nil {
		d := p
		p = p.next
		writeDataPool.Put(d)
	}
}

// 检查发送队列，超时重发
func (c *Conn) checkRTO(now time.Time, rudp *RUDP) {
	// 需要发送的数据包个数
	n := c.writeLen
	// 没有数据
	if n < 1 {
		if c.state&shutdownState != 0 {
			// 应用层关闭连接，发送closeSegment
			rudp.WriteToConn(c.buff[:closeSegmentLength], c)
		}
		return
	}
	// 不能超过最大发送个数
	if n > c.remoteReadLen {
		n = c.remoteReadLen
	}
	// 开始遍历发送队列
	p := c.writeHead
	for p != nil && n > 0 {
		if p.first.IsZero() {
			// 第一次发送
			p.first = now
			rudp.WriteToConn(p.buf[:p.len], c)
			p.last = now
			n--
		} else {
			// 超时重传
			if now.Sub(p.last) >= p.rto {
				rudp.WriteToConn(p.buf[:p.len], c)
				p.last = now
				p.rto += c.rto
				if p.rto > c.maxRTO {
					p.rto = c.maxRTO
				}
				n--
			}
		}
		p = p.next
	}
}

func (c *Conn) encConnectSegment(buff []byte, segType byte) {
	buff[segmentType] = segType | c.from
	buff[connectSegmentVersion] = protolVersion
	binary.BigEndian.PutUint32(buff[segmentToken:], c.token)
	binary.BigEndian.PutUint64(buff[connectSegmentTimestamp:], c.timestamp)
	copy(buff[connectSegmentLocalIP:], c.localListenAddr.IP.To16())
	binary.BigEndian.PutUint16(buff[connectSegmentLocalPort:], uint16(c.localListenAddr.Port))
	copy(buff[connectSegmentRemoteIP:], c.remoteInternetAddr.IP.To16())
	binary.BigEndian.PutUint16(buff[connectSegmentRemotePort:], uint16(c.remoteInternetAddr.Port))
	binary.BigEndian.PutUint16(buff[connectSegmentMSS:], c.writeMSS)
	binary.BigEndian.PutUint16(buff[connectSegmentReadQueue:], c.readCap)
}

func (c *Conn) decConnectSegment(seg *segment) {
	copy(c.remoteListenAddr.IP, seg.b[connectSegmentLocalIP:])
	c.remoteListenAddr.Port = int(binary.BigEndian.Uint16(seg.b[connectSegmentLocalPort:]))
	copy(c.localInternetAddr.IP, seg.b[connectSegmentRemoteIP:])
	c.localInternetAddr.Port = int(binary.BigEndian.Uint16(seg.b[connectSegmentRemotePort:]))
	c.remoteWriteMSS = binary.BigEndian.Uint16(seg.b[connectSegmentMSS:])
	if c.remoteWriteMSS == 0 {
		c.remoteWriteMSS = minMSS
	}
	c.remoteReadLen = binary.BigEndian.Uint16(seg.b[connectSegmentReadQueue:])
}

func (c *Conn) encAckSegment(seg *segment, sn uint32) {
	seg.b[segmentType] = ackSegment | c.from
	binary.BigEndian.PutUint32(seg.b[segmentToken:], c.token)
	putUint24(seg.b[ackSegmentDataSN:], sn)
	if c.readSN == 0 {
		putUint24(seg.b[ackSegmentDataMaxSN:], maxDataSN)
	} else {
		putUint24(seg.b[ackSegmentDataMaxSN:], c.readSN-1)
	}
	binary.BigEndian.PutUint16(seg.b[ackSegmentReadQueueLength:], c.readCap-c.readLen)
	binary.BigEndian.PutUint64(seg.b[ackSegmentTimestamp:], c.writeAckSN)
	c.writeAckSN++
}
