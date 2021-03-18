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
	dataSegment
	ackSegment
	closeSegment
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
	maxDataSN     = 0xffffff                        // dataSegement的最大sn，24位
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
	dataSegmentSN      = segmentToken + 4  // sn
	dataSegmentPayload = dataSegmentSN + 3 // 数据起始
)

const (
	ackSegmentDataSN        = segmentToken + 4            // 数据包的sn
	ackSegmentDataMaxSN     = ackSegmentDataSN + 3        // 已收到数据包的最大sn
	ackSegmentReadQueueFree = ackSegmentDataMaxSN + 3     // 接收队列的空闲
	ackSegmentSN            = ackSegmentReadQueueFree + 2 // ack的递增sn，sn更新才能更新ackSegmentReadQueueFree
	ackSegmentLength        = ackSegmentSN + 4            // 长度
)

const (
	closeSegmentSN        = segmentToken + 4
	closeSegmentTimestamp = closeSegmentSN + 3
	closeSegmentLength    = closeSegmentTimestamp + 8
)

const (
	invalidSegmentLength = segmentToken + 4
)

var (
	segmentPool  sync.Pool
	checkSegment = []func(seg *segment) bool{
		func(seg *segment) bool {
			fmt.Println("dial segment from", seg.a)
			return seg.b[connectSegmentVersion] == protolVersion && seg.n == connectSegmentLength
		}, // dialSegment
		func(seg *segment) bool {
			fmt.Println("accept segment from", seg.a)
			return seg.b[connectSegmentVersion] == protolVersion && seg.n == connectSegmentLength
		}, // acceptSegment
		func(seg *segment) bool {
			fmt.Println("reject segment from", seg.a)
			return seg.b[rejectSegmentVersion] == protolVersion && seg.n == rejectSegmentLength
		}, // rejectSegment
		func(seg *segment) bool {
			fmt.Println("data segment from", seg.a)
			return seg.n >= dataSegmentPayload
		}, // dataSegment
		func(seg *segment) bool {
			fmt.Println("ack segment from", seg.a)
			return seg.n == ackSegmentLength
		}, // ackSegment
		func(seg *segment) bool {
			fmt.Println("close segment from", seg.a)
			return seg.n == closeSegmentLength
		}, // closeSegment
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

func uint24(b []byte) uint32 {
	return uint32(b[3]) | uint32(b[2])<<8 | uint32(b[1])<<16
}

func putUint24(b []byte, v uint32) {
	b[0] = byte(v >> 16)
	b[1] = byte(v >> 8)
	b[2] = byte(v)
}
