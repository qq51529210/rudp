package rudp

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"
)

const (
	dialState    = 1 << iota // 正在建立连接
	connectState             // 已经确认连接
	closedState              // 对面关闭连接
	invalidState             // 无效的连接
)

const (
	connMaxReadQueue  = 0xffff
	connMaxWriteQueue = 0xffff
)

var (
	readDataPool    sync.Pool                // readData pool
	writeDataPool   sync.Pool                // writeData pool
	connWriteQueue  = uint16(1024)           // 默认的conn的发送队列长度
	connReadQueue   = uint16(1024)           // 默认的conn的接收队列长度
	connHandleQueue = uint16(1024)           // 默认的conn的处理队列长度
	connMinRTO      = 100 * time.Millisecond // 最小超时重传，毫秒
	connMaxRTO      = 400 * time.Millisecond // 最大超时重传，毫秒
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
	sn    uint32       // 序号，24位
	buff  [maxMSS]byte // 数据
	len   uint16       // 数据大小，因为write有可能写不完数据块
	next  *writeData   // 下一个
	first time.Time    // 第一次被发送的时间，用于计算rto
	last  time.Time    // 上一次被发送的时间，超时重发判断
}

type Conn struct {
	from            byte            // 0:client，1:server
	token           uint32          // 连接token
	timestamp       uint64          // 连接的时间戳
	state           byte            // 状态
	lock            sync.RWMutex    // 同步锁
	stateSignal     chan byte       // 状态改变的信号
	handleQueue     chan *segment   // 待处理的数据缓存
	timer           *time.Timer     // 计时器
	listenAddr      [2]*net.UDPAddr // local/remote的监听地址
	internetAddr    [2]*net.UDPAddr // local/remote的互联网地址
	ackSN           [2]uint32       // 发送ack的次数，递增，收到新的max sn时，重置为0。0:local，1:remote
	ioBytes         [2]uint64       // read/write的总字节
	dataLock        [2]sync.RWMutex // 接收/发送队列数据锁
	dataTimeout     [2]time.Time    // 应用层设置的io读/写超时
	dataEnable      [2]chan byte    // 接收/发送队列，可读/写通知
	dataLen         [2]uint16       // 接收/发送队列长度
	dataCap         [2]uint16       // 接收/发送队列最大长度
	dataSN          [2]uint32       // 24位，0:接收队列最大连续序号，小于sn的数据块是可读的。1:发送队列下一个数据块的sn
	dataMSS         [2]uint16       // 0:接收队列的mss参考。1:发送队列数据块的大小
	readDataHead    *readData       // 接收队列，第一个数据块
	writeDataHead   *writeData      // 发送队列，第一个数据块
	writeDataTail   *writeData      // 发送队列，最后一个数据块，添加时直接添加到末尾
	remoteReadQueue uint16          // 发送队列，窗口控制，最多能发多少个segment
	rto             time.Duration   // 超时重发
	rtt             time.Duration   // 实时RTT，用于计算rto
	avrRTO          time.Duration   // 平均RTT，用于计算rto
	minRTO          time.Duration   // 最小rto，防止发送"过快"
	maxRTO          time.Duration   // 最大rto，防止发送"假死"
	buff            [maxMSS]byte    // 发送dialSegment和closeSegment的缓存
}

