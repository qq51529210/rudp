package rudp

import (
	"encoding/binary"
	"math/rand"
	"net"
	"sync"
	"time"
)

// 如果n1==0，返回n2
func defaultInt(n1, n2 int) int {
	if n1 == 0 {
		return n2
	}
	return n1
}

// 如果n1<n2，返回n2
func maxInt(n1, n2 int) int {
	if n1 > n2 {
		return n1
	}
	return n2
}

// 如果n1<n2，返回n1
func minInt(n1, n2 int) int {
	if n1 > n2 {
		return n2
	}
	return n1
}

// 读写字节
type uint64RW struct {
	r uint64
	w uint64
}

// 读写字节
type uint32RW struct {
	r uint32
	w uint32
}

// 新建RUDP的参数
type Config struct {
	Listen          string `json:"listen"`            // udp监听地址，不能为空
	UDPDataQueue    int    `json:"udp_data_queue"`    // udp原始数据包的缓存队列，默认是defaultUDPDataQueue
	UDPDataRoutine  int    `json:"udp_data_routine"`  // udp原始数据包的处理协程个数，默认是runtime.NumCPU()
	ConnReadBuffer  int    `json:"conn_read_buffer"`  // conn的读缓存大小，用于窗口控制，默认是defaultReadBuffer
	ConnWriteBuffer int    `json:"conn_write_buffer"` // conn的写缓存大小，用于窗口控制，默认是defaultWriteBuffer
	AcceptQueue     int    `json:"accept_queue"`      // 作为服务端时，接受连接的队列。为0则不作为server，Accept()函数返回错误
	AcceptRTO       int    `json:"accept_rto"`        // 作为服务端时，accept消息超时重发，单位ms，默认100ms
	DialRTO         int    `json:"dial_rto"`          // 作为客户端时，dial消息超时重发，单位ms，默认100ms
}

// ipV4地址转ipV6整数时用
const v4InV6Prefix = uint64(0xff)<<40 | uint64(0xff)<<32

// ip地址作为map连接池的键
type keyAddr struct {
	ip1  uint64 // IPV6地址字符数组前64位
	ip2  uint64 // IPV6地址字符数组后64位
	port uint16 // 端口
}

func (this *keyAddr) Init(addr *net.UDPAddr) {
	if len(addr.IP) == net.IPv4len {
		this.ip1 = 0
		this.ip2 = v4InV6Prefix |
			uint64(addr.IP[0])<<24 |
			uint64(addr.IP[1])<<16 |
			uint64(addr.IP[2])<<8 |
			uint64(addr.IP[3])
	} else {
		this.ip1 = binary.BigEndian.Uint64(addr.IP[0:])
		this.ip2 = binary.BigEndian.Uint64(addr.IP[8:])
	}
	this.port = uint16(addr.Port)
}

// 连接池的键，因为客户端可能在nat后面，所以加上token
type connKey struct {
	keyAddr
	cToken uint32 // 客户端32位随机码
	sToken uint32 // 服务端32位随机码
}

// ip传输包的参数
const (
	ipV4Header = 20                              // IPv4数据包头大小
	ipV6Header = 40                              // IPv6数据包头大小
	udpHeader  = 8                               // UDP数据包头大小
	minMTU     = 576                             // 链路最小的MTU
	maxMTU     = 1500                            // 链路最大的MTU
	minMSS     = minMTU - ipV6Header - udpHeader // 应用层最小数据包
	maxMSS     = maxMTU - ipV4Header - udpHeader // 应用层最大数据包
)

// RUDP是一个同时具备C/S能力的引擎
const (
	ClientConn = 0
	ServerConn = 1
)

// 一些默认值
const (
	defaultUDPDataQueue = 1024     // 默认的udp数据包缓存队列
	defaultReadBuffer   = 1024 * 4 // Conn默认的读缓存大小
	defaultWriteBuffer  = 1024 * 4 // Conn默认的写缓存大小
	defaultConnectRTO   = 100      // 客户端/服务端，建立连接，默认的发送超时，毫秒
)

const (
	maxUint32 uint32 = 1<<32 - 1 // 32位整数最大值
)

// 表示一个udp的原始数据包
type udpData struct {
	buf  [msgBuffLen]byte // 数据缓存
	len  int              // 数据大小
	addr *net.UDPAddr     // udp地址
}

// udp数据包的缓存池
var udpDataPool sync.Pool

type readData struct {
	sn   uint32    // 序号
	buf  []byte    // 数据
	idx  int       // 数据下标
	next *readData // 下一个
}

// readData的缓存池
var readDataPool sync.Pool

type writeData struct {
	sn    uint32     // 序号
	data  udpData    // 数据
	next  *writeData // 下一个
	first time.Time  // 第一次被发送的时间，用于计算rtt
	last  time.Time  // 上一次被发送的时间，超时重发判断
}

// writeData的缓存池
var writeDataPool sync.Pool

// 随机数
var _rand = rand.New(rand.NewSource(time.Now().Unix()))

// 为了避免分包，应该探测本地到对方的udp数据包最合适的mss
// 这里返回最小值，可以重写这个函数来确定最大值
var DetectMSS = func(*net.UDPAddr) uint16 {
	return minMSS
}

func init() {
	udpDataPool.New = func() interface{} {
		return new(udpData)
	}
	readDataPool.New = func() interface{} {
		return new(readData)
	}
	writeDataPool.New = func() interface{} {
		return new(writeData)
	}
}

type opErr string

func (e opErr) Error() string   { return string(e) }
func (e opErr) Timeout() bool   { return string(e) == "timeout" }
func (e opErr) Temporary() bool { return false }

type closeErr string

func (e closeErr) Error() string   { return string(e) + " has been closed" }
func (e closeErr) Timeout() bool   { return false }
func (e closeErr) Temporary() bool { return false }
