package rudp

import (
	"fmt"
	"net"
	"sync"
)

const (
	clientSegment = 1 << 7
	serverSegment = 0
)

// segment类型
const (
	dialSegment = iota
	acceptSegment
	rejectSegment
	pingSegment
	pongSegment
	dataSegment
	discardSegment
	ackSegment
	invalidSegment
)

const (
	protolVersion = 1                               // rudp协议版本
	ipV4Header    = 20                              // IPv4数据包头大小
	ipV6Header    = 40                              // IPv6数据包头大小
	udpHeader     = 8                               // UDP数据包头大小
	minMTU        = 576                             // 链路最小的MTU
	maxMTU        = 1500                            // 链路最大的MTU
	minMSS        = minMTU - ipV6Header - udpHeader // 应用层最小数据包
	maxMSS        = maxMTU - ipV4Header - udpHeader // 应用层最大数据包
)

const (
	segmentType  = 0               // segment类型
	segmentToken = segmentType + 1 // 发起连接的随机token
)

const (
	connectSegmentVersion    = segmentToken + 4             // 协议版本，protolVersion
	connectSegmentTimestamp  = connectSegmentVersion + 1    // 发起连接的时间戳
	connectSegmentLocalIP    = connectSegmentTimestamp + 8  // local listen ip
	connectSegmentLocalPort  = connectSegmentLocalIP + 16   // local listen port
	connectSegmentRemoteIP   = connectSegmentLocalPort + 2  // remote internet ip
	connectSegmentRemotePort = connectSegmentRemoteIP + 16  // remote internet port
	connectSegmentMSS        = connectSegmentRemotePort + 2 // 探测的mss，用于对方调整接收队列
	connectSegmentReadQueue  = connectSegmentMSS + 2        // 接收队列的空闲个数
	connectSegmentTimeout    = connectSegmentReadQueue + 2  // client dial timeout，server发送acceptSegment超时判断
	connectSegmentLength     = connectSegmentTimeout + 8    // 长度
)

const (
	rejectSegmentVersion   = segmentToken + 4           // 协议版本，protolVersion
	rejectSegmentTimestamp = rejectSegmentVersion + 1   // 发起连接的时间戳
	rejectSegmentLength    = rejectSegmentTimestamp + 8 // 长度
)

const (
	pingSegmentSN     = segmentToken + 4  // ping sn
	pingSegmentLength = pingSegmentSN + 4 // 长度
)

const (
	pongSegmentSN     = segmentToken + 4  // ping的sn
	pongSegmentLength = pongSegmentSN + 4 // 长度
)

const (
	dataSegmentSN      = segmentToken + 4  // sn
	dataSegmentPayload = dataSegmentSN + 2 // 数据起始
)

const (
	ackSegmentDataSN        = segmentToken + 4            // 数据包的sn
	ackSegmentDataMaxSN     = ackSegmentDataSN + 2        // 已收到数据包的最大sn
	ackSegmentReadQueueFree = ackSegmentDataMaxSN + 2     // 接收队列的空闲
	ackSegmentSN            = ackSegmentReadQueueFree + 2 // ack的递增sn，sn更新才能更新ackSegmentReadQueueFree
	ackSegmentLength        = ackSegmentSN + 4            // 长度
)

const (
	discardSegmentSN     = segmentToken + 4     // 数据包的sn
	discardSegmentBegin  = discardSegmentSN + 2 // 丢弃的起始sn
	discardSegmentLength = discardSegmentSN + 2 // 长度
)

const (
	invalidSegmentLength = segmentToken + 4
)

var (
	segmentPool  sync.Pool
	checkSegment = []func(seg *segment) bool{
		func(seg *segment) bool {
			fmt.Println("dial segment", seg.a)
			return seg.b[connectSegmentVersion] == protolVersion && seg.n == connectSegmentLength
		}, // dialSegment
		func(seg *segment) bool {
			fmt.Println("accept segment", seg.a)
			return seg.b[connectSegmentVersion] == protolVersion && seg.n == connectSegmentLength
		}, // acceptSegment
		func(seg *segment) bool {
			fmt.Println("reject segment", seg.a)
			return seg.b[rejectSegmentVersion] == protolVersion && seg.n == rejectSegmentLength
		}, // rejectSegment
		func(seg *segment) bool {
			fmt.Println("ping segment", seg.a)
			return seg.n == pingSegmentLength
		}, // pingSegment
		func(seg *segment) bool {
			fmt.Println("pong segment", seg.a)
			return seg.n == pongSegmentLength
		}, // pongSegment
		func(seg *segment) bool {
			fmt.Println("data segment", seg.a)
			return seg.n >= dataSegmentPayload
		}, // dataSegment
		func(seg *segment) bool {
			fmt.Println("discard segment", seg.a)
			return seg.n == discardSegmentLength
		}, // discardSegment
		func(seg *segment) bool {
			fmt.Println("ack segment", seg.a)
			return seg.n == ackSegmentLength
		}, // ackSegment
		func(seg *segment) bool {
			fmt.Println("invalid segment from", seg.a)
			return seg.n == invalidSegmentLength
		}, // invalidSegment
	}
)

func init() {
	segmentPool.New = func() interface{} {
		return new(segment)
	}
}

// 一个udp数据包
type segment struct {
	b [maxMTU]byte
	n int
	a *net.UDPAddr
}
