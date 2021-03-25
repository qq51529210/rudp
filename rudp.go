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
	connectRTO    = 300 * time.Millisecond                      // connectSegment超时重传，毫秒
	maxConnection = 1024 * 1024                                 // 最大的连接数
	mathRand      = rand.New(rand.NewSource(time.Now().Unix())) // 随机数
	DetectLink    = detectLink                                  // 返回mss和rtt
	errConnClosed = errors.New("conn has been closed")          // conn被关闭
	errRudpClosed = errors.New("rudp has been closed")          // rudp被关闭
	checkSegment  = []func(seg *segment) bool{
		func(seg *segment) bool {
			return seg.b[connectSegmentVersion] == protolVersion && seg.n == connectSegmentLength
		}, // dialSegment
		func(seg *segment) bool {
			return seg.b[connectSegmentVersion] == protolVersion && seg.n == connectSegmentLength
		}, // acceptSegment
		func(seg *segment) bool {
			return seg.b[rejectSegmentVersion] == protolVersion && seg.n == rejectSegmentLength
		}, // rejectSegment
		func(seg *segment) bool {
			return seg.n >= dataSegmentPayload
		}, // dataSegment
		func(seg *segment) bool {
			return seg.n == ackSegmentLength
		}, // ackSegment
		func(seg *segment) bool {
			return seg.n == closeSegmentLength
		}, // closeSegment
		func(seg *segment) bool {
			return seg.n == invalidSegmentLength
		}, // invalidSegment
	}
)

// 连接池的键，因为客户端可能在nat后面，所以加上token
type connKey struct {
	ip1   uint64 // IPV6地址字符数组前64位
	ip2   uint64 // IPV6地址字符数组后64位
	port  uint16 // 端口
	token uint32 // token
}

// 将128位的ip地址（v4的转成v6）的字节分成两个64位整数，加上端口，作为key
func (k *connKey) Init(a *net.UDPAddr) {
	if len(a.IP) == net.IPv4len {
		k.ip1 = 0
		k.ip2 = uint64(0xff)<<40 | uint64(0xff)<<32 |
			uint64(a.IP[0])<<24 | uint64(a.IP[1])<<16 |
			uint64(a.IP[2])<<8 | uint64(a.IP[3])
	} else {
		k.ip1 = binary.BigEndian.Uint64(a.IP[0:])
		k.ip2 = binary.BigEndian.Uint64(a.IP[8:])
	}
	k.port = uint16(a.Port)
}