// net.Conn接口
func (c *Conn) Read(b []byte) (int, error) {
	n := 0
	// 没有设置超时
	c.dataLock[0].RLock()
	if c.dataTimeout[0].IsZero() {
		c.dataLock[0].RUnlock()
		for {
			c.dataLock[0].Lock()
			n = c.read(b)
			c.dataLock[0].Unlock()
			if n == 0 {
				if c.state&closedState != 0 {
					return 0, io.EOF
				}
				if c.state&connectState != 0 {
					select {
					case <-c.dataEnable[0]:
						continue
					case <-c.stateSignal:
						if c.state&closedState != 0 {
							return 0, io.EOF
						}
					}
				}
				return 0, c.netOpError("read", connClosedError)
			}
			return n, nil
		}
	}
	// 设置了超时
	duration := c.dataTimeout[0].Sub(time.Now())
	c.dataLock[0].RUnlock()
	if duration <= 0 {
		return 0, c.netOpError("read", new(timeoutError))
	}
	for {
		c.dataLock[0].Lock()
		n = c.read(b)
		c.dataLock[0].Unlock()
		if n == 0 {
			if c.state&closedState != 0 {
				return 0, io.EOF
			}
			if c.state&connectState != 0 {
				select {
				case <-time.After(duration):
					return 0, c.netOpError("read", new(timeoutError))
				case <-c.dataEnable[0]:
					continue
				case <-c.stateSignal:
					if c.state&closedState != 0 {
						return 0, io.EOF
					}
				}
			}
			return 0, c.netOpError("read", connClosedError)
		}
		return n, nil
	}
}

// net.Conn接口
func (c *Conn) Write(b []byte) (int, error) {
	// 没有设置超时
	m, n := 0, 0
	c.dataLock[1].RLock()
	if c.dataTimeout[1].IsZero() {
		c.dataLock[1].RUnlock()
		for {
			if c.state != connectState {
				return 0, c.netOpError("write", connClosedError)
			}
			c.dataLock[1].Lock()
			n = c.addWriteData(b[m:])
			c.dataLock[1].Unlock()
			if n == 0 {
				select {
				case <-c.dataEnable[1]:
					continue
				case <-c.stateSignal:
					return 0, c.netOpError("write", connClosedError)
				}
			}
			m += n
			if m == len(b) {
				return n, nil
			}
		}
	}
	// 设置了超时
	duration := c.dataTimeout[1].Sub(time.Now())
	c.dataLock[1].RUnlock()
	if duration <= 0 {
		return 0, c.netOpError("write", new(timeoutError))
	}
	for {
		if c.state != connectState {
			return 0, c.netOpError("write", connClosedError)
		}
		c.dataLock[1].Lock()
		n = c.addWriteData(b[m:])
		c.dataLock[1].Unlock()
		if n == 0 {
			select {
			case <-c.dataEnable[1]:
				continue
			case <-c.stateSignal:
				return 0, c.netOpError("write", connClosedError)
			case <-time.After(duration):
				return 0, c.netOpError("write", new(timeoutError))
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
		return c.netOpError("close", connClosedError)
	}
	c.state = closedState
	c.lock.Unlock()
	return nil
}

// net.Conn接口
func (c *Conn) LocalAddr() net.Addr {
	return c.listenAddr[0]
}

// net.Conn接口
func (c *Conn) RemoteAddr() net.Addr {
	return c.internetAddr[1]
}

// net.Conn接口
func (c *Conn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	c.SetWriteDeadline(t)
	return nil
}

// net.Conn接口
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.dataLock[0].Lock()
	c.dataTimeout[0] = t
	c.dataLock[0].Unlock()
	return nil
}

// net.Conn接口
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.dataLock[1].Lock()
	c.dataTimeout[1] = t
	c.dataLock[1].Unlock()
	return nil
}

// local的公网地址
func (c *Conn) LocalInternetAddr() net.Addr {
	return c.internetAddr[0]
}

// remote的监听地址
func (c *Conn) RemoteListenAddr() net.Addr {
	return c.listenAddr[1]
}

// local地址是否nat(Network Address Translation)
func (c *Conn) IsNat() bool {
	return bytes.Compare(c.internetAddr[0].IP.To16(), c.listenAddr[0].IP.To16()) == 0 &&
		c.internetAddr[0].Port == c.listenAddr[0].Port
}

// remote地址是否在nat(Network Address Translation)
func (c *Conn) IsRemoteNat() bool {
	return bytes.Compare(c.internetAddr[1].IP.To16(), c.listenAddr[1].IP.To16()) == 0 &&
		c.internetAddr[1].Port == c.listenAddr[1].Port
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
	if n <= int(c.dataMSS[1]) {
		m = 1
	} else {
		m = n / int(c.dataMSS[1])
		if n%int(c.dataMSS[1]) != 0 {
			m++
		}
	}
	if m > connMaxReadQueue {
		m = connMaxReadQueue
	}
	c.dataCap[0] = uint16(m)
}

