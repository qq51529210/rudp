package rudp

import (
	"encoding/binary"
	"sync"
	"time"
)

func newReadBuffer() *readBuffer {
	p := new(readBuffer)
	p.cond = sync.NewCond(&p.RWMutex)
	p.enable = make(chan int)
	return p
}

func newWriteBuffer(mss uint16) *writeBuffer {
	p := new(writeBuffer)
	p.mss = mss
	p.cond = sync.NewCond(&p.RWMutex)
	p.enable = make(chan int)
	return p
}

// 数据块
type readData struct {
	sn   uint32    // 序号
	buf  []byte    // 数据
	idx  int       // 数据下标
	next *readData // 下一个
}

// Conn的读缓存
type readBuffer struct {
	sync.RWMutex            // 锁
	cond         *sync.Cond // 可写通知
	enable       chan int   // 可读标志
	pool         *readData  // 数据块缓存池
	data         *readData  // 数据队列
	tail         *readData  // 数据块链表队列最后一个
	len          uint32     // 队列当前容量
	cap          uint32     // 队列最大容量
	nextSN       uint32     // 想要的下一个连接数据块的序号，序号前的数据是可读的
	minSN        uint32     // 当前接受队列最小序号，用于接收到新数据判断
	maxSN        uint32     // 当前接受队列最大序号，用于接收到新数据判断
	ackId        uint64     // ack的id，递增，对方用于判断ack消息的buffer最新大小
	timeout      time.Time  // 应用层设置了读超时
	time         time.Time  // 上一次接受到有效数据的时间（无效的，比如，重复的数据）
}

// 队列剩余的长度
func (this *readBuffer) Left() uint32 {
	return this.cap - this.len
}

// 获取一个数据块
func (this *readBuffer) GetData(sn uint32, data []byte) *readData {
	// 取头部
	p := this.pool
	if p == nil {
		p = new(readData)
	} else {
		this.pool = p.next
		p.next = nil
	}
	p.idx = 0
	p.sn = sn
	p.buf = p.buf[:0]
	p.buf = append(p.buf, data...)
	return p
}

// 缓存数据块
func (this *readBuffer) PutData(p *readData) {
	// 放到头部
	p.next = this.pool
	this.pool = p
}

// 读取数据
func (this *readBuffer) Read(b []byte) (n int) {
	p := this.data
	i := 0
	// 队列中，小于sn的都是可读的
	for p != nil && p.sn < this.nextSN {
		// 拷贝
		i = copy(b[n:], p.buf[p.idx:])
		n += i
		p.idx += i
		// 数据块读完了，移除，回收
		if len(p.buf) == p.idx {
			next := p.next
			this.PutData(p)
			p = next
			this.len--
		}
		// 缓存读满了
		if n == len(b) {
			return
		}
	}
	return
}

// 添加数据
func (this *readBuffer) Add(sn uint32, buf []byte) {
	// 第一个数据块
	if this.data == nil {
		this.data = this.GetData(sn, buf)
		this.len++
		this.time = time.Now()
		return
	}
	p := this.data
	if p.sn == sn {
		return
	}
	if sn < p.sn {
		d := this.GetData(sn, buf)
		d.next = p
		this.data = d
		this.len++
		this.time = time.Now()
		return
	}
	prev := p
	p = prev.next
	for p != nil {
		if p.sn == sn {
			return
		}
		if sn < p.sn {
			d := this.GetData(sn, buf)
			d.next = p
			this.len++
			this.time = time.Now()
			return
		}
		prev = p
		p = p.next
	}
	prev.next = this.GetData(sn, buf)
	this.len++
	this.time = time.Now()
}

// 写缓存队列node
type writeData struct {
	sn    uint32     // 序号
	data  udpData    // 数据
	next  *writeData // 下一个
	first time.Time  // 第一次被发送的时间，用于计算rtt
	last  time.Time  // 上一次被发送的时间，超时重发判断
}

// Conn的写缓存
type writeBuffer struct {
	sync.RWMutex               // 锁
	cond         *sync.Cond    // 可写通知
	enable       chan int      // 可写通知
	pool         *writeData    // 可用的数据块列表
	data         *writeData    // 等待确认的消息发送队列
	tail         *writeData    // 等待确认的消息发送队列
	len          uint32        // 数据队列当前长度
	cap          uint32        // 数据队列最大长度
	mss          uint16        // 每一个消息大小，包括消息头
	sn           uint32        // 当前数据块的最大序号
	ack          uint32        // 对方ack消息的sn最大序号
	ackId        uint64        // 上一个ack的id，用于判断对方的buffer最新大小
	canWrite     uint32        // 对方接受缓存大小
	timeout      time.Time     // 应用层设置了写超时
	rto          time.Duration // 超时重发
	rtt          time.Duration // 实时RTT，用于数据补发判断
	rttVar       time.Duration // 平均RTT，用于计算RTO
	minRTO       time.Duration // 最小rto，不要发送"太快"
	maxRTO       time.Duration // 最大rto，不要出现"假死"
}

// 发送队列可用的缓存
func (this *writeBuffer) Left() uint32 {
	return this.cap - this.len
}

// 更新ack和对方的free缓存
func (this *writeBuffer) UpdateAck(sn uint32, msg *udpData) {
	if sn > this.ack {
		this.ack = sn
		this.canWrite = binary.BigEndian.Uint32(msg.b[msgAckBuffer:])
		this.ackId = binary.BigEndian.Uint64(msg.b[msgAckId:])
	} else {
		ack_id := binary.BigEndian.Uint64(msg.b[msgAckId:])
		if ack_id > this.ackId {
			this.ackId = ack_id
			this.canWrite = binary.BigEndian.Uint32(msg.b[msgAckBuffer:])
		}
	}
}

// 根据实时的rtt来计算rto，使用的是tcp那套算法
func (this *writeBuffer) CalcRTO(data *writeData) {
	rtt := time.Now().Sub(data.first)
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

// 获取一个数据块
func (this *writeBuffer) GetData(msg byte, token uint32, buf []byte) *writeData {
	p := this.pool
	if p == nil {
		p = new(writeData)
	} else {
		this.pool = p.next
		p.next = nil
	}
	p.sn = this.sn
	p.first = time.Time{}
	p.last = time.Time{}
	// 直接编码data消息
	p.data.b[msgType] = msg
	binary.BigEndian.PutUint32(p.data.b[msgDataToken:], token)
	binary.BigEndian.PutUint32(p.data.b[msgDataSN:], p.sn)
	p.data.n = copy(p.data.b[msgDataPayload:this.mss], buf)
	//
	this.sn++
	this.len++
	return p
}

// 缓存数据块
func (this *writeBuffer) PutData(p *writeData) {
	if this.pool != nil {
		p.next = this.pool
	}
	this.pool = p
}

// 写数据
func (this *writeBuffer) Write(msg byte, token uint32, buf []byte) (n int) {
	// 还能添加多少数据块
	m := this.Left()
	if m < 1 {
		return
	}
	// 没有数据
	if this.data == nil {
		d := this.GetData(msg, token, buf)
		// 头
		this.data = d
		// 尾=头
		this.tail = this.data
		m--
		n += d.data.n
		buf = buf[d.data.n:]
	}
	// 循环
	for m > 0 && len(buf) > 0 {
		d := this.GetData(msg, token, buf)
		// 添加到尾部
		this.tail.next = this.GetData(msg, token, buf)
		this.tail = this.tail.next
		m--
		n += d.data.n
		buf = buf[d.data.n:]
	}
	return
}
