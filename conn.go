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
	closedState  = iota // 关闭的连接
	dialState           // 正在建立连接，client & server
	connectState        // 已经确认连接，client & server
	closingState        // 正在关闭连接
)

var (
	connReadQueueMaxLength  = uint16(0xffff/3) * 2   // 默认的读缓存队列最大长度
	connWriteQueueMaxLength = uint16(0xffff/3) * 2   // 默认的写缓冲队列最大长度
	connHandleQueueLen      = uint16(1024)           // 默认的conn的处理队列长度
	connMinRTO              = 10 * time.Millisecond  // 最小超时重传，毫秒
	connMaxRTO              = 100 * time.Millisecond // 最大超时重传，毫秒
	emptyData               = make([]byte, 0)        // 空数据
	readDataPool            sync.Pool                // readData pool
	writeDataPool           sync.Pool                // writeData pool
)

func init() {
	readDataPool.New = func() interface{} {
		return new(readData)
	}
	writeDataPool.New = func() interface{} {
		return new(writeData)
	}
}

// 设置默认的conn的读缓存队列最大长度
func SetReadQueueMaxLength(n uint16) {
	connReadQueueMaxLength = n
}

// 设置默认的conn的写缓冲队列最大长度
func SetWriteQueueMaxLength(n uint16) {
	connWriteQueueMaxLength = n
}

// 设置默认的conn的segment处理队列长度
func SetConnHandleQueueLength(n uint16) {
	connHandleQueueLen = n
}

// net.timeout的接口
type timeoutError struct{}

func (e timeoutError) Error() string { return "timeout" }
func (e timeoutError) Timeout() bool { return true }

// readQueue的数据块
type readData struct {
	dataSN      uint16       // 序号
	data        [maxMSS]byte // 数据
	len         uint16       // 数据大小
	discardable bool         // 是否可丢弃
	idx         uint16       // 有效数据的起始，因为read有可能一次读不完数据
	next        *readData    // 下一个
}

// writeQueue的数据块
type writeData struct {
	sync.RWMutex
	discardable bool         // 可以丢弃
	dataSN      uint16       // 序号
	buff        [maxMSS]byte // 数据
	len         uint16       // 数据大小，因为write有可能写不完数据块
	next        *writeData   // 下一个
	first       time.Time    // 第一次被发送的时间，用于计算rtt
	last        time.Time    // 上一次被发送的时间，超时重发判断
}

type Conn struct {
	rudp            *RUDP
	from            byte            // 0:client，1:server
	token           uint32          // 连接token
	timestamp       uint64          // 连接的时间戳
	state           byte            // 状态
	lock            sync.RWMutex    // 同步锁
	stateSignal     chan byte       // 状态改变的信号
	handleQueue     chan *segment   // 待处理的数据缓存
	timer           *time.Timer     // 计时器
	readTime        time.Time       // 接收队列，接收新的有效数据的时间，用于判断连接超时
	pingSN          uint32          // 发送ping的sn
	listenAddr      [2]*net.UDPAddr // local/remote的监听地址
	internetAddr    [2]*net.UDPAddr // local/remote的互联网地址
	ackSN           [2]uint32       // 发送ack的次数，递增，收到新的max sn时，重置为0。0:local，1:remote
	ioBytes         [2]uint64       // read/write的总字节
	dataBytes       [2]uint64       // read/write的数据的总字节
	dataLock        [2]sync.RWMutex // 接收/发送队列数据锁
	dataTimeout     [2]time.Time    // 应用层设置的io读/写超时
	dataEnable      [2]chan byte    // 接收/发送队列，可读/写通知
	dataLen         [2]uint16       // 接收/发送队列长度
	dataCap         [2]uint16       // 接收/发送队列最大长度
	dataSN          [2]uint16       // 0:接收队列最大连续序号，小于sn的数据块是可读的。1:发送队列下一个数据块的sn
	discardSN       [2]uint16       // 0:接收队列期待的下一个丢弃数据segment的sn。1:发送队列下一个丢弃数据segment的sn
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
}