// 探测链路的mss，返回最适合的mss和rtt，以免被分包。
func detectLink(*net.UDPAddr) (uint16, time.Duration, error) {
	return minMSS, connMinRTO * 2, nil
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
	readBytes       uint64               // udp读取总字节
	writeBytes      uint64               // udp发送总字节
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

// 最大的连接数，为0表示不接收连接
func (r *RUDP) SetMaxConnection(n int) {
	if n < 0 {
		r.maxConnection = 0
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
	// 等待所有协程退出
	r.waitGroup.Wait()
	return nil
}

// net.Listener接口
func (r *RUDP) Accept() (net.Conn, error) {
	r.acceptCond.L.Lock()
	for !r.closed && r.acceptConn.Len() < 1 {
		r.acceptCond.Wait()
	}
	if r.closed {
		r.acceptCond.L.Unlock()
		return nil, r.netOpError("accept", errRudpClosed)
	}
	conn := r.acceptConn.Remove(r.acceptConn.Front()).(*Conn)
	r.acceptCond.L.Unlock()
	r.waitGroup.Add(1)
	go r.connHandleSegmentRoutine(conn)
	return conn, nil
}

// 连接指定的地址，对于一个地址，可以创建2^32个连接
func (r *RUDP) Dial(address string, timeout time.Duration) (*Conn, error) {
	// 解析地址
	addr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	// 探测mss
	mss, rtt, err := DetectLink(addr)
	if err != nil {
		return nil, err
	}
	if mss > maxMSS {
		mss = maxMSS
	}
	if mss < minMSS {
		mss = minMSS
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
			return nil, r.netOpError("dial", errRudpClosed)
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
			return nil, r.netOpError("dial", fmt.Errorf("too many connections to %s", address))
		}
	}
	// 初始化Conn
	r.initConn(conn, addr, clientSegment, key.token)
	conn.timestamp = uint64(time.Now().Unix())
	conn.writeMSS = mss
	conn.initRTO(rtt)
	r.waitGroup.Add(1)
	go r.connHandleSegmentRoutine(conn)
	// 初始化dialSegment
	conn.encConnectSegment(conn.buff[:], dialSegment)
	timeout -= r.connectRTO
	for {
		select {
		case now := <-conn.timer.C:
			if time.Until(now) >= timeout {
				// 连接超时
				err = new(timeoutError)
			} else {
				// 超时重传
				r.WriteToConn(conn.buff[:connectSegmentLength], conn)
				conn.timer.Reset(r.connectRTO)
			}
		case state := <-conn.stateSignal: // 连接信号
			switch state {
			case connectState: // 已连接，返回
				r.waitGroup.Add(1)
				go r.connCheckRTORoutine(conn)
				return conn, nil
			case dialState: // 拒绝连接，返回
				err = errors.New("connect reject")
			default:
				err = errConnClosed
			}
		case <-r.closeSignal: // RUDP关闭信号
			err = errRudpClosed
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
		r.writeBytes += uint64(n)
	}
	return n, err
}

// 使用net.UDPConn向指定地址发送指定数据
func (r *RUDP) WriteToConn(data []byte, conn *Conn) (int, error) {
	n, err := r.conn.WriteToUDP(data, conn.remoteInternetAddr)
	if err == nil {
		conn.writeBytes += uint64(n)
		r.writeBytes += uint64(n)
	}
	return n, err
}

// 返回net.UDPConn的本地地址
func (r *RUDP) LocalAddr() net.Addr {
	return r.conn.LocalAddr()
}

// 读取的总字节
func (r *RUDP) ReadBytes() uint64 {
	return r.readBytes
}

// 发送的总字节
func (r *RUDP) WriteBytes() uint64 {
	return r.writeBytes
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

// 初始化Conn的一些变量
func (r *RUDP) initConn(conn *Conn, addr *net.UDPAddr, from byte, token uint32) {
	conn.from = from
	conn.token = token
	conn.state = dialState
	conn.stateSignal = make(chan byte, 1)
	conn.handleQueue = make(chan *segment, r.connHandleQueue)
	conn.localListenAddr = r.conn.LocalAddr().(*net.UDPAddr)
	conn.remoteListenAddr = new(net.UDPAddr)
	conn.remoteListenAddr.IP = make(net.IP, net.IPv6len)
	conn.localInternetAddr = new(net.UDPAddr)
	conn.localInternetAddr.IP = make(net.IP, net.IPv6len)
	conn.remoteInternetAddr = addr
	conn.readSignle = make(chan byte, 1)
	conn.writeSignle = make(chan byte, 1)
	conn.readCap = connReadQueue
	conn.writeCap = connWriteQueue
	conn.timer = time.NewTimer(0)
	// test
	conn.readSN = maxDataSN - 20
	conn.writeSN = conn.readSN
}

// 从连接池中移除Conn
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
	key.Init(conn.remoteInternetAddr)
	key.token = conn.token
	r.lock[from].Lock()
	delete(r.connection[from], key)
	r.lock[from].Unlock()
	// 释放资源
	close(conn.stateSignal)
	close(conn.readSignle)
	close(conn.writeSignle)
	conn.releaseReadData()
	conn.releaseWriteData()
	close(conn.handleQueue)
	for seg := range conn.handleQueue {
		segmentPool.Put(seg)
	}
}

// 读取并处理udp数据包
func (r *RUDP) handleUDPDataRoutine() {
	defer func() {
		r.waitGroup.Done()
		fmt.Println("handleUDPDataRoutine exit")
	}()
	var err error
	var from byte
	var conn *Conn
	var ok bool
	key := new(connKey)
	for !r.closed {
		seg := segmentPool.Get().(*segment)
		// 读取udp数据
		seg.n, seg.a, err = r.conn.ReadFromUDP(seg.b[:])
		if err != nil {
			continue
		}
		r.readBytes += uint64(seg.n)
		// 0:serverSegement，1:clientSegment
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
					// 在routine中移除
					if old.state&invalidState == 0 {
						r.waitGroup.Add(1)
						go func(c *Conn) {
							defer r.waitGroup.Done()
							r.removeConn(c)
						}(old)
					}
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
			if ok && conn.state&invalidState == 0 {
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
				seg.b[segmentType] = invalidSegment | (^from)<<7
				r.WriteTo(seg.b[:invalidSegmentLength], seg.a)
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

// Conn处理消息和超时重发数据
func (r *RUDP) connHandleSegmentRoutine(conn *Conn) {
	defer func() {
		r.removeConn(conn)
		r.waitGroup.Done()
		fmt.Println("connHandleSegmentRoutine exit")
	}()
	for !r.closed {
		select {
		case <-r.closeSignal: // RUDP关闭信号
			return
		case <-conn.stateSignal: // Conn关闭信号
			return
		case seg, ok := <-conn.handleQueue: // 消息处理
			if !ok {
				return
			}
			conn.readBytes += uint64(seg.n)
			switch seg.b[0] {
			case dialSegment:
				fmt.Println(seg.a, "dial segment")
				conn.lock.Lock()
				switch conn.state {
				case dialState:
					conn.state = connectState
					conn.lock.Unlock()
					// 探测mss
					mss, rtt, err := DetectLink(seg.a)
					if err != nil {
						return
					}
					if mss > maxMSS {
						mss = maxMSS
					}
					if mss < minMSS {
						mss = minMSS
					}
					conn.writeMSS = mss
					conn.initRTO(rtt)
					// 解析acceptSegment字段
					conn.decConnectSegment(seg)
					// 加入acceptConn等待应用层Accept()
					r.acceptCond.L.Lock()
					r.acceptConn.PushBack(conn)
					r.acceptCond.L.Unlock()
					r.acceptCond.Broadcast()
					// 响应acceptSegment
					conn.encConnectSegment(seg.b[:], acceptSegment)
					r.WriteToConn(seg.b[:connectSegmentLength], conn)
				case connectState:
					conn.lock.Unlock()
					// 响应acceptSegment
					conn.encConnectSegment(seg.b[:], acceptSegment)
					r.WriteToConn(seg.b[:connectSegmentLength], conn)
				default:
					conn.lock.Unlock()
				}
			case acceptSegment:
				fmt.Println(seg.a, "accept segment")
				conn.lock.Lock()
				switch conn.state {
				case dialState:
					// 修改状态
					conn.state = connectState
					conn.lock.Unlock()
					// 解析acceptSegment字段
					conn.decConnectSegment(seg)
					// 发送信号
					conn.stateSignal <- connectState
				case closedState:
					conn.lock.Unlock()
					// 已经关闭，响应invalidSegment
					seg.b[segmentType] = invalidSegment | conn.from
					r.WriteTo(seg.b[:invalidSegmentLength], seg.a)
				default:
					conn.lock.Unlock()
				}
			case rejectSegment:
				// 时间戳不对
				timestamp := binary.BigEndian.Uint64(seg.b[rejectSegmentTimestamp:])
				if conn.timestamp != timestamp {
					break
				}
				fmt.Println(seg.a, "reject segment", "timestamp", timestamp)
				conn.lock.Lock()
				switch conn.state {
				case dialState:
					// 发起连接状态
					conn.lock.Unlock()
					// 发送信号
					conn.stateSignal <- dialState
				default:
					conn.lock.Unlock()
				}
			case dataSegment:
				// 消息长度不对，或者状态不对，不处理
				if uint16(seg.n) > conn.remoteWriteMSS || conn.state != connectState {
					break
				}
				sn := uint24(seg.b[dataSegmentSN:])
				// 尝试添加
				conn.readLock.Lock()
				ok = conn.addReadData(sn, seg.b[dataSegmentPayload:seg.n])
				conn.encAckSegment(seg, sn)
				conn.readLock.Unlock()
				// 响应ackSegment
				r.WriteToConn(seg.b[:ackSegmentLength], conn)
				if ok {
					fmt.Println(seg.a, "data segment", "sn", sn, "len", seg.n-dataSegmentPayload)
					// 通知可读
					select {
					case conn.readSignle <- 1:
					default:
					}
				}
			case ackSegment:
				// 状态不对，不处理
				if conn.state&invalidState != 0 {
					break
				}
				sn := uint24(seg.b[ackSegmentDataSN:])
				maxSN := uint24(seg.b[ackSegmentDataMaxSN:])
				// 移除发送队列
				conn.writeLock.Lock()
				ok = conn.removeWriteData(sn, maxSN)
				conn.writeLock.Unlock()
				// 更新ack sn
				ackSN := binary.BigEndian.Uint64(seg.b[ackSegmentTimestamp:])
				if ackSN > conn.readAckSN || conn.readAckSN-ackSN >= 0xffffffff {
					conn.readAckSN = ackSN
					conn.remoteReadLen = binary.BigEndian.Uint16(seg.b[ackSegmentReadQueueLength:])
				}
				if ok {
					fmt.Println(seg.a, "ack segment", "sn", sn, "max", maxSN, "ack", ackSN)
					select {
					case conn.writeSignle <- 1:
					default:
					}
				}
			case closeSegment:
				timestamp := binary.BigEndian.Uint64(seg.b[closeSegmentTimestamp:])
				if conn.timestamp != timestamp {
					break
				}
				fmt.Println(seg.a, "close segment")
				conn.state |= shutdownState
				seg.b[segmentType] = invalidSegment | conn.from
				r.WriteTo(seg.b[:invalidSegmentLength], seg.a)
				segmentPool.Put(seg)
				return
			}
			segmentPool.Put(seg)
		}
	}
}

func (r *RUDP) connCheckRTORoutine(conn *Conn) {
	defer func() {
		conn.timer.Stop()
		r.waitGroup.Done()
		fmt.Println("connCheckRTORoutine exit")
	}()
	conn.timer.Reset(conn.rto)
	for !r.closed {
		select {
		case <-r.closeSignal: // RUDP关闭信号
			return
		case <-conn.stateSignal: // Conn关闭信号
			return
		case now := <-conn.timer.C: // 超时重传检查
			conn.writeLock.Lock()
			conn.checkRTO(now, r)
			conn.writeLock.Unlock()
			conn.timer.Reset(conn.rto)
		}
	}
}
