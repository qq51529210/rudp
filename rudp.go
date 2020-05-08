package rudp

import (
	"math/rand"
	"net"
	"runtime"
	"sync"
	"time"
)

const (
	defaultMessageQueue = 1024
)

// 随机数
var _rand = rand.New(rand.NewSource(time.Now().Unix()))

func New(address string) (*RUDP, error) {
	addr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP(addr.Network(), addr)
	if err != nil {
		return nil, err
	}
	p := new(RUDP)
	p.init(conn)
	// 启动读数据协程
	p.waitExit.Add(1)
	go p.readUDPRoutine()
	// 启动处理数据协程
	for i := 0; i < runtime.NumCPU(); i++ {
		p.waitExit.Add(1)
		go p.handleUDPRoutine()
	}
	return p, nil
}

type RUDP struct {
	conn        *net.UDPConn
	closeSignal chan struct{}  // 通知所有协程退出的信号
	waitExit    sync.WaitGroup // 等待所有协程退出
	message     chan *message  // 等待处理的原始的udp数据队列
	*server
}

func (this *RUDP) init(conn *net.UDPConn) {
	this.conn = conn
	this.message = make(chan *message, defaultMessageQueue)
	this.server = newServer()
}

// 读udp数据的协程
func (this *RUDP) readUDPRoutine() {
	defer this.waitExit.Done()
	var err error
	for {
		msg := _msgPool.Get().(*message)
		// 读取
		msg.n, msg.a, err = this.conn.ReadFromUDP(msg.b[:])
		if err != nil {
			// 出错，关闭
			this.Close()
			return
		}
		// 处理
		select {
		case this.message <- msg: // 添加到等待处理队列
		case <-this.closeSignal: // 关闭信号
			return
		default: // 等待处理队列已满，添加不进，丢弃
			_msgPool.Put(msg)
		}
	}
}

// 处理udp数据的协程
func (this *RUDP) handleUDPRoutine() {
	defer this.waitExit.Done()
	for {
		select {
		case msg, ok := <-this.message:
			if !ok {
				// 被关闭了
				return
			}
			switch msg.Type() {
			case msgDial: // c->s，请求创建连接，握手1
				this.handleMsgDial(msg)
			case msgAccept: // s->c，接受连接，握手2
				this.handleMsgAccept(msg)
			case msgRefuse: // s->c，拒绝连接，握手2
				this.handleMsgRefuse(msg)
			case msgConnect: // c->s，收到接受连接的消息，握手3
				this.handleMsgConnect(msg)
			case msgData: // p<->p，数据
				this.handleMsgData(msg)
			case msgAck: // p<->p，收到数据确认
				this.handleMsgAck(msg)
			case msgPing: // c->s
				this.handleMsgPing(msg)
			case msgPong: // s->c
				this.handleMsgPong(msg)
			default: // 其他消息，不处理
				_msgPool.Put(msg)
			}
		case <-this.closeSignal: // 关闭信号
			return
		}
	}
}
