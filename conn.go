package rudp

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"sync"
	"time"
)

const (
	closedState     = iota // 关闭的连接
	connectingState        // 正在发起连接
	connectedState         // 已经确认连接
	closingState           // 正在关闭的连接
)

var (
	diffieHellmanP = big.NewInt(0)
	diffieHellmanG = big.NewInt(0)
	readDataPool   sync.Pool
	writeDataPool  sync.Pool
	emptyData      []byte
)

func init() {
	readDataPool.New = func() interface{} { return new(readData) }
	writeDataPool.New = func() interface{} { return new(writeData) }
	diffieHellmanP.SetBytes([]byte("RUDP DIFFIE-HELLMAN KEY EXCHANGE"))
	diffieHellmanG.SetBytes([]byte("rudp diffie-hellman key exchange"))
}

// 判断两个udp地址是否相同
func isAddressEqual(a1, a2 *net.UDPAddr) bool {
	return bytes.Compare(a1.IP.To16(), a2.IP.To16()) == 0 && a1.Port == a2.Port
}

// 计算队列最大长度
func calcQueueCap(bytes, mss int) int {
	// 计算数据块队列的长度
	if bytes < mss {
		return 1
	}
	// 长度=字节/mss
	max := bytes / mss
	// 不能整除，+1
	if bytes%mss != 0 {
		max++
	}
	return max
}

// Conn接收队列数据包
type readData struct {
	sn   uint32    // 序号
	data []byte    // 数据
	idx  uint16    // 有效数据的起始，因为read有可能一次读不完数据
	next *readData // 下一个
}

// Conn发送队列数据包
type writeData struct {
	sn    uint32     // 序号
	data  []byte     // 数据
	len   uint16     // 数据大小，因为write有可能写不完数据块
	next  *writeData // 下一个
	first int64      // 第一次被发送的时间，用于计算rtt
	last  int64      // 上一次被发送的时间，超时重发判断
}

