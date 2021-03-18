package rudp

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"runtime"
	"sync"
	"time"
)

var (
	connectRTO      = 300 * time.Millisecond                      // connectSegment超时重传，毫秒
	maxConnection   = 1024 * 1024                                 // 最大的连接数
	mathRand        = rand.New(rand.NewSource(time.Now().Unix())) // 随机数
	DetectLink      = detectLink                                  // 返回mss和rtt
	connClosedError = errors.New("conn has been closed")          // conn被关闭
	rudpClosedError = errors.New("rudp has been closed")          // rudp被关闭
)

// 连接池的键，因为客户端可能在nat后面，所以加上token
type connKey struct {
	ip1   uint64 // IPV6地址字符数组前64位
	ip2   uint64 // IPV6地址字符数组后64位
	port  uint16 // 端口
	token uint32 // token
}

// 将128位的ip地址（v4的转成v6）的字节分成两个64位整数，加上端口，作为key
func (this *connKey) Init(a *net.UDPAddr) {
	if len(a.IP) == net.IPv4len {
		this.ip1 = 0
		this.ip2 = uint64(0xff)<<40 | uint64(0xff)<<32 |
			uint64(a.IP[0])<<24 | uint64(a.IP[1])<<16 |
			uint64(a.IP[2])<<8 | uint64(a.IP[3])
	} else {
		this.ip1 = binary.BigEndian.Uint64(a.IP[0:])
		this.ip2 = binary.BigEndian.Uint64(a.IP[8:])
	}
	this.port = uint16(a.Port)
}

// 探测链路的mss，返回最适合的mss和rto，以免被分包。
func detectLink(*net.UDPAddr) (uint16, time.Duration, error) {
	return minMSS, connMinRTO, nil
}

// 使用配置值创建一个新的RUDP，server能力需要配置Config.AcceptQueue
func Listen(address string) (*RUDP, error) {
	// 解析地址
	addr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	// 绑定地址
	conn, err := net.ListenUDP(addr.Network(), addr)
	if err != nil {
		return nil, err
	}
	// 初始化成员变量
	r := new(RUDP)
	r.conn = conn
	r.closeSignal = make(chan struct{})
	r.connectRTO = connectRTO
	r.connHandleQueue = connHandleQueue
	r.maxConnection = maxConnection
	// 使用的是server lock
	r.acceptCond.L = new(sync.Mutex)
	for i := 0; i < len(r.connection); i++ {
		r.connection[i] = make(map[connKey]*Conn)
	}
	// 读取udp数据并处理
	for i := 0; i < runtime.NumCPU(); i++ {
		r.waitGroup.Add(1)
		go r.handleUDPDataRoutine()
	}
	return r, nil
}

// RUDP是一个同时具备C/S能力的引擎
type RUDP struct {
	conn            *net.UDPConn         // 底层socket
	waitGroup       sync.WaitGroup       // 等待所有协程退出
	closeSignal     chan struct{}        // 通知所有协程退出的信号
	closed          bool                 // 是否调用了Close()
	connectRTO      time.Duration        // 建立连接，connect segment的超时重发
	connHandleQueue uint16               // 新连接的segment处理队列大小
	ioBytes         [2]uint64            // io总字节，0:read，1:write
	lock            [2]sync.RWMutex      // 相关数据的锁，0:client，1:server
	connection      [2]map[connKey]*Conn // 所有的Conn，0:client，1:server
	acceptConn      list.List            // 已经建立连接的Conn，等待Accept()调用
	acceptCond      sync.Cond            // 等待client连接的信号
	maxConnection   int                  // 最大的连接数量
}

// 建立连接，发送connect segment的超时重传，最小1ms
func (r *RUDP) SetConnectRTO(timeout time.Duration) {
	if timeout < time.Millisecond {
		r.connectRTO = time.Millisecond
	} else {
		r.connectRTO = timeout
	}
}

