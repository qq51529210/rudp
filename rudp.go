package rudp

import (
	"github.com/qq51529210/log"
	"io"
	"math/rand"
	"net"
	"os"
	"runtime"
	"sync"
	"time"
)

// 随机数
var _rand = rand.New(rand.NewSource(time.Now().Unix()))

func defaultInt(n1, n2 int) int {
	if n1 < 1 {
		return n2
	}
	return n1
}

func maxInt(n1, n2 int) int {
	if n1 > n2 {
		return n1
	}
	return n2
}

func minInt(n1, n2 int) int {
	if n1 > n2 {
		return n2
	}
	return n1
}

// 为了避免分包，选择最合适的mss
var DetectMSS = func(*net.UDPAddr) uint16 {
	return minMSS
}

// 使用默认值创建一个新的RUDP
func New(address string) (*RUDP, error) {
	var cfg Config
	cfg.Listen = address
	cfg.AcceptQueue = 128
	return NewWithConfig(&cfg)
}

// 使用配置值创建一个新的RUDP
func NewWithConfig(cfg *Config) (*RUDP, error) {
	addr, err := net.ResolveUDPAddr("udp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP(addr.Network(), addr)
	if err != nil {
		return nil, err
	}
	p := new(RUDP)
	p.logger = os.Stdout
	p.conn = conn
	p.closeSignal = make(chan struct{})
	p.udpDataQueue = make(chan *udpData, defaultInt(cfg.UDPDataQueue, 1024))
	p.connReadBuffer = defaultInt(cfg.ConnReadBuffer, 1024*4)
	p.connWriteBuffer = defaultInt(cfg.ConnWriteBuffer, 1024*4)
	p.server = newServer(cfg)
	p.client = newClient(cfg)
	// 启动读数据和处理数据协程
	go p.readUDPDataRoutine()
	for i := 0; i < defaultInt(cfg.UDPDataRoutine, runtime.NumCPU()); i++ {
		go p.handleUDPDataRoutine()
	}
	return p, nil
}

// io字节
type rwBytes struct {
	r uint64
	w uint64
}

// 表示一个udp的原始数据包
type udpData struct {
	b [msgBuff]byte // 数据缓存
	n int           // 数据大小
	a *net.UDPAddr  // 对方的地址
}

// rudp引擎
type RUDP struct {
	conn            *net.UDPConn   // 底层socket
	lock            sync.Mutex     // 同步锁
	wait            sync.WaitGroup // 等待所有协程退出
	server          *server        // 作为server端的数据
	client          *client        // 作为client端的数据
	dataBytes       rwBytes        // 有效传输的字节
	totalBytes      rwBytes        // io总字节
	closeSignal     chan struct{}  // 通知所有协程退出的信号
	udpDataQueue    chan *udpData  // 等待处理的原始的udp数据队列
	connReadBuffer  int            // 新的Conn的读缓存大小
	connWriteBuffer int            // 新的Conn的读缓存大小
}

// 使用net.UDPConn向指定地址发送指定数据
func (this *RUDP) WriteTo(b []byte, addr *net.UDPAddr) (int, error) {
	n, err := this.conn.WriteToUDP(b, addr)
	this.totalBytes.w += uint64(n)
	return n, err
}

// 返回net.UDPConn的本地地址
func (this *RUDP) LocalAddr() net.Addr {
	return this.conn.LocalAddr()
}

// 关闭RUDP，该RUDP对象，不可以再使用，net.Listener接口
func (this *RUDP) Close() error {
	// 关闭底层Conn
	this.Lock()
	if this.conn == nil {
		this.Unlock()
		return this.opError("close", nil, errClosed("rudp"))
	}
	this.conn.Close()
	this.conn = nil
	this.Unlock()
	// 通知所有协程退出
	close(this.closeSignal)
	// 等待所有协程退出
	this.wait.Wait()
	// 清理资源
	close(this.udpDataQueue)
	this.closeClient()
	this.closeServer()
	return nil
}

// 读udp数据的协程
func (this *RUDP) readUDPDataRoutine() {
	this.wait.Add(1)
	defer this.wait.Done()
	var err error
	for {
		// 读取
		msg := _msgPool.Get().(*udpData)
		msg.n, msg.a, err = this.conn.ReadFromUDP(msg.b[:])
		if err != nil {
			// 出错，关闭
			log.Print(this.logger, log.LevelError, 0, log.FileLineFullPath, err.Error())
			this.Close()
			return
		}
		this.totalBytes.r += uint64(msg.n)
		// 处理
		select {
		case this.udpDataQueue <- msg:
			// 添加到等待处理队列
		case <-this.closeSignal:
			// rudp关闭信号
			return
		default:
			// 等待处理队列已满，添加不进，丢弃
			_msgPool.Put(msg)
		}
	}
}

// 处理udp数据的协程
func (this *RUDP) handleUDPDataRoutine() {
	this.wait.Add(1)
	defer this.wait.Done()
	for {
		select {
		case msg := <-this.udpDataQueue:
			switch msg.b[msgType] {
			case msgDial: // c->s，请求创建连接，握手1
				this.handleMsgDial(msg)
			case msgAccept: // s->c，接受连接，握手2
				this.handleMsgAccept(msg)
			case msgRefuse: // s->c，拒绝连接，握手2
				this.handleMsgRefuse(msg)
			case msgConnect: // c->s，收到接受连接的消息，握手3
				this.handleMsgConnect(msg)
			case msgDataC: // c->s，数据
				this.handleMsgDataC(msg)
			case msgDataS: // s->c，数据
				this.handleMsgDataS(msg)
			case msgAckC: // c->s，收到数据确认
				this.handleMsgAckC(msg)
			case msgAckS: // s->c，收到数据确认
				this.handleMsgAckS(msg)
			case msgCloseC: // c->s，关闭
				this.handleMsgCloseC(msg)
			case msgCloseS: // s->c，关闭
				this.handleMsgCloseS(msg)
			case msgPing: // c->s
				this.handleMsgPing(msg)
			case msgPong: // s->c
				this.handleMsgPong(msg)
			case msgInvalid: // c<->s，无效的连接
				this.handleMsgInvalid(msg)
			}
			// 回收
			_msgPool.Put(msg)
		case <-this.closeSignal:
			// rudp关闭信号
			return
		}
	}
}

// 返回net.OpError对象
func (this *RUDP) netOpError(op string, rAddr net.Addr, err error) error {
	return &net.OpError{
		Op:     op,
		Net:    "udp",
		Source: rAddr,
		Addr:   this.conn.LocalAddr(),
		Err:    err,
	}
}
