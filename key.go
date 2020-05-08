package rudp

import (
	"encoding/binary"
	"net"
)

const v4InV6Prefix = uint64(0xff)<<40 | uint64(0xff)<<32 // V4地址转V6整数时用

// 表示一个IPV6:PORT地址。
type addressKey struct {
	ip1  uint64 // IPV6转整数前64位
	ip2  uint64 // IPV6转整数后64位
	port uint16 // 端口
}

// 从net.UDPAddr赋值。
func (this *addressKey) Init(a *net.UDPAddr) {
	switch len(a.IP) {
	case net.IPv4len:
		this.ip1 = 0
		this.ip2 = v4InV6Prefix | uint64(a.IP[0])<<24 |
			uint64(a.IP[1])<<16 | uint64(a.IP[2])<<8 | uint64(a.IP[3])
	case net.IPv6len:
		this.ip1 = binary.BigEndian.Uint64(a.IP[0:])
		this.ip2 = binary.BigEndian.Uint64(a.IP[8:])
	}
	this.port = uint16(a.Port)
}

// 表示正在创建连接的键。
type dialKey struct {
	addressKey        // 地址
	cToken     uint32 // 客户端32位随机码
}