// 最大的连接数
func (r *RUDP) SetMaxConnection(n int) {
	if n < 1 {
		r.maxConnection = maxConnection
	} else {
		r.maxConnection = n
	}
}

// net.Listener接口
func (r *RUDP) Addr() net.Addr {
	return r.conn.LocalAddr()
}

// net.Listener接口
func (r *RUDP) Close() error {
	// 关闭底层Conn
	err := r.conn.Close()
	if err != nil {
		return err
	}
	r.closed = true
	// 通知所有协程退出
	close(r.closeSignal)
	r.acceptCond.Broadcast()
	// 关闭所有的Conn
	for i := 0; i < len(r.connection); i++ {
		for _, v := range r.connection[i] {
			r.removeConn(v)
		}
	}
	// 等待所有协程退出
	r.waitGroup.Wait()
	return nil
}

// net.Listener接口
func (r *RUDP) Accept() (net.Conn, error) {
	for {
		r.acceptCond.L.Lock()
		if r.closed {
			r.acceptCond.L.Unlock()
			break
		}
		if r.acceptConn.Len() < 1 {
			r.acceptCond.Wait()
		}
		v := r.acceptConn.Remove(r.acceptConn.Front())
		r.acceptCond.L.Unlock()
		return v.(*Conn), nil
	}
	return nil, r.netOpError("accept", rudpClosedError)
}

// 连接指定的地址，对于一个地址，可以创建2^32个连接
func (r *RUDP) Dial(address string, timeout time.Duration) (*Conn, error) {
	// 解析地址
	addr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	// 新的client Conn
	conn, err := r.newClientConn(addr)
	if err != nil {
		return nil, err
	}
	// 初始化dialSegment
	conn.encConnectSegment(conn.buff[:], dialSegment, timeout)
	for {
		select {
		case <-time.After(timeout): // 连接超时
			err = new(timeoutError)
		case <-conn.timer.C: // 超时重传
			r.WriteToConn(conn.buff[:connectSegmentLength], conn)
			conn.timer.Reset(r.connectRTO)
		case state := <-conn.stateSignal: // 连接信号
			switch state {
			case connectState: // 已连接，返回
				r.waitGroup.Add(1)
				go r.connRtoRoutine(conn)
				return conn, nil
			case dialState: // 拒绝连接，返回
				err = errors.New("connect reject")
			default:
				err = connClosedError
			}
		case <-r.closeSignal: // RUDP关闭信号
			err = rudpClosedError
		}
		if err != nil {
			r.removeConn(conn)
			return nil, conn.netOpError("dial", err)
		}
	}
}

// 使用net.UDPConn向指定地址发送指定数据
func (r *RUDP) WriteTo(data []byte, addr *net.UDPAddr) (int, error) {
	n, err := r.conn.WriteToUDP(data, addr)
	if err == nil {
		r.ioBytes[1] += uint64(n)
	}
	return n, err
}

// 使用net.UDPConn向指定地址发送指定数据
func (r *RUDP) WriteToConn(data []byte, conn *Conn) (int, error) {
	n, err := r.conn.WriteToUDP(data, conn.internetAddr[1])
	if err == nil {
		conn.ioBytes[1] += uint64(n)
		r.ioBytes[1] += uint64(n)
	}
	return n, err
}

// 返回net.UDPConn的本地地址
func (r *RUDP) LocalAddr() net.Addr {
	return r.conn.LocalAddr()
}

// 读取的总字节
func (r *RUDP) ReadBytes() uint64 {
	return r.ioBytes[0]
}

// 发送的总字节
func (r *RUDP) WriteBytes() uint64 {
	return r.ioBytes[1]
}

// 返回*net.OpError
func (r *RUDP) netOpError(op string, err error) error {
	return &net.OpError{
		Op:     op,
		Net:    "udp",
		Source: nil,
		Addr:   r.conn.LocalAddr(),
		Err:    err,
	}
}

