package rudp

import (
	"net"
	"time"
)

// 一个虚拟的连接对象，实现了net.Conn接口
type Conn struct {
	lAddr, rAddr net.Addr  // 本地/对端地址
	rto, wto    time.Time // io读/写超时时间
}

func (this *Conn) Read(b []byte) (int, error) {
	return 0, nil
}

func (this *Conn) Write(b []byte) (int, error) {
	return 0, nil
}

func (this *Conn) Close() error {
	return nil
}

func (this *Conn) LocalAddr() net.Addr {
	return this.lAddr
}

func (this *Conn) RemoteAddr() net.Addr {
	return this.rAddr
}

func (this *Conn) SetDeadline(t time.Time) error {
	this.rto=t
	this.wto=t
	return nil
}

func (this *Conn) SetReadDeadline(t time.Time) error {
	this.rto=t
	return nil
}

func (this *Conn) SetWriteDeadline(t time.Time) error {
	this.wto=t
	return nil
}