// 设置发送缓存大小
func (c *Conn) SetWriteBuffer(n int) {
	m := 0
	if n <= int(c.dataMSS[0]) {
		m = 1
	} else {
		m = n / int(c.dataMSS[0])
		if n%int(c.dataMSS[0]) != 0 {
			m++
		}
	}
	if m > connMaxWriteQueue {
		m = connMaxWriteQueue
	}
	c.dataCap[1] = uint16(m)
}

// 返回net.OpError
func (c *Conn) netOpError(op string, err error) error {
	return &net.OpError{
		Op:     op,
		Net:    "udp",
		Source: c.internetAddr[1],
		Addr:   c.listenAddr[0],
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
	n, m := 0, 0
	for c.readDataHead != nil {
		if c.readDataHead.sn > c.dataSN[0] {
			break
		}
		// 拷贝数据到buf
		m = copy(b[n:], c.readDataHead.data[c.readDataHead.idx:])
		n += m
		c.readDataHead.idx += uint16(m)
		// 数据块数据拷贝完了，从队列中移除
		if c.readDataHead.idx == uint16(len(c.readDataHead.data)) {
			c.removeReadDataFront()
		}
		// buf满了
		if n == len(b) {
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
	c.dataLen[0]++
	return d
}

// 尝试添加一个数据块，成功返回true
func (c *Conn) addReadData(sn uint32, data []byte) bool {
	// 是否在接收范围
	maxSN := c.dataSN[0] + uint32(c.dataCap[0])
	if maxSN > maxDataSN {
		// 溢出的情况
		maxSN -= maxDataSN
		if sn < c.dataSN[0] && sn > maxSN {
			return false
		}
		if c.readDataHead == nil {
			c.readDataHead = c.newReadData(sn, data, nil)
		} else {
			p := c.readDataHead
			if sn == p.sn {
				return false
			}
			if sn < p.sn {
				c.readDataHead = c.newReadData(sn, data, p)
			} else {
				n := p
				p = p.next
				for p != nil {
					if sn == p.sn {
						return false
					}
					if sn >= c.dataSN[0] && sn < p.sn {
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
		if sn < c.dataSN[0] || sn > maxSN {
			return false
		}
		if c.readDataHead == nil {
			c.readDataHead = c.newReadData(sn, data, nil)
		} else {
			p := c.readDataHead
			if sn == p.sn {
				return false
			}
			if sn < p.sn {
				c.readDataHead = c.newReadData(sn, data, p)
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
	p := c.readDataHead
	for p != nil && p.sn == c.dataSN[0] {
		c.dataSN[0]++
		if c.dataSN[0] > maxDataSN {
			c.dataSN[0] = 0
		}
		p = p.next
	}
	return true
}

// 新的添加到发送队列的writeData
func (c *Conn) newWriteData() *writeData {
	d := writeDataPool.Get().(*writeData)
	d.sn = c.dataSN[1]
	c.dataSN[1]++
	// 24位溢出
	if c.dataSN[1] >= maxDataSN {
		c.dataSN[1] = 0
	}
	d.next = nil
	d.first = time.Time{}
	d.buff[segmentType] = c.from | dataSegment
	binary.BigEndian.PutUint32(d.buff[segmentToken:], c.token)
	putUint24(d.buff[dataSegmentSN:], d.sn)
	d.len = dataSegmentPayload
	c.dataLen[1]++
	return d
}

// 写入数据，返回0表示队列满了无法写入
func (c *Conn) addWriteData(data []byte) int {
	// 还能添加多少个数据包
	maxAdd := c.dataCap[1] - c.dataLen[1]
	if maxAdd <= 0 {
		return 0
	}
	n, m := 0, 0
	if c.writeDataHead == nil {
		// 队列中没有数据
		c.writeDataHead = c.newWriteData()
		c.writeDataTail = c.writeDataHead
		m = copy(c.writeDataTail.buff[c.writeDataTail.len:c.dataMSS[1]], data)
		c.writeDataTail.len += uint16(m)
		maxAdd--
		n += m
		data = data[m:]
	} else {
		// 检查最后一个数据包是否"满数据"，有可以写的空间，没有被发送过
		if c.writeDataTail.len < c.dataMSS[1] && c.writeDataTail.first.IsZero() {
			m = copy(c.writeDataTail.buff[c.writeDataTail.len:c.dataMSS[1]], data)
			c.writeDataTail.len += uint16(m)
			n += m
			data = data[m:]
		}
	}
	// 新的数据包
	for maxAdd > 0 && len(data) > 0 {
		c.writeDataTail.next = c.newWriteData()
		c.writeDataTail = c.writeDataTail.next
		m = copy(c.writeDataTail.buff[c.writeDataTail.len:c.dataMSS[1]], data)
		c.writeDataTail.len += uint16(m)
		maxAdd--
		n += m
		data = data[m:]
	}
	return n
}

// 移除第一个数据块
func (c *Conn) removeReadDataFront() {
	d := c.readDataHead
	c.readDataHead = c.readDataHead.next
	c.dataLen[0]--
	readDataPool.Put(d)
}

// 移除sn前面的所有数据包
func (c *Conn) removeWriteDataBefore(sn uint32) bool {
	// 检查sn是否在发送窗口范围
	if c.writeDataHead == nil || sn < c.writeDataHead.sn || sn > c.dataSN[1] {
		return false
	}
	// 遍历发送队列数据包
	p := c.writeDataHead
	var n *writeData
	for {
		// 因为是递增有序队列，sn如果小于当前，就没必要继续
		if sn < p.sn {
			break
		}
		// 计算rto
		if sn == p.sn {
			c.calculateRTO(time.Now().Sub(p.first))
			n = p.next
			writeDataPool.Put(p)
			c.dataLen[1]--
			p = n
			if p == nil {
				c.writeDataTail = nil
			}
			break
		}
		n = p.next
		writeDataPool.Put(p)
		c.dataLen[1]--
		p = n
		if p == nil {
			c.writeDataTail = nil
			break
		}
	}
	c.writeDataHead = p
	return true
}

// 移除指定sn数据包，成功返回true
func (c *Conn) removeWriteData(sn uint32) bool {
	// 检查sn是否在发送窗口范围
	if c.writeDataHead == nil || sn < c.writeDataHead.sn || sn > c.writeDataTail.sn {
		return false
	}
	// 遍历发送队列数据块
	if sn == c.writeDataHead.sn {
		// 计算rto
		c.calculateRTO(time.Now().Sub(c.writeDataHead.first))
		// 从队列移除
		p := c.writeDataHead
		c.writeDataHead = c.writeDataHead.next
		if c.writeDataHead == nil {
			c.writeDataTail = nil
		}
		writeDataPool.Put(p)
		c.dataLen[1]--
		return true
	}
	prev := c.writeDataHead
	p := prev.next
	for p != nil {
		// 因为是递增有序队列，sn如果小于当前，就没必要继续
		if sn < p.sn {
			return false
		}
		if sn == p.sn {
			// 计算rto
			c.calculateRTO(time.Now().Sub(p.first))
			// 从队列移除
			prev.next = p.next
			// 移除的是最后一个
			if p == c.writeDataTail {
				c.writeDataTail = prev
			}
			writeDataPool.Put(p)
			c.dataLen[1]--
			return true
		}
		prev = p
		p = prev.next
	}
	return false
}

// 回收接收队列
func (c *Conn) releaseReadData() {
	p := c.readDataHead
	for p != nil {
		d := p
		p = p.next
		readDataPool.Put(d)
	}
}

// 回收发送队列
func (c *Conn) releaseWriteData() {
	p := c.writeDataHead
	for p != nil {
		d := p
		p = p.next
		writeDataPool.Put(d)
	}
}

// 检查发送队列，超时重发
func (c *Conn) checkRTO(now time.Time, rudp *RUDP) {
	// 需要发送的数据包个数
	n := c.dataLen[1]
	// 没有数据
	if n < 1 {
		if c.state&closedState != 0 {
			// 应用层关闭连接，发送closeSegment
			c.buff[segmentType] = closeSegment | c.from
			binary.BigEndian.PutUint32(c.buff[segmentToken:], c.token)
			putUint24(c.buff[closeSegmentSN:], c.dataSN[1])
			binary.BigEndian.PutUint64(c.buff[closeSegmentTimestamp:], c.timestamp)
			rudp.WriteToConn(c.buff[:closeSegmentLength], c)
		}
		return
	}
	// 不能超过最大发送个数
	if n > c.remoteReadQueue {
		n = c.remoteReadQueue
	}
	// 如果remote没有接收空间，发1个
	if n == 0 {
		n = 1
	}
	// 开始遍历发送队列
	p := c.writeDataHead
	for p != nil && n > 0 {
		// 第一次发送
		if p.first.IsZero() {
			p.first = now
		}
		if now.Sub(p.last) >= c.rto {
			rudp.WriteToConn(p.buff[:p.len], c)
			p.last = now
			n--
		}
		p = p.next
	}
}

func (c *Conn) encConnectSegment(buff []byte, segType byte, timeout time.Duration) {
	buff[segmentType] = segType | c.from
	buff[connectSegmentVersion] = protolVersion
	binary.BigEndian.PutUint32(buff[segmentToken:], c.token)
	binary.BigEndian.PutUint64(buff[connectSegmentTimestamp:], c.timestamp)
	copy(buff[connectSegmentLocalIP:], c.listenAddr[0].IP.To16())
	binary.BigEndian.PutUint16(buff[connectSegmentLocalPort:], uint16(c.listenAddr[0].Port))
	copy(buff[connectSegmentRemoteIP:], c.internetAddr[1].IP.To16())
	binary.BigEndian.PutUint16(buff[connectSegmentRemotePort:], uint16(c.internetAddr[1].Port))
	binary.BigEndian.PutUint16(buff[connectSegmentMSS:], c.dataMSS[0])
	binary.BigEndian.PutUint16(buff[connectSegmentReadQueue:], c.dataCap[0])
	binary.BigEndian.PutUint64(buff[connectSegmentTimeout:], uint64(timeout))
}

func (c *Conn) decConnectSegment(seg *segment) {
	copy(c.listenAddr[1].IP, seg.b[connectSegmentLocalIP:])
	c.listenAddr[1].Port = int(binary.BigEndian.Uint16(seg.b[connectSegmentLocalPort:]))
	copy(c.internetAddr[0].IP, seg.b[connectSegmentRemoteIP:])
	c.internetAddr[0].Port = int(binary.BigEndian.Uint16(seg.b[connectSegmentRemotePort:]))
	c.dataMSS[1] = binary.BigEndian.Uint16(seg.b[connectSegmentMSS:])
	if c.dataMSS[1] == 0 {
		c.dataMSS[1] = minMSS
	}
	c.remoteReadQueue = binary.BigEndian.Uint16(seg.b[connectSegmentReadQueue:])
}

func (c *Conn) writeAckSegment(rudp *RUDP, seg *segment, sn uint32) {
	seg.b[segmentType] = ackSegment | c.from
	binary.BigEndian.PutUint32(seg.b[segmentToken:], c.token)
	putUint24(seg.b[ackSegmentDataSN:], sn)
	putUint24(seg.b[ackSegmentDataMaxSN:], c.dataSN[0]-1)
	binary.BigEndian.PutUint16(seg.b[ackSegmentReadQueueFree:], c.dataCap[0]-c.dataLen[0])
	binary.BigEndian.PutUint32(seg.b[ackSegmentSN:], c.ackSN[0])
	c.ackSN[0]++
	rudp.WriteToConn(seg.b[:ackSegmentLength], c)
}