// 读取并处理udp数据包
func (r *RUDP) handleUDPDataRoutine() {
	defer r.waitGroup.Done()
	var err error
	var from byte
	var conn *Conn
	var ok bool
	key := new(connKey)
	for !r.closed {
		seg := segmentPool.Get().(*segment)
		// 读取数据并处理
		seg.n, seg.a, err = r.conn.ReadFromUDP(seg.b[:])
		if err != nil {
			// todo: 日志
			continue
		}
		r.ioBytes[0] += uint64(seg.n)
		// 处理
		from = seg.b[0] >> 7
		seg.b[0] = seg.b[0] & 0b01111111
		if seg.b[0] > invalidSegment || !checkSegment[seg.b[0]](seg) {
			segmentPool.Put(seg)
			continue
		}
		key.Init(seg.a)
		key.token = binary.BigEndian.Uint32(seg.b[segmentToken:])
		switch seg.b[0] {
		case dialSegment:
			timestamp := binary.BigEndian.Uint64(seg.b[connectSegmentTimestamp:])
			// 最大连接数
			r.lock[1].Lock()
			if len(r.connection[1]) >= r.maxConnection {
				r.lock[1].Unlock()
				// 响应rejectSegment
				seg.b[0] = rejectSegment | serverSegment
				seg.b[rejectSegmentVersion] = protolVersion
				binary.BigEndian.PutUint64(seg.b[rejectSegmentTimestamp:], timestamp)
				r.WriteTo(seg.b[:rejectSegmentLength], seg.a)
				segmentPool.Put(seg)
				continue
			}
			// 获取Conn
			conn, ok = r.connection[1][*key]
			if !ok {
				// 不存在，注册到connection
				conn = new(Conn)
				conn.timestamp = timestamp
				r.connection[1][*key] = conn
				r.lock[1].Unlock()
				r.initConn(conn, seg.a, serverSegment, key.token)
				r.waitGroup.Add(1)
				go r.connHandleSegmentRoutine(conn)
			} else {
				// 有相应的Conn，但是timestamp比较新，说明是新的dialSegment
				if timestamp > conn.timestamp {
					// 原来的
					old := conn
					// 新的
					conn = new(Conn)
					conn.timestamp = timestamp
					r.connection[1][*key] = conn
					r.lock[1].Unlock()
					r.initConn(conn, seg.a, serverSegment, key.token)
					// 启动routine
					r.waitGroup.Add(1)
					go r.connHandleSegmentRoutine(conn)
					// 在routine中关闭
					r.waitGroup.Add(1)
					go func(c *Conn) {
						defer r.waitGroup.Done()
						r.removeConn(c)
					}(old)
				}
			}
			// 添加到Conn处理队列
			select {
			case conn.handleQueue <- seg:
			default:
				segmentPool.Put(seg)
			}
		case invalidSegment:
			r.lock[from].RLock()
			conn, ok = r.connection[from][*key]
			r.lock[from].RUnlock()
			if ok {
				// 在routine中关闭
				r.waitGroup.Add(1)
				go func(c *Conn) {
					defer r.waitGroup.Done()
					r.removeConn(c)
				}(conn)
			}
			segmentPool.Put(seg)
		default:
			r.lock[from].RLock()
			conn, ok = r.connection[from][*key]
			r.lock[from].RUnlock()
			if !ok {
				// 不存在响应invalidSegment
				r.writeInvalidSegment(seg, (^from)<<7, key.token)
				segmentPool.Put(seg)
			} else {
				// 添加到Conn处理队列
				select {
				case conn.handleQueue <- seg:
				default:
					segmentPool.Put(seg)
				}
			}
		}
	}
}

// 发送invalidSegment
func (r *RUDP) writeInvalidSegment(seg *segment, from byte, token uint32) {
	seg.b[segmentType] = invalidSegment | from
	binary.BigEndian.PutUint32(seg.b[segmentToken:], token)
	r.WriteTo(seg.b[:invalidSegmentLength], seg.a)
}

