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
	connMinRTO      = 30 * time.Millisecond   // 最小超时重传，毫秒
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
	data [maxMSS]byte // 数据
	len  uint16       // 数据大小
	idx  uint16       // 有效数据的起始，因为read有可能一次读不完数据
	next *readData    // 下一个
}

// writeQueue的数据块
type writeData struct {
	sn    uint32        // 序号，24位
	buff  [maxMSS]byte  // 数据
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
	readSN             uint32        // 接收队列的可读sn
	writeSN            uint32        // 发送队列的下一个sn
	remoteWriteMSS     uint16        // 接收队列的mss，remote发送队列的mss
	writeMSS           uint16        // 发送队列的mss
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
			n = c.addWriteData(b[m:])
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
		n = c.addWriteData(b[m:])
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

// 读取连续的数据，返回0表示没有数据，返回-1表示io.EOF
func (c *Conn) read(b []byte) int {
	if c.readHead == nil {
		return 0
	}
	n, m := 0, 0
	for {
		if c.readSN >= c.readHead.sn {
			// ...1,2,3,readSN,4,5,6...
			if c.readHead.sn > c.readSN {
				break
			}
			// } else {
			// 4,readSN,5,6...1,2,3
		}
		// 拷贝数据到buf
		m = copy(b[n:], c.readHead.data[c.readHead.idx:c.readHead.len])
		n += m
		c.readHead.idx += uint16(m)
		// 数据块数据拷贝完了，从队列中移除
		if c.readHead.idx >= c.readHead.len {
			c.removeReadDataFront()
		}
		// 没有数据，或者buf满了
		if c.readHead == nil || n == len(b) {
			return n
		}
	}
	return n
}

func (c *Conn) newReadData(sn uint32, data []byte, next *readData) *readData {
	d := readDataPool.Get().(*readData)
	d.sn = sn
	d.idx = 0
	d.len = uint16(copy(d.data[:], data))
	d.next = next
	c.readLen++
	return d
}

// 新的添加到发送队列的writeData
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
	d.buff[segmentType] = c.from | dataSegment
	binary.BigEndian.PutUint32(d.buff[segmentToken:], c.token)
	putUint24(d.buff[dataSegmentSN:], d.sn)
	d.len = dataSegmentPayload
	c.writeLen++
	return d
}

// 尝试添加一个数据块，成功返回true
func (c *Conn) addReadData(sn uint32, data []byte) bool {
	// 是否在接收范围
	maxSN := c.readSN + uint32(c.readCap)
	if maxSN > maxDataSN {
		// 溢出的情况
		maxSN -= maxDataSN
		if sn < c.readSN && sn > maxSN {
			return false
		}
		if c.readHead == nil {
			c.readHead = c.newReadData(sn, data, nil)
		} else {
			p := c.readHead
			if sn == p.sn {
				return false
			}
			if sn < p.sn {
				c.readHead = c.newReadData(sn, data, p)
			} else {
				n := p
				p = p.next
				for p != nil {
					if sn == p.sn {
						return false
					}
					if sn >= c.readSN && sn < p.sn {
						break
					} else if p.sn <= maxSN && sn < p.sn {
						break
					}
					n = p
					p = p.next
				}
				n.next = c.newReadData(sn, data, p)
			}
		}
	} else {
		if sn < c.readSN || sn > maxSN {
			return false
		}
		if c.readHead == nil {
			c.readHead = c.newReadData(sn, data, nil)
		} else {
			p := c.readHead
			if sn == p.sn {
				return false
			}
			if sn < p.sn {
				c.readHead = c.newReadData(sn, data, p)
			} else {
				n := p
				p = p.next
				for p != nil {
					if sn == p.sn {
						return false
					}
					if sn < p.sn {
						break
					}
					n = p
					p = p.next
				}
				n.next = c.newReadData(sn, data, p)
			}
		}
	}
	// 检查连续的sn
	p := c.readHead
	for p != nil && p.sn == c.readSN {
		c.readSN++
		if c.readSN > maxDataSN {
			c.readSN = 0
		}
		p = p.next
	}
	return true
}

