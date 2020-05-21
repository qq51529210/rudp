package rudp

import (
	"encoding/binary"
	"sync"
	"time"
)

func newReadBuffer(mss uint16, size uint32) *readBuffer {
	p := new(readBuffer)
	p.head = readDataPool.Get().(*readData)
	p.head.next = nil
	p.mss = uint16(minInt(maxInt(int(mss), minMSS), maxMSS))
	p.maxLen = calcMaxLen(size, p.mss)
	p.enable = make(chan int, 1)
	p.maxSN = p.maxLen - 1
	p.time = time.Now()
	return p
}

type readBuffer struct {
	sync.RWMutex           // 锁
	enable       chan int  // 可写通知
	head         *readData // 数据包有序（sn）队列，第一个不参与计算
	len          uint32    // 队列长度
	maxLen       uint32    // 最大长度
	mss          uint16    // 对方的数据包大小，用于计算maxLen
	bytes        uint64    // 读取数据的总字节
	nextSN       uint32    // 数据包最大连续序号，队列中小于这个序号的数据是可读的
	minSN        uint32    // 队列最小序号，窗口控制
	maxSN        uint32    // 队列最大序号，窗口控制
	timeout      time.Time // 应用层设置的超时
	time         time.Time // 上一次读取有效数据的时间
}

func (this *readBuffer) newReadData(sn uint32, buf []byte, next *readData) *readData {
	d := readDataPool.Get().(*readData)
	d.sn = sn
	d.idx = 0
	d.buf = d.buf[:0]
	d.buf = append(d.buf, buf...)
	d.next = next
	this.len++
	return d
}

// 读取连续的数据，返回0表示没有数据，返回-1表示io.EOF
func (this *readBuffer) Read(buf []byte) int {
	// 没有数据
	if this.len < 1 {
		return 0
	}
	n, m := 0, 0
	cur := this.head.next
	for cur != nil && cur.sn < this.nextSN {
		// 有数据包，但是没有数据，表示io.EOF
		if len(cur.buf) == 0 {
			// 前面的数据包都有数据，先返回前面的
			if n > 0 {
				return n
			}
			// 前面没有数据
			this.removeFront()
			return -1
		}
		// 拷贝数据到buf
		m = copy(buf[n:], cur.buf[cur.idx:])
		n += m
		cur.idx += m
		// 数据包数据拷贝完了，从队列中移除
		if cur.idx == len(cur.buf) {
			this.removeFront()
			this.minSN++
			this.maxSN++
		}
		// buf满了
		if n == len(buf) {
			return n
		}
		cur = this.head.next
	}
	return n
}

// 移除第一个数据包
func (this *readBuffer) removeFront() {
	cur := this.head.next
	this.head.next = cur.next
	readDataPool.Put(cur)
	this.len--
}

// 添加数据包
func (this *readBuffer) ReadFromUDP(sn uint32, buf []byte) bool {
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

// 可读通知
func (this *readBuffer) Enable() {
	select {
	case this.enable <- 1:
	default:
	}
}

func (this *readBuffer) Release() {
	close(this.enable)
	p := this.head
	for p != nil {
		n := p.next
		readDataPool.Put(p)
		p = n
	}
}

func newWriteBuffer(mss uint16, size uint32) *writeBuffer {
	p := new(writeBuffer)
	p.head = writeDataPool.Get().(*writeData)
	p.head.next = nil
	p.tail = p.head
	p.mss = uint16(minInt(maxInt(int(mss), minMSS), maxMSS))
	p.maxLen = calcMaxLen(size, p.mss)
	p.maxWrite = p.maxLen
	p.enable = make(chan int, 1)
	p.minRTO = time.Millisecond
	p.maxRTO = time.Millisecond * 100
	return p
}

type writeBuffer struct {
	sync.RWMutex               // 锁
	enable       chan int      // 可写通知
	head         *writeData    // 数据包有序（sn）队列，第一个不参与计算
	tail         *writeData    // 数据包有序（sn）队列，最后一个
	len          uint32        // 队列长度
	maxLen       uint32        // 最大长度
	mss          uint16        // 最大数据包
	bytes        uint64        // 读取数据的总字节
	maxWrite     uint32        // 一次发送的数据包最大数量，窗口控制
	sn           uint32        // 数据包最大连续序号，每添加一个数据，递增
	timeout      time.Time     // 应用层设置的超时
	rto          time.Duration // 超时重发
	rtt          time.Duration // 实时RTT，用于计算rto
	rttVar       time.Duration // 平均RTT，用于计算rto
	minRTO       time.Duration // 最小rto，不要出现"过快"
	maxRTO       time.Duration // 最大rto，不要出现"假死"
	cToken       uint32        // 客户端token
	sToken       uint32        // 服务端token
}

func (this *writeBuffer) newWriteData(msg uint8) {
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

// 写入数据到发送队列，返回0表示队列满了无法写入
func (this *writeBuffer) Write(msg uint8, buf []byte) int {
	// 还能添加多少个数据包
	m := this.maxLen - this.len
	if m <= 0 {
		return 0
	}
	n, i := 0, 0
	// 检查最后一个数据包是否"满数据"
	if this.head != this.tail && this.tail.len < this.mss && this.tail.first.IsZero() {
		i = copy(this.tail.buf[this.tail.len:this.mss], buf)
		this.tail.len += uint16(i)
		n += i
		buf = buf[i:]
	}
	// 新的数据包
	for m > 0 && len(buf) > 0 {
		this.newWriteData(msg)
		i = copy(this.tail.buf[msgPayload:this.mss], buf)
		this.tail.len += uint16(i)
		// 个数减1
		m--
		n += i
		buf = buf[i:]
	}
	return n
}

// 写入0数据，表示eof
func (this *writeBuffer) WriteEOF(msg uint8) {
	this.newWriteData(msg)
}

// 从队列中移除小于sn的数据包
func (this *writeBuffer) RemoveBefore(sn uint32) {
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
func (this *writeBuffer) Remove(sn uint32) {
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
func (this *writeBuffer) WriteToUDP(out func([]byte), now time.Time) {
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

// 可写通知
func (this *writeBuffer) Enable() {
	select {
	case this.enable <- 1:
	default:
	}
}

func (this *writeBuffer) Release() {
	close(this.enable)
	p := this.head
	for p != nil {
		n := p.next
		readDataPool.Put(p)
		p = n
	}
}

// 根据实时的rtt来计算rto，使用的是tcp那套算法
func (this *writeBuffer) calcRTO(rtt time.Duration) {
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