func (r *RUDP) newClientConn(addr *net.UDPAddr) (*Conn, error) {
	// 探测mss
	mss, rto, err := DetectLink(addr)
	if err != nil {
		return nil, err
	}
	// 注册到connection表
	var key connKey
	key.Init(addr)
	key.token = mathRand.Uint32()
	// 初始token
	token := key.token
	var conn *Conn
	var ok bool
	// 检查没有使用的token
	r.lock[0].Lock()
	for {
		// 已关闭
		if r.closed {
			r.lock[0].Unlock()
			return nil, r.netOpError("dial", rudpClosedError)
		}
		_, ok = r.connection[0][key]
		if !ok {
			conn = new(Conn)
			r.connection[0][key] = conn
			r.lock[0].Unlock()
			break
		}
		key.token++
		if key.token == token {
			r.lock[0].Unlock()
			return nil, r.netOpError("dial", fmt.Errorf("too many connections to %s", addr.String()))
		}
	}
	// 初始化Conn
	r.initConn(conn, addr, clientSegment, key.token)
	conn.timestamp = uint64(time.Now().Unix())
	conn.dataMSS[0] = mss
	conn.rto = rto
	// conn处理segment
	r.waitGroup.Add(1)
	go r.connHandleSegmentRoutine(conn)
	return conn, nil
}

func (r *RUDP) initConn(conn *Conn, addr *net.UDPAddr, from byte, token uint32) {
	conn.from = from
	conn.token = token
	conn.state = dialState
	conn.stateSignal = make(chan byte, 1)
	conn.handleQueue = make(chan *segment, r.connHandleQueue)
	conn.listenAddr[0] = r.conn.LocalAddr().(*net.UDPAddr)
	conn.listenAddr[1] = new(net.UDPAddr)
	conn.internetAddr[0] = new(net.UDPAddr)
	conn.internetAddr[1] = addr
	conn.dataEnable[0] = make(chan byte, 1)
	conn.dataEnable[1] = make(chan byte, 1)
	conn.dataCap[0] = connReadQueue
	conn.dataCap[1] = connWriteQueue
	conn.minRTO = connMinRTO
	conn.maxRTO = connMaxRTO
	conn.timer = time.NewTimer(0)
}

func (r *RUDP) removeConn(conn *Conn) {
	// 修改状态
	conn.lock.Lock()
	if conn.state&invalidState != 0 {
		conn.lock.Unlock()
		return
	}
	conn.state |= invalidState
	conn.lock.Unlock()
	// 移除
	from := (^conn.from) >> 7
	var key connKey
	key.Init(conn.internetAddr[1])
	key.token = conn.token
	r.lock[from].Lock()
	delete(r.connection[from], key)
	r.lock[from].Unlock()
	// 释放资源
	close(conn.stateSignal)
	close(conn.dataEnable[0])
	close(conn.dataEnable[1])
	conn.releaseReadData()
	conn.releaseWriteData()
	close(conn.handleQueue)
	for seg := range conn.handleQueue {
		segmentPool.Put(seg)
	}
}

func (r *RUDP) connRtoRoutine(conn *Conn) {
	defer func() {
		conn.timer.Stop()
		r.removeConn(conn)
		r.waitGroup.Done()
	}()
	conn.timer.Reset(conn.rto)
	for {
		select {
		case <-r.closeSignal: // RUDP关闭信号
			return
		case <-conn.stateSignal: // Conn关闭信号
			return
		case now := <-conn.timer.C: // 超时重传检查
			conn.dataLock[1].Lock()
			conn.checkRTO(now, r)
			conn.dataLock[1].Unlock()
			conn.timer.Reset(conn.rto)
		}
	}
}