type Conn struct {
	segmentType        byte          // 是client还是server
	state              byte          // 状态，closeState/connectingState/connectedState
	lock               sync.RWMutex  // 同步锁
	quit               chan struct{} // 关闭的信号
	localEnableFEC     bool          // local是否开启纠错
	remoteEnableFEC    bool          // remote是否开启纠错
	localEnableCrypto  bool          // local是否开启加密
	remoteEnableCrypto bool          // remote是否开启加密
	localListenAddr    *net.UDPAddr  // local监听地址
	remoteListenAddr   *net.UDPAddr  // remote监听地址
	localInternetAddr  *net.UDPAddr  // local互联网地址
	remoteInternetAddr *net.UDPAddr  // remote互联网地址
	localAckSN         uint32        // 发送ack的次数，递增，收到新的max sn时，重置为0
	remoteAckSN        uint32        // remoteack的次数，用于判断更新remote的接收队列空闲长度
	readBytes          uint64        // read的总字节
	writeBytes         uint64        // write的总字节
	readTime           int64         // 上一次读取有效数据的时间戳
	readLock           sync.RWMutex  // 读缓存锁
	readable           chan int      // 可读通知
	readTimeout        time.Time     // 应用层设置的io读超时
	readQueue          *readData     // 接收队列，第一个数据包，仅仅作为指针
	readQueueLen       int           // 接收队列长度
	readQueueCap       int           // 接收队列最大长度
	readNextSN         uint16        // 接收队列接收到的连续的数据包最大序号
	readMinSN          uint16        // 接收队列最小序号，接收窗口控制
	readMaxSN          uint16        // 接收队列最大序号，接收窗口控制
	readMSS            uint16        // 接收队列的每个数据包的大小（remote的发送队列数据包大小）
	writeLock          sync.RWMutex  // 写缓存锁
	writeTimeout       time.Time     // 应用层设置的io写超时
	writeable          chan int      // 可写通知
	writeQueueHead     *writeData    // 发送队列，第一个数据包，仅仅作为指针
	writeQueueTail     *writeData    // 发送队列，最后一个数据包
	writeQueueLen      int           // 发送队列长度
	writeQueueCap      int           // 发送队列最大长度
	writeSN            uint32        // 发送队列的最大sn
	writeMSS           uint16        // 发送队列的每个数据包的大小
	writeMax           uint32        // remote读缓存队列剩余长度，发送窗口控制
	rto                time.Duration // 超时重发
	rtt                time.Duration // 实时RTT，用于计算rto
	rttVar             time.Duration // 平均RTT，用于计算rto
	minRTO             time.Duration // 最小rto，不要出现"过快"
	maxRTO             time.Duration // 最大rto，不要出现"假死"
	cToken             uint32        // 客户端token
	sToken             uint32        // 服务端token
	pingId             uint32        // ping消息的id，递增
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

// net.Conn接口
func (c *Conn) Read(b []byte) (int, error) {
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
	if duration <= 0 {
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
	if duration <= 0 {
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
	if c.state == closedState || c.state == closingState {
		c.lock.Unlock()
		return c.netOpError("close", closeErr("conn"))
	}
	c.state = closingState
	c.lock.Unlock()
	// 写入0数据，表示eof
	c.write(emptyData)
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

// 设置读缓存大小（字节），传输的速度由双方的rw最小缓存决定
func (c *Conn) SetReadBuffer(n int) error {
	if n < 0 {
		n = defaultReadBuffer
	}
	c.readLock.Lock()
	c.readQueueCap = calcQueueCap(n, int(c.readMSS))
	c.readLock.Unlock()
	return nil
}

// 设置写缓存大小（字节），传输的速度由双方的rw最小缓存决定
func (c *Conn) SetWriteBuffer(n int) error {
	if n < 0 {
		n = defaultWriteBuffer
	}
	c.writeLock.Lock()
	c.writeQueueCap = calcQueueCap(n, int(c.writeMSS))
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
	return isAddressEqual(c.localInternetAddr, c.localListenAddr)
}

// remote地址是否在nat(Network Address Translation)
func (c *Conn) IsRemoteNat() bool {
	return isAddressEqual(c.remoteInternetAddr, c.remoteListenAddr)
}

// // 返回一个初始化的readData
// func (c *Conn) newReadData(sn uint32, b []byte, next *readData) *readData {
// 	d := readDataPool.Get().(*readData)
// 	d.sn = sn
// 	d.idx = 0
// 	d.b = d.b[:0]
// 	d.b = append(d.b, b...)
// 	d.next = next
// 	c.readQueueLen++
// 	return d
// }

// 在发送队列末尾，添加一个初始化的writeData
func (c *Conn) newWriteData() *writeData {
	d := writeDataPool.Get().(*writeData)
	d.sn = c.writeSN
	d.next = nil
	d.first = 0
	d.data = d.data[:1]
	d.data[0] = c.segmentType | dataSegment
	binary.BigEndian.PutUint32(d.data[dataSegmentToken:], c.sToken)
	binary.BigEndian.PutUint32(d.data[dataSegmentSN:], d.sn)
	d.len = dataSegmentPayload
	c.writeSN++
	c.writeQueueLen++
	return d
}

func (c *Conn) freeReadData() {
	d := c.readQueue
	c.readQueue = c.readQueue.next
	c.readQueueLen--
	readDataPool.Put(d)
}

// 读取连续的数据，返回0表示没有数据，返回-1表示io.EOF
func (c *Conn) read(b []byte) int {
	// 没有数据
	if c.readQueueLen < 1 {
		return 0
	}
	n, m := 0, 0
	for c.readQueue != nil {
		// 有数据包，但是没有数据，表示io.EOF
		if len(c.readQueue.data) == 0 {
			// 前面的数据包都有数据，先返回前面的
			if n > 0 {
				return n
			}
			// 前面没有数据
			c.freeReadData()
			c.readQueue = nil
			return -1
		}
		// 拷贝数据到buf
		m = copy(b[n:], c.readQueue.data[c.readQueue.idx:])
		n += m
		c.readQueue.idx += uint16(m)
		// 数据包数据拷贝完了，从队列中移除
		if c.readQueue.idx == uint16(len(c.readQueue.data)) {
			c.freeReadData()
			c.readMinSN++
			c.readMaxSN++
		}
		// buf满了
		if n == len(b) {
			return n
		}
	}
	return n
}

// 写入数据到发送队列，返回0表示队列满了无法写入
func (c *Conn) write(b []byte) int {
	// 还能添加多少个数据包
	free := c.writeQueueCap - c.writeQueueLen
	if free <= 0 {
		return 0
	}
	// 加密
	if c.localCrypto {

	}
	// 纠错
	if c.localFEC {

	}
	n, m := 0, 0
	if c.writeQueueHead == nil {
		c.writeQueueHead = c.newWriteData()
		c.writeQueueTail = c.writeQueueHead
		m = copy(c.writeQueueTail.data[c.writeQueueTail.len:c.writeMSS], b)
		c.writeQueueTail.len += uint16(m)
		free--
		n += m
		b = b[m:]
	} else {
		// 检查最后一个数据包是否"满数据"
		if c.writeQueueTail.len < c.writeMSS && // 有可以写的空间
			c.writeQueueTail.first == 0 { // 没有被发送
			m = copy(c.writeQueueTail.data[c.writeQueueTail.len:c.writeMSS], b)
			c.writeQueueTail.len += uint16(m)
			n += m
			b = b[m:]
		}
	}
	// 新的数据包
	for free > 0 && len(b) > 0 {
		c.writeQueueTail.next = c.newWriteData()
		c.writeQueueTail = c.writeQueueTail.next
		m = copy(c.writeQueueTail.data[c.writeQueueTail.len:c.writeMSS], b)
		c.writeQueueTail.len += uint16(m)
		free--
		n += m
		b = b[m:]
	}
	return n
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
	// 如果remote没有接收空间，发1个
	if n == 0 {
		n = 1
	}
	// 开始遍历发送队列
	prev := c.writeQueueHead
	cur := prev.next
	for cur != nil && n > 0 {
		// 第一次发送
		if cur.first.IsZero() {
			out(cur.b[:cur.len])
			// 记录时间
			cur.first = now
			cur.last = now
			n--
		} else if now.Sub(cur.last) >= cur.rto {
			out(cur.b[:cur.len])
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
func (c *Conn) readFromUDP(sn uint16, b []byte) bool {
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
	prev.next = c.newReadData(sn, b, cur)
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

// t: connectSegmentType
func (c *Conn) encConnectSegment(b []byte, t byte) {
	b[segmentHeader] = cmdSegment | c.segmentType
	b[cmdSegmentType] = t
	b[connectSegmentVersion] = protolVersion
	binary.BigEndian.PutUint32(b[connectSegmentClientToken:], c.cToken)
	binary.BigEndian.PutUint32(b[connectSegmentServerToken:], c.sToken)
	copy(b[connectSegmentLocalIP:], c.localListenAddr.IP.To16())
	binary.BigEndian.PutUint16(b[connectSegmentLocalPort:], uint16(c.localListenAddr.Port))
	copy(b[connectSegmentRemoteIP:], c.remoteInternetAddr.IP.To16())
	binary.BigEndian.PutUint16(b[connectSegmentRemotePort:], uint16(c.remoteInternetAddr.Port))
	if c.localEnableFEC {
		b[connectSegmentFEC] = 1
	} else {
		b[connectSegmentFEC] = 0
	}
	if c.localEnableCrypto {
		b[connectSegmentCrypto] = 1
		// copy(b[connectSegmentExchangeKey:], c.exchangeKey[:])
	} else {
		b[connectSegmentCrypto] = 0
	}
}

// // return: connectSegmentType
// func (c *Conn) decConnectSegment(b []byte, size int) byte {
// 	c.remoteListenAddr.IP = append(c.remoteListenAddr.IP, b[msgDialLocalIP:msgDialLocalPort]...)
// 	c.remoteListenAddr.Port = int(binary.BigEndian.Uint16(b[msgDialLocalPort:]))
// 	c.localInternetAddr.IP = append(c.localInternetAddr.IP, b[msgDialRemoteIP:msgDialRemotePort]...)
// 	c.localInternetAddr.Port = int(binary.BigEndian.Uint16(b[msgDialRemotePort:]))
// 	c.readMSS = binary.BigEndian.Uint16(b[msgDialMSS:])
// 	c.readQueueCap = calcQueueCap(size, int(c.readMSS))
// 	c.readMaxSN = uint16(c.readQueueCap)
// 	return b[connectSegmentType]
// }

// sn: 收到的新的数据sn
func (c *Conn) encAckSegment(b []byte, sn uint16) {
	b[0] = ackSegment | c.segmentType
	binary.BigEndian.PutUint32(b[ackSegmentToken:], c.sToken)
	binary.BigEndian.PutUint16(b[ackSegmentDataSN:], sn)
	binary.BigEndian.PutUint16(b[ackSegmentDataMaxSN:], c.readNextSN-1)
	binary.BigEndian.PutUint16(b[ackSegmentDataFree:], uint16(c.readQueueCap-c.readQueueLen))
	binary.BigEndian.PutUint32(b[ackSegmentSN:], c.localAckSN)
	c.localAckSN++
}
