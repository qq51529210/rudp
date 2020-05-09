package rudp

import (
	"sync"
	"time"
)

var _dataPool sync.Pool

func init() {
	_dataPool.New = func() interface{} {
		return new(data)
	}
}

type data struct {
	sn   uint32
	buf  []byte
	next *data
	prev *data
}

type buffer struct {
	first *data // 数据
	len   int   // 长度
	max   int   // 最大长度
}

func (this *buffer) Free() int {
	return this.max - this.len
}

type readBuffer struct {
	sync.RWMutex               // 锁
	data         buffer        // 数据缓存
	sn           uint32        // 接受到的连续的msgData消息序号，队列中，序号前都是连续的可读的
	minSN        uint32        // 当前接受队列最小序号，用于计算
	maxSN        uint32        // 当前接受队列最大序号，用于计算
	timeout      time.Time     // 应用层设置了读超时
	enable       chan struct{} // 可读标志
	time         time.Time     // 上一次接受到有效数据的时间（无效的，比如，重复的数据）
}

type writeBuffer struct {
	sync.RWMutex               // 锁
	data         buffer        // 等待确认的消息发送队列
	sn           uint32        // 消息的最大序号
	timeout      time.Time     // 应用层设置了写超时
	enable       chan struct{} // 可写标志
	time         time.Time     // 上一次发送数据的时间
}
