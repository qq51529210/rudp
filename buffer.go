package rudp

import (
	"encoding/binary"
	"sync"
	"time"
)

var _readDataPool, _writeDataPool sync.Pool

func init() {
	_readDataPool.New = func() interface{} {
		return new(readData)
	}
	_writeDataPool.New = func() interface{} {
		return new(writeData)
	}
}

// 数据块
type readData struct {
	sn   uint32    // 序号
	buf  []byte    // 数据
	idx  int       // 数据下标
	next *readData // 下一个
}

// Conn的读缓存，是一个队列
type readBuffer struct {
	sync.RWMutex           // 锁
	enable       chan int  // 可读标志
	timeout      time.Time // 应用层设置了读数据的时间，用于"读超时"判断
	time         time.Time // 上一次接受到有效数据的时间，用于"连接超时"判断
	head         *readData // 队列第一个，不参与数据计算
	len          uint32    // 队列当前长度
	maxLen       uint32    // 队列最大长度
	maxBytes     uint32    // 队列最大字节
	nextSN       uint32    // 想要的下一个连接数据块的序号，序号前的数据是可读的
	minSN        uint32    // 窗口最小序号
	maxSN        uint32    // 窗口最大序号
	mss          uint16    // 对方的数据块大小，用于确定maxLen，在建立连接时确定
	ackId        uint64    // 对于同一个nextSN的ack，递增的id
}

// 设置数据块队列的最大长度
func calcBufferMaxLen(bytes uint32, mss uint16) uint32 {
	mss = uint16(minInt(maxInt(int(mss), minMSS), maxMSS))
	// 计算数据块队列的长度
	if bytes < uint32(mss) {
		return 1
	}
	// 长度=字节/mss
	max_len := bytes / uint32(mss)
	// 不能整除，+1
	if bytes%uint32(mss) != 0 {
		max_len++
	}
	return max_len
}

// 获取一个数据块
func (this *readBuffer) getData(sn uint32, data []byte) *readData {
	// 取头部
	p := _readDataPool.Get().(*readData)
	p.idx = 0
	p.sn = sn
	p.buf = p.buf[:0]
	p.buf = append(p.buf, data...)
	// 长度+1
	this.len++
	// 更新超时时间
	this.time = time.Now()
	return p
}

// 添加数据
func (this *readBuffer) AddData(sn uint32, data []byte) {
	// sn在接收缓存范围内
	if sn < this.nextSN || sn >= this.maxSN {
		return
	}
	prev := this.head
	p := prev.next
	for p != nil {
		// 重复的数据
		if p.sn == sn {
			return
		}
		// 新的数据
		if sn < p.sn {
			d := this.getData(sn, data)
			d.next = p
			prev.next = d
			return
		}
		prev = p
		p = prev.next
	}
	prev.next = this.getData(sn, data)
	// 如果正好是下一个sn
	if sn == this.nextSN {
		this.nextSN++
		this.ackId = 0
	}
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
	enable       chan int      // 可写通知
	head         *writeData    // 等待确认的消息发送队列
	tail         *writeData    // 等待确认的消息发送队列
	len          uint32        // 数据队列当前长度
	maxLen       uint32        // 数据队列最大长度
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

func (this *writeBuffer) Remove(msg *udpData) {
	// 检查发送队列
	this.Lock()
	if this.head == nil {
		this.Unlock()
		return
	}
	sn := binary.BigEndian.Uint32(msg.b[msgAckSN:])
	max_sn := binary.BigEndian.Uint32(msg.b[msgAckMaxSN:])
	p := this.head
	ok := false
	if max_sn >= sn {
		// 从发送队列移除小于max_sn的数据块
		for p != nil {
			if p.sn > sn {
				break
			}
			if p.sn == sn {
				// 计算RTO
				this.CalcRTO(p)
			}
			// 移除
			n := p.next
			this.putData(p)
			p = n
		}
		this.head = p
		this.UpdateAck(max_sn, msg)
		ok = true
	} else {
		// 从发送队列移除指定sn的数据块
		if p.sn == sn {
			// 链表头
			this.head = p.next
			this.UpdateAck(sn, msg)
			this.putData(p)
			ok = true
		} else {
			next := p.next
			for next != nil {
				if next.sn > sn {
					break
				}
				if next.sn == sn {
					// 计算RTO
					this.CalcRTO(next)
					// 移除
					p.next = next.next
					this.putData(next)
					this.UpdateAck(sn, msg)
					ok = true
					break
				}
				next = p.next
			}
		}
	}
	this.Unlock()
	// 是否可写入新数据
	if ok {
		this.Signal()
	}
}
