package rudp

import (
	"encoding/binary"
	"net"
)

// V4地址转V6整数时用
const v4InV6Prefix = uint64(0xff)<<40 | uint64(0xff)<<32

// 创建连接是
type dialKey struct {
	ip1  uint64 // IPV6转整数前64位
	ip2  uint64 // IPV6转整数后64位
	port uint16 // 端口
	tkn  uint32 // 客户端产生的随机数
}

// 从net.UDPAddr赋值。
func (this *dialKey) Init(addr *net.UDPAddr) {
	switch len(addr.IP) {
	case net.IPv4len:
		this.ip1 = 0
		this.ip2 = v4InV6Prefix | uint64(addr.IP[0])<<24 |
			uint64(addr.IP[1])<<16 | uint64(addr.IP[2])<<8 | uint64(addr.IP[3])
	case net.IPv6len:
		this.ip1 = binary.BigEndian.Uint64(addr.IP[0:])
		this.ip2 = binary.BigEndian.Uint64(addr.IP[8:])
	}
	this.port = uint16(addr.Port)
}

