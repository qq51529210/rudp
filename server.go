package rudp

import (
	"net"
	"sync"
)

const (
	defaultAcceptBacklog = 64
)

func newServer() *server {
	p := new(server)
	p.accepted = make(chan *conn, defaultAcceptBacklog)
	p.connected = make(map[uint64]*conn)
	p.accepting = make(map[uint32]*conn)
	return p
}

type server struct {
	lock      sync.RWMutex     // 锁
	accepted  chan *conn       // 已经建立连接，等待应用层处理
	connected map[uint64]*conn // 已经建立的连接
	accepting map[uint32]*conn // 正在建立的连接
}

// net.Listener接口
func (this *RUDP) Accept() (net.Conn, error) {
	select {
	case conn, ok := <-this.accepted: // 等待新的连接
		if ok {
			return conn, nil
		}
	case <-this.closeSignal: // 退出信号
	}
	return nil, &net.OpError{
		Op:     "listen",
		Net:    this.conn.LocalAddr().Network(),
		Source: nil,
		Addr:   this.conn.LocalAddr(),
		Err:    errClosed("RUDP"),
	}
}

// net.Listener接口
func (this *RUDP) Close() error {
	// 关闭底层Conn
	err := this.closeNetConn()
	if err != nil {
		return err
	}
	// 通知所有协程退出
	close(this.closeSignal)
	// 等待所有协程退出
	this.waitExit.Wait()
	// 清理资源
	close(this.accepted)
	// 关闭正在建立的连接
	for _, c := range this.accepting {
		c.Close()
	}
	// 关闭已经建立的连接
	for _, c := range this.connected {
		c.Close()
	}
	return nil
}

// net.Listener接口
func (this *RUDP) Addr() net.Addr {
	return this.conn.LocalAddr()
}

// 关闭底层net.UDPConn
func (this *RUDP) closeNetConn() error {
	this.lock.Lock()
	if this.conn == nil {
		this.lock.Unlock()
		return &net.OpError{
			Op:     "Close",
			Net:    this.conn.LocalAddr().Network(),
			Source: nil,
			Addr:   this.conn.LocalAddr(),
			Err:    errClosed("RUDP"),
		}
	}
	this.conn.Close()
	this.conn = nil
	this.lock.Unlock()
	return nil
}
