package rudp

import "net"

// 第一字节
const (
	serverSegment = 0b10000000 // server
	clientSegment = 0b00000000 // client
	cmdSegment    = 0b01000000 // 命令
	dataSegment   = 0b00100000 // 数据
	ackSegment    = 0b01100000 // 数据
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
	cmdSegmentTypeDial = iota
	cmdSegmentTypeAccept
	cmdSegmentTypeReject
	cmdSegmentTypeClose
	cmdSegmentTypeInvalid
)

const (
	segmentHeader  = 0
	cmdSegmentType = 1
)

const (
	connectSegmentVersion     = cmdSegmentType + 1 // 协议版本，protolVersion
	connectSegmentClientToken = connectSegmentVersion + 1
	connectSegmentServerToken = connectSegmentClientToken + 4
	connectSegmentLocalIP     = connectSegmentServerToken + 4
	connectSegmentLocalPort   = connectSegmentLocalIP + 16
	connectSegmentRemoteIP    = connectSegmentLocalPort + 2
	connectSegmentRemotePort  = connectSegmentRemoteIP + 16
	connectSegmentMSS         = connectSegmentRemotePort + 2
	connectSegmentFEC         = connectSegmentMSS + 2
	connectSegmentCrypto      = connectSegmentFEC + 1
	connectSegmentExchangeKey = connectSegmentCrypto + 1
	connectSegmentLength      = connectSegmentExchangeKey + 32
)

const (
	rejectSegmentVersion     = cmdSegmentType + 1
	rejectSegmentClientToken = rejectSegmentVersion + 1
	rejectSegmentLength      = rejectSegmentClientToken + 4
)

const (
	invalidSegmentToken  = cmdSegmentType + 1
	invalidSegmentLength = invalidSegmentToken + 4
)

const (
	dataSegmentToken   = 1
	dataSegmentSN      = dataSegmentToken + 4
	dataSegmentLevel   = dataSegmentSN + 2
	dataSegmentPayload = dataSegmentLevel + 1
)

const (
	ackSegmentToken     = 1
	ackSegmentDataSN    = dataSegmentToken + 4
	ackSegmentDataMaxSN = ackSegmentDataSN + 2
	ackSegmentDataFree  = ackSegmentDataMaxSN + 2
	ackSegmentSN        = ackSegmentDataFree + 2
	ackSegmentLength    = ackSegmentSN + 4
)

// 一个udp数据包
type segment struct {
	b [maxMTU]byte
	n int
	a *net.UDPAddr
}