// net.Conn接口
func (c *Conn) Read(b []byte) (int, error) {
	n := 0
	// 没有设置超时
	c.dataLock[0].RLock()
	if c.dataTimeout[0].IsZero() {
		c.dataLock[0].RUnlock()
		for {
			if c.state != connectState {
				return 0, c.netOpError("read", connClosedError)
			}
			c.dataLock[0].Lock()
			n = c.read(b)
			c.dataLock[0].Unlock()
			if n == 0 {
				select {
				case <-c.dataEnable[0]:
					continue
				case <-c.stateSignal:
					return 0, c.netOpError("read", connClosedError)
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
	duration := c.dataTimeout[0].Sub(time.Now())
	c.dataLock[0].RUnlock()
	if duration <= 0 {
		return 0, c.netOpError("read", new(timeoutError))
	}
	for {
		if c.state != connectState {
			return 0, c.netOpError("read", connClosedError)
		}
		c.dataLock[0].Lock()
		n = c.read(b)
		c.dataLock[0].Unlock()
		if n == 0 {
			select {
			case <-c.dataEnable[0]:
				continue
			case <-c.stateSignal:
				return 0, c.netOpError("read", connClosedError)
			case <-time.After(duration):
				return 0, c.netOpError("read", new(timeoutError))
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
	return c.write(b, false)
}

// net.Conn接口
func (c *Conn) Close() error {
	// 修改状态
	c.lock.Lock()
	if c.state == closedState || c.state == closingState {
		c.lock.Unlock()
		return c.netOpError("close", connClosedError)
	}
	c.state = closingState
	c.lock.Unlock()
	// 写入0数据，表示eof
	c.write(emptyData, false)
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

// 丢弃最早的一段可丢弃的数据
func (c *Conn) DiscardData() {
	n := 0
	p := c.writeDataHead
	// 移除前面一段，可丢弃的数据
	for {
		if !p.discardable {
			break
		}
		n++
		next := p.next
		writeDataPool.Put(p)
		p = next
		c.dataLen[1]--
		if p == nil {
			c.writeDataTail = nil
			break
		}
	}
	c.writeDataHead = p
	// 添加一个通知到发送队列头
	if n > 0 {
		p = writeDataPool.Get().(*writeData)
		p.discardable = false
		p.next = nil
		p.first = time.Time{}
		p.buff[segmentType] = c.from | discardSegment
		binary.BigEndian.PutUint32(p.buff[segmentToken:], c.token)
		p.len = dataSegmentPayload
		c.dataLen[1]++
		if c.writeDataHead == nil {
			c.writeDataHead = p
			binary.BigEndian.PutUint16(p.buff[discardSegmentSN:], c.dataSN[1])
		} else {
			p.dataSN = c.writeDataHead.dataSN
			c.writeDataHead.next = c.writeDataHead
			c.writeDataHead = p
			binary.BigEndian.PutUint16(p.buff[discardSegmentSN:], p.dataSN)
		}
	}
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
		if c.readDataHead.dataSN > c.dataSN[0] {
			break
		}
		// 有数据块，但是没有数据，表示io.EOF
		if len(c.readDataHead.data) == 0 {
			// 前面的数据块都有数据，先返回前面的
			if n > 0 {
				return n
			}
			// 前面没有数据
			c.removeReadDataFront()
			return -1
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

// 移除第一个数据块
func (c *Conn) removeReadDataFront() {
	d := c.readDataHead
	c.readDataHead = c.readDataHead.next
	c.dataLen[0]--
	readDataPool.Put(d)
}

// 尝试添加一个数据块，成功返回true
func (c *Conn) addReadData(dataSN uint16, data []byte) {
	// 是否在接收范围
	if dataSN < c.dataSN[0] || dataSN >= c.dataCap[0]+c.dataSN[0] {
		return
	}
	// 按sn顺序放到队列中
	p := c.readDataHead
	for p != nil {
		// 重复数据
		if dataSN == p.dataSN {
			return
		}
		// 插入
		if dataSN < p.dataSN {
			break
		}
		p = p.next
	}
	// 新的数据块
	d := readDataPool.Get().(*readData)
	d.dataSN = dataSN
	d.idx = 0
	d.len = uint16(copy(d.data[:], data))
	d.next = p
	c.dataLen[0]++
	if p == c.readDataHead {
		c.readDataHead = d
	}
	// 更新有效数据的读时间
	c.readTime = time.Now()
	// 检查连续的sn
	p = c.readDataHead
	for p != nil && p.dataSN == c.dataSN[0] {
		c.dataSN[0]++
		p = p.next
	}
}

// 丢弃数据
func (c *Conn) discardReadData(dataSN uint16) {
	p := c.readDataHead
	for p != nil {
		if p.discardable && dataSN < p.dataSN {
			d := p
			p = p.next
			readDataPool.Put(d)
			continue
		}
		p = p.next
	}
	c.readDataHead = p
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

// 写入可丢弃的数据
func (c *Conn) WriteDiscardable(b []byte) (int, error) {
	return c.write(b, true)
}

// 写入数据到缓存
func (c *Conn) write(b []byte, discardable bool) (int, error) {
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
			n = c.addWriteData(b[m:], discardable)
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
		n = c.addWriteData(b[m:], discardable)
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

// 新的添加到发送队列的writeData
func (c *Conn) newWriteData(discardable bool) *writeData {
	d := writeDataPool.Get().(*writeData)
	d.discardable = discardable
	d.dataSN = c.dataSN[1]
	c.dataSN[1]++
	d.next = nil
	d.first = time.Time{}
	d.buff[segmentType] = c.from | dataSegment
	binary.BigEndian.PutUint32(d.buff[segmentToken:], c.token)
	binary.BigEndian.PutUint16(d.buff[dataSegmentSN:], d.dataSN)
	d.len = dataSegmentPayload
	c.dataLen[1]++
	return d
}

// 写入数据，返回0表示队列满了无法写入
func (c *Conn) addWriteData(data []byte, discardable bool) int {
	// 还能添加多少个数据包
	free := c.dataCap[1] - c.dataLen[1]
	if free <= 0 {
		return 0
	}
	n, m := 0, 0
	if c.writeDataHead == nil {
		// 队列中没有数据，添加到第一
		c.writeDataHead = c.newWriteData(discardable)
		c.writeDataTail = c.writeDataHead
		m = copy(c.writeDataTail.buff[c.writeDataTail.len:c.dataMSS[1]], data)
		c.writeDataTail.len += uint16(m)
		free--
		n += m
		data = data[m:]
	} else {
		// 检查最后一个数据包是否"满数据"
		if c.writeDataTail.len < c.dataMSS[1] && // 有可以写的空间
			c.writeDataTail.first.IsZero() && // 没有被发送
			c.writeDataTail.discardable == discardable { // 一样的数据类型
			m = copy(c.writeDataTail.buff[c.writeDataTail.len:c.dataMSS[1]], data)
			c.writeDataTail.len += uint16(m)
			n += m
			data = data[m:]
		}
	}
	// 新的数据包
	for free > 0 && len(data) > 0 {
		c.writeDataTail.next = c.newWriteData(discardable)
		c.writeDataTail = c.writeDataTail.next
		m = copy(c.writeDataTail.buff[c.writeDataTail.len:c.dataMSS[1]], data)
		c.writeDataTail.len += uint16(m)
		free--
		n += m
		data = data[m:]
	}
	return n
}

// 移除sn前面的所有数据包
func (c *Conn) removeWriteDataBefore(dataSN uint16) bool {
	// 检查sn是否在发送窗口范围
	if c.writeDataHead == nil || dataSN < c.writeDataHead.dataSN || dataSN > c.writeDataTail.dataSN {
		return false
	}
	// 遍历发送队列数据包
	data := c.writeDataHead
	for {
		// 因为是递增有序队列，sn如果小于当前，就没必要继续
		if dataSN < data.dataSN {
			break
		}
		// 计算rto
		if dataSN == data.dataSN {
			c.calculateRTO(time.Now().Sub(data.first))
		}
		next := data.next
		writeDataPool.Put(data)
		data = next
		c.dataLen[1]--
		if data == nil {
			c.writeDataTail = nil
			break
		}
	}
	c.writeDataHead = data
	return true
}

// 移除指定sn数据包，成功返回true
func (c *Conn) removeWriteData(dataSN uint16) bool {
	// 检查sn是否在发送窗口范围
	if c.writeDataHead == nil || dataSN < c.writeDataHead.dataSN || dataSN > c.writeDataTail.dataSN {
		return false
	}
	// 遍历发送队列数据块
	if dataSN == c.writeDataHead.dataSN {
		// 计算rto
		c.calculateRTO(time.Now().Sub(c.writeDataHead.first))
		// 从队列移除
		temp := c.writeDataHead
		c.writeDataHead = c.writeDataHead.next
		if c.writeDataHead == nil {
			c.writeDataTail = nil
		}
		writeDataPool.Put(temp)
		c.dataLen[1]--
		return true
	}
	prev := c.writeDataHead
	p := prev.next
	for p != nil {
		// 因为是递增有序队列，sn如果小于当前，就没必要继续
		if dataSN < p.dataSN {
			return false
		}
		if dataSN == p.dataSN {
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
func (c *Conn) checkRTO(now *time.Time) {
	// 需要发送的数据包个数
	n := c.dataLen[1]
	// 没有数据
	if n < 1 {
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
			p.first = *now
		}
		c.rudp.WriteToConn(p.buff[:p.len], c)
		p.last = *now
		n--
		p = p.next
	}
}