// 写入数据，返回0表示队列满了无法写入
func (c *Conn) addWriteData(data []byte) int {
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
		m = copy(c.writeTail.buff[c.writeTail.len:c.writeMSS], data)
		c.writeTail.len += uint16(m)
		maxAdd--
		n += m
		data = data[m:]
	} else {
		// 检查最后一个数据包是否"满数据"，有可以写的空间，没有被发送过
		if c.writeTail.len < c.writeMSS && c.writeTail.first.IsZero() {
			m = copy(c.writeTail.buff[c.writeTail.len:c.writeMSS], data)
			c.writeTail.len += uint16(m)
			n += m
			data = data[m:]
		}
	}
	// 新的数据包
	for maxAdd > 0 && len(data) > 0 {
		c.writeTail.next = c.newWriteData()
		c.writeTail = c.writeTail.next
		m = copy(c.writeTail.buff[c.writeTail.len:c.writeMSS], data)
		c.writeTail.len += uint16(m)
		maxAdd--
		n += m
		data = data[m:]
	}
	return n
}

// 移除第一个数据块
func (c *Conn) removeReadDataFront() {
	d := c.readHead
	c.readHead = c.readHead.next
	c.readLen--
	readDataPool.Put(d)
}

// 移除发送队列小于sn的数据包
func (c *Conn) removeWriteDataBefore(p *writeData, sn uint32) *writeData {
	for p != nil {
		if sn < p.sn {
			break
		}
		if sn == p.sn {
			c.calculateRTO(time.Until(p.first))
		}
		n := p
		p = p.next
		writeDataPool.Put(n)
		c.writeLen--
	}
	return p
}

// 移除发送队列指定sn的数据包
func (c *Conn) removeWriteDataAt(sn uint32) bool {
	p := c.writeHead
	// 第一个
	if sn == p.sn {
		c.calculateRTO(time.Until(p.first))
		c.writeHead = p.next
		writeDataPool.Put(p)
		c.writeLen--
		if p.next == nil {
			c.writeTail = p
		}
		return true
	}
	// 剩下的
	n := p.next
	for n != nil {
		if sn < n.sn {
			return false
		}
		if sn == n.sn {
			c.calculateRTO(time.Until(n.first))
			p.next = n.next
			writeDataPool.Put(n)
			c.writeLen--
			if p.next == nil {
				c.writeTail = p
			}
			return true
		}
		p = n
		n = n.next
	}
	return false
}

