package rudp

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"hash"
	"io"
	"math/big"
	"sync"
	"testing"
	"time"
)

func Test_RUDP(t *testing.T) {
	tr, err := newTestRUDP()
	if err != nil {
		t.Fatal(err)
	}
	tr.WaitGroup.Add(2)
	go tr.ServerRoutine()
	go tr.ClientRoutine(10240)
	tr.Wait()
	tr.server.Close()
	tr.client.Close()
	//
	if tr.serverError != nil {
		t.Fatal(tr.serverError)
	}
	if tr.clientError != nil {
		t.Fatal(tr.clientError)
	}
	t.Log(tr.serverBytes, tr.clientBytes)
	// 比较传输的数据哈希值
	if !bytes.Equal(tr.clientHash.Sum(nil), tr.serverHash.Sum(nil)) {
		t.FailNow()
	}
}

type testRUDP struct {
	sync.WaitGroup
	server      *RUDP
	client      *RUDP
	serverHash  hash.Hash
	clientHash  hash.Hash
	serverBytes int
	clientBytes int
	serverError error
	clientError error
}

func newTestRUDP() (*testRUDP, error) {
	var err error
	tr := new(testRUDP)
	tr.server, err = Listen("127.0.0.1:30000")
	if err != nil {
		return nil, err
	}
	tr.client, err = Listen("127.0.0.1:20000")
	if err != nil {
		return nil, err
	}
	tr.serverHash = md5.New()
	tr.clientHash = md5.New()
	return tr, nil
}

func (tr *testRUDP) ServerRoutine() {
	defer tr.WaitGroup.Done()
	// tls
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		tr.serverError = err
		return
	}
	template := x509.Certificate{SerialNumber: big.NewInt(time.Now().Unix())}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		tr.serverError = err
		return
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		tr.serverError = err
		return
	}
	// 连接
	conn, err := tr.server.Accept()
	if err != nil {
		tr.serverError = err
		return
	}
	tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{tlsCert}})
	defer tlsConn.Close()
	err = tlsConn.Handshake()
	if err != nil {
		tr.serverError = err
		return
	}
	// 读数据
	buff := make([]byte, 1024)
	for {
		n, err := tlsConn.Read(buff)
		if err != nil {
			if err != io.EOF {
				tr.serverError = err
			}
			return
		}
		tr.serverBytes += n
		tr.serverHash.Write(buff[:n])
	}
}

func (tr *testRUDP) ClientRoutine(multiple int) {
	defer tr.WaitGroup.Done()
	// 等server accept
	time.Sleep(time.Millisecond * 300)
	// 连接
	tr.client.SetConnectRTO(time.Second * 3)
	conn, err := tr.client.Dial(tr.server.Addr().String(), time.Hour)
	if err != nil {
		tr.clientError = err
		return
	}
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	defer tlsConn.Close()
	err = tlsConn.Handshake()
	if err != nil {
		tr.clientError = err
		return
	}
	// 写数据
	// conn.SetWriteBuffer(minMSS * 10)
	buff := make([]byte, 1024)
	for i := 0; i < multiple; i++ {
		mathRand.Read(buff)
		n, err := tlsConn.Write(buff)
		if err != nil {
			tr.clientError = err
			return
		}
		if n != len(buff) {
			fmt.Println(n)
		}
		tr.clientBytes += n
		tr.clientHash.Write(buff[:n])
	}
}
