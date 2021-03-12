package rudp

import (
	"encoding/binary"
	"net"
)

// 连接池的键，因为客户端可能在nat后面，所以加上token
type connKey struct {
	ip1   uint64 // IPV6地址字符数组前64位
	ip2   uint64 // IPV6地址字符数组后64位
	port  uint16 // 端口
	token uint32 // token
}

// 将128位的ip地址（v4的转成v6）的字节分成两个64位整数，加上端口，作为key
func (this *connKey) Init(addr *net.UDPAddr) {
	if len(addr.IP) == net.IPv4len {
		this.ip1 = 0
		this.ip2 = uint64(0xff)<<40 | uint64(0xff)<<32 |
			uint64(addr.IP[0])<<24 | uint64(addr.IP[1])<<16 |
			uint64(addr.IP[2])<<8 | uint64(addr.IP[3])
	} else {
		this.ip1 = binary.BigEndian.Uint64(addr.IP[0:])
		this.ip2 = binary.BigEndian.Uint64(addr.IP[8:])
	}
	this.port = uint16(addr.Port)
}
