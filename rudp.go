package rudp

import (
	"math/rand"
	"net"
	"runtime"
	"sync"
	"time"
)

const (
	defaultMessageQueue           = 1024                  // 默认等待处理的消息缓存队列
	defaultCheckAcceptingDuration = time.Millisecond * 10 // 默认检查间隔
	defaultReadBufferLength       = 1024 * 4              // 默认读缓存，4k
	defaultWriteBufferLength      = 1024 * 4              // 默认写缓存，4k
)

// 随机数
var _rand = rand.New(rand.NewSource(time.Now().Unix()))

// 为了避免分包，选择最合适的mss
var DetectMSS = func(*net.UDPAddr) uint16 {
	return minMSS
}

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
	p.waitQuit.Add(1)
	go p.readUDPRoutine()
	// 启动处理数据协程
	for i := 0; i < runtime.NumCPU(); i++ {
		p.waitQuit.Add(1)
		go p.handleUDPRoutine()
	}
	return p, nil
}

type RUDP struct {
	sync.Mutex
	*server
	*client
	rwBytes                    // io字节
	conn        *net.UDPConn   // 底层socket
	closeSignal chan struct{}  // 通知所有协程退出的信号
	waitQuit    sync.WaitGroup // 等待所有协程退出
	message     chan *message  // 等待处理的原始的udp数据队列
	rBufLen     int            // 新的Conn的读缓存大小
	wBufLen     int            // 新的Conn的读缓存大小
}

// 初始化成员变量
func (this *RUDP) init(conn *net.UDPConn) {
	this.conn = conn
	this.message = make(chan *message, defaultMessageQueue)
	this.rBufLen = defaultReadBufferLength
	this.wBufLen = defaultWriteBufferLength
	this.server = newServer()
	this.client = newClient()
}

// 创建一个新的Conn变量
func (this *RUDP) newConn(state connState, rAddr *net.UDPAddr) *Conn {
	conn := new(Conn)
	conn.connState = state
	conn.rAddr = rAddr
	conn.lAddr = this.conn.LocalAddr().(*net.UDPAddr)
	conn.rBuf.length = this.rBufLen
	conn.wBuf.length = this.wBufLen
	conn.connectSignal = make(chan struct{})
	return conn
}

// 使用net.UDPConn向指定地址发送指定数据
func (this *RUDP) WriteTo(b []byte, addr *net.UDPAddr) (int, error) {
	return this.conn.WriteToUDP(b, addr)
}

// 返回net.UDPConn的本地地址
func (this *RUDP) LocalAddr() net.Addr {
	return this.conn.LocalAddr()
}

// 关闭RUDP，该RUDP对象，不可以再使用，net.Listener接口
func (this *RUDP) Close() error {
	// 关闭底层Conn
	err := this.closeNetConn()
	if err != nil {
		return err
	}
	// 通知所有协程退出
	close(this.closeSignal)
	// 等待所有协程退出
	this.waitQuit.Wait()
	// 清理资源
	this.closeClient()
	this.closeServer()
	return nil
}

// 关闭底层net.UDPConn
func (this *RUDP) closeNetConn() error {
	this.Lock()
	if this.conn == nil {
		this.Unlock()
		return this.opError("close", nil, errClosed)
	}
	this.conn.Close()
	this.conn = nil
	this.Unlock()
	return nil
}

// 读udp数据的协程
func (this *RUDP) readUDPRoutine() {
	defer this.waitQuit.Done()
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
		this.rwBytes.r += uint64(msg.n)
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
	defer this.waitQuit.Done()
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
				_msgPool.Put(msg)
			case msgAccept: // s->c，接受连接，握手2
				this.handleMsgAccept(msg)
				_msgPool.Put(msg)
			case msgRefuse: // s->c，拒绝连接，握手2
				this.handleMsgRefuse(msg)
				_msgPool.Put(msg)
			case msgConnect: // c->s，收到接受连接的消息，握手3
				this.handleMsgConnect(msg)
				_msgPool.Put(msg)
			case msgDataC: // c->s，数据
				this.handleMsgDataC(msg)
			case msgDataS: // s->c，数据
				this.handleMsgDataS(msg)
			case msgAckC: // c->s，收到数据确认
				this.handleMsgAckC(msg)
				_msgPool.Put(msg)
			case msgAckS: // s->c，收到数据确认
				this.handleMsgAckS(msg)
				_msgPool.Put(msg)
			case msgCloseC: // c->s，关闭
				this.handleMsgCloseC(msg)
				_msgPool.Put(msg)
			case msgCloseS: // s->c，关闭
				this.handleMsgCloseS(msg)
				_msgPool.Put(msg)
			case msgPing: // c->s
				this.handleMsgPing(msg)
				_msgPool.Put(msg)
			case msgPong: // s->c
				this.handleMsgPong(msg)
				_msgPool.Put(msg)
			default: // 其他消息，不处理
				_msgPool.Put(msg)
			}
		case <-this.closeSignal: // 关闭信号
			return
		}
	}
}

// 返回net.OpError对象
func (this *RUDP) opError(op string, rAddr net.Addr, err error) error {
	return &net.OpError{
		Op:     op,
		Net:    "udp",
		Source: rAddr,
		Addr:   this.conn.LocalAddr(),
		Err:    err,
	}
}