func (r *RUDP) connHandleSegmentRoutine(conn *Conn) {
	defer func() {
		r.removeConn(conn)
		r.waitGroup.Done()
	}()
	for !r.closed {
		select {
		case <-r.closeSignal:
			return
		case <-conn.stateSignal:
			return
		case seg, ok := <-conn.handleQueue:
			if !ok {
				return
			}
			switch seg.b[0] {
			case dialSegment:
				conn.lock.Lock()
				switch conn.state {
				case dialState:
					conn.state = connectState
					conn.lock.Unlock()
					// 探测mss
					var err error
					conn.dataMSS[0], conn.rto, err = DetectLink(seg.a)
					if err != nil {
						return
					}
					// 解析acceptSegment字段
					conn.decConnectSegment(seg)
					// 加入acceptConn
					r.acceptCond.L.Lock()
					r.acceptConn.PushBack(conn)
					r.acceptCond.L.Unlock()
					r.acceptCond.Broadcast()
					// rto routine
					r.waitGroup.Add(1)
					go r.connRtoRoutine(conn)
					// 响应acceptSegment
					conn.encConnectSegment(seg.b[:], acceptSegment, 0)
					r.WriteToConn(seg.b[:connectSegmentLength], conn)
				case connectState:
					conn.lock.Unlock()
					// 响应acceptSegment
					conn.encConnectSegment(seg.b[:], acceptSegment, 0)
					r.WriteToConn(seg.b[:connectSegmentLength], conn)
				default:
					conn.lock.Unlock()
				}
			case acceptSegment:
				if conn.from == clientSegment {
					conn.lock.Lock()
					switch conn.state {
					case dialState:
						// 状态
						conn.state = connectState
						conn.lock.Unlock()
						// 解析acceptSegment字段
						conn.decConnectSegment(seg)
						// 发送信号
						conn.stateSignal <- connectState
					case closedState:
						conn.lock.Unlock()
						// 已经关闭，响应invalidSegment
						r.writeInvalidSegment(seg, conn.from, conn.token)
					default:
						conn.lock.Unlock()
					}
				}
			case rejectSegment:
				if conn.from == clientSegment {
					timestamp := binary.BigEndian.Uint64(seg.b[rejectSegmentTimestamp:])
					if conn.timestamp == timestamp {
						conn.lock.Lock()
						switch conn.state {
						case dialState:
							conn.lock.Unlock()
							// 发送信号
							conn.stateSignal <- dialState
						default:
							conn.lock.Unlock()
						}
					}
				}
			case dataSegment:
				if uint16(seg.n) <= conn.dataMSS[1] && conn.state == connectState {
					sn := uint24(seg.b[dataSegmentSN:])
					data := seg.b[dataSegmentPayload:seg.n]
					// 尝试添加
					conn.dataLock[0].Lock()
					ok = conn.addReadData(sn, data)
					conn.dataLock[0].Unlock()
					// 响应ackSegment
					conn.writeAckSegment(r, seg, sn)
					if ok {
						// 通知可读
						select {
						case conn.dataEnable[0] <- 1:
						default:
						}
					}
				} else {
					r.writeInvalidSegment(seg, conn.from, conn.token)
				}
			case ackSegment:
				if conn.state != invalidSegment {
					sn := uint24(seg.b[ackSegmentDataSN:])
					maxSN := uint24(seg.b[ackSegmentDataMaxSN:])
					// 移除
					conn.dataLock[1].Lock()
					if maxSN > sn {
						ok = conn.removeWriteDataBefore(maxSN)
					} else {
						ok = conn.removeWriteData(sn)
					}
					conn.dataLock[1].Unlock()
					// 更新
					ackSN := binary.BigEndian.Uint32(seg.b[ackSegmentSN:])
					if ackSN > conn.ackSN[1] {
						conn.remoteReadQueue = binary.BigEndian.Uint16(seg.b[ackSegmentReadQueueFree:])
					}
					if ok {
						select {
						case conn.dataEnable[1] <- 1:
						default:
						}
					}
				}
			case closeSegment:
				if conn.state&closedState == 0 {
					conn.state |= closedState
				}
				r.writeInvalidSegment(seg, conn.from, conn.token)
				segmentPool.Put(seg)
				return
			}
			segmentPool.Put(seg)
		}
	}
}