// 移除发送队列的数据包
func (c *Conn) removeWriteData(sn, maxSN uint32) bool {
	// 检查sn是否在发送窗口范围
	if c.writeHead == nil {
		return false
	}
	if c.writeHead.sn <= c.writeTail.sn {
		// sn没溢出，...3,4,5,6,7,8...
		if sn < c.writeHead.sn || sn > c.writeTail.sn || maxSN < c.writeHead.sn || maxSN > c.writeTail.sn {
			return false
		}
		// 原来的数量
		writeLen := c.writeLen
		// 移除小于maxSN
		c.writeHead = c.removeWriteDataBefore(c.writeHead, maxSN)
		if c.writeHead == nil {
			c.writeTail = nil
			return writeLen != c.writeLen
		}
		// 移除sn，下面两种情况
		// ...3,4,sn,5,6,maxSN,7,8...，已
		// ...3,4,5,maxSN,6,7,sn,8...，未
		if c.writeHead != nil && sn > maxSN && c.removeWriteDataAt(sn) {
			return true
		}
		return writeLen != c.writeLen
	} else {
		// sn溢出，5,6,7,8,9...2,3,4
		if (sn < c.writeHead.sn && sn > c.writeTail.sn) || (maxSN < c.writeHead.sn && maxSN > c.writeTail.sn) {
			return false
		}
		// 原来的数量
		writeLen := c.writeLen
		// 移除小于maxSN的数据
		if maxSN <= c.writeTail.sn {
			// 5,6,7,maxSN,9...2,3,4
			p := c.writeHead
			// 先移除...2,3,4
			for p != nil {
				if p.sn < c.writeTail.sn {
					break
				}
				n := p
				p = p.next
				writeDataPool.Put(n)
				c.writeLen--
			}
			// 移除5,6,7,maxSN,...
			c.writeHead = c.removeWriteDataBefore(p, maxSN)
			if c.writeHead == nil {
				c.writeTail = nil
				return writeLen != c.writeLen
			}
			// 移除sn，下面三种情况
			// 5,6,7,maxSN,9...2,3,sn,4，已
			// 5,sn,6,7,maxSN,9...2,3,4，已
			// 5,6,maxSN,7,sn,9...2,3,4，未
			if c.writeHead != nil && sn < c.writeTail.sn && sn > maxSN && c.removeWriteDataAt(sn) {
				return true
			}
		} else {
			// 5,6,7,9...2,3,maxSN,4
			c.writeHead = c.removeWriteDataBefore(c.writeHead, maxSN)
			if c.writeHead == nil {
				c.writeTail = nil
				return writeLen != c.writeLen
			}
			// 移除sn，下面三种情况
			// 5,6,7,9...2,sn,3,maxSN,4，已
			// 5,6,7,9...2,3,maxSN,sn,4，未
			// 5,6,sn,7,9...2,3,maxSN,4，未
			if c.writeHead != nil {
				if sn >= c.writeHead.sn {
					// 5,6,7,9...2,3,maxSN,sn,4，未
					if sn > maxSN && c.removeWriteDataAt(sn) {
						return true
					}
				} else {
					// 5,6,sn,7,9...2,3,maxSN,4，未
					p := c.writeHead
					if p.sn <= c.writeTail.sn {
						if sn == p.sn {
							c.calculateRTO(time.Until(p.first))
							c.writeHead = p.next
							writeDataPool.Put(p)
							c.writeLen--
							if p.next == nil {
								c.writeTail = p
							}
							return true
						}
					} else {
						// 遍历末尾到溢出的开始
						n := p.next
						for n != nil {
							if n.sn < c.writeTail.sn {
								break
							}
							p = n
							n = n.next
						}
						if sn == n.sn {
							c.calculateRTO(time.Until(n.first))
							p.next = n.next
							writeDataPool.Put(n)
							c.writeLen--
							if p.next == nil {
								c.writeTail = p
							}
							return true
						}
						p = n
					}
					// 剩下的
					n := p.next
					for n != nil {
						if sn < n.sn {
							return false
						}
						if sn == n.sn {
							c.calculateRTO(time.Until(n.first))
							p.next = n.next
							writeDataPool.Put(n)
							c.writeLen--
							if p.next == nil {
								c.writeTail = p
							}
							return true
						}
						p = n
						n = n.next
					}
				}
			}
		}
		return writeLen != c.writeLen
	}
}

// 检查发送队列，超时重发
func (c *Conn) checkRTO(now time.Time, rudp *RUDP) {
	// 需要发送的数据包个数
	n := c.writeLen
	// 没有数据
	if n < 1 {
		if c.state&closedState != 0 {
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
		// 第一次发送
		if p.first.IsZero() {
			p.first = now
			rudp.WriteToConn(p.buff[:p.len], c)
			p.last = now
			n--
		} else {
			// 超时重传
			if now.Sub(p.last) >= p.rto {
				rudp.WriteToConn(p.buff[:p.len], c)
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
	c.writeMSS = binary.BigEndian.Uint16(seg.b[connectSegmentMSS:])
	if c.writeMSS == 0 {
		c.writeMSS = minMSS
	}
	c.remoteReadLen = binary.BigEndian.Uint16(seg.b[connectSegmentReadQueue:])
}

func (c *Conn) encAckSegment(seg *segment, sn uint32) {
	seg.b[segmentType] = ackSegment | c.from
	binary.BigEndian.PutUint32(seg.b[segmentToken:], c.token)
	putUint24(seg.b[ackSegmentDataSN:], sn)
	putUint24(seg.b[ackSegmentDataMaxSN:], c.readSN-1)
	binary.BigEndian.PutUint16(seg.b[ackSegmentReadQueueLength:], c.readCap-c.readLen)
	binary.BigEndian.PutUint64(seg.b[ackSegmentTimestamp:], c.writeAckSN)
	c.writeAckSN++
}
