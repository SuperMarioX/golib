package net

import (
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	mylog "github.com/buf1024/golib/logging"
)

const (
	EventNone = iota
	EventNewConnection
	EventConnectionError
	EventConnectionClosed
	EventNewConnectionData
	EventProtoError
	EventTimeout
)

const (
	StatusNone = iota
	StatusListenning
	StatusConnected
	StatusBroken
)

type ConnEvent struct {
	EventType int
	Conn      *Connection
	Data      interface{}
}

type Connection struct {
	net    *SimpleNet
	listen *Listener

	id      int64
	status  int64
	conn    net.Conn
	msgChan chan []byte

	localAddr  string
	remoteAddr string
	upTime     time.Time

	proto    IProto // 为了实现多种proto
	UserData interface{}
}

func (c *Connection) Net() *SimpleNet {
	return c.net
}
func (c *Connection) ID() int64 {
	return c.id
}

func (c *Connection) Status() int64 {
	return c.status
}

func (c *Connection) LocalAddress() string {
	return c.localAddr
}

func (c *Connection) RemoteAddress() string {
	return c.remoteAddr
}
func (c *Connection) UpdateTime() time.Time {
	return c.upTime
}

type Listener struct {
	net *SimpleNet

	id     int64
	status int64
	listen net.Listener
	conns  []*Connection

	lockClient sync.Locker

	proto    IProto
	UserData interface{}
}

func (l *Listener) ID() int64 {
	return l.id
}
func (l *Listener) Net() *SimpleNet {
	return l.net
}
func (l *Listener) LocalAddress() string {
	return l.listen.Addr().String()
}

type SimpleNet struct {
	events chan *ConnEvent

	connClient []*Connection
	connServer []*Listener

	lockServer sync.Locker
	lockClient sync.Locker

	nextid  int64
	destroy bool

	log *mylog.Log

	UserData interface{}
}

type IProto interface {
	FilterAccept(conn *Connection) bool
	HeadLen() uint32
	BodyLen(head []byte) (interface{}, uint32, error)
	Parse(head interface{}, body []byte) (interface{}, error)
	Serialize(data interface{}) ([]byte, error)
}

// NewSimpleNet 创建
func NewSimpleNet(log *mylog.Log) *SimpleNet {
	n := &SimpleNet{
		events:     make(chan *ConnEvent, 1024),
		lockServer: &sync.Mutex{},
		lockClient: &sync.Mutex{},
		log:        log,
	}

	return n
}

func SimpleNetDestroy(n *SimpleNet) {
	close(n.events)
	for _, v := range n.connClient {
		n.CloseConn(v)
	}

	for _, v := range n.connServer {
		n.CloseListen(v)
	}
	n.destroy = true
}

func (n *SimpleNet) logMsg(level int, msg string) {
	if n.log != nil {
		switch level {
		case mylog.LevelTrace:
			n.log.Trace("%s", msg)
		case mylog.LevelDebug:
			n.log.Debug("%s", msg)
		case mylog.LevelInformational:
			n.log.Info("%s", msg)
		case mylog.LevelNotice:
			n.log.Notice("%s", msg)
		case mylog.LevelWarning:
			n.log.Warning("%s", msg)
		case mylog.LevelError:
			n.log.Error("%s", msg)
		case mylog.LevelCritical:
			n.log.Critical("%s", msg)
		}
		return
	}
	fmt.Printf("%s", msg)
}

func (n *SimpleNet) syncAddListen(listen *Listener) {
	n.lockServer.Lock()
	defer n.lockServer.Unlock()

	n.connServer = append(n.connServer, listen)

}
func (n *SimpleNet) syncDelListen(listen *Listener) {
	n.lockServer.Lock()
	defer n.lockServer.Unlock()

	for i, v := range n.connServer {
		if v == listen {
			if i == len(n.connServer)-1 {
				n.connServer = n.connServer[:i]
			} else {
				n.connServer = append(n.connServer[:i], n.connServer[i+1:]...)
			}
			break
		}
	}

}

func (n *SimpleNet) syncAddClient(conn *Connection) {
	var connQueue []*Connection

	connQueue = n.connClient
	lock := n.lockClient
	if conn.listen != nil {
		connQueue = conn.listen.conns
		lock = conn.listen.lockClient
	}

	lock.Lock()
	defer lock.Unlock()

	connQueue = append(connQueue, conn)

	if conn.listen != nil {
		conn.listen.conns = connQueue
	} else {
		n.connClient = connQueue
	}
}
func (n *SimpleNet) syncDelClient(conn *Connection) {
	var connQueue []*Connection

	connQueue = n.connClient
	lock := n.lockClient
	if conn.listen != nil {
		connQueue = conn.listen.conns
		lock = conn.listen.lockClient
	}

	lock.Lock()
	defer lock.Unlock()

	var del bool
	for i, v := range connQueue {
		if v == conn {
			if i == len(connQueue)-1 {
				connQueue = connQueue[:i]
			} else {
				connQueue = append(connQueue[:i], connQueue[i+1:]...)
			}
			del = true
			break
		}
	}

	if del {
		if conn.listen != nil {
			conn.listen.conns = connQueue
		} else {
			n.connClient = connQueue
		}
	}
}

func (n *SimpleNet) checkConnErr(count int, err error, conn *Connection) error {
	if err != nil {
		n.logMsg(mylog.LevelError, fmt.Sprintf("conn err = %s\n", err))
		if conn.net.destroy {
			n.logMsg(mylog.LevelError, fmt.Sprintf("net destroy\n"))
			return err
		}
		if conn.status == StatusConnected {
			close(conn.msgChan)
			conn.conn.Close()
			conn.status = StatusBroken

			n.syncDelClient(conn)
		}
		evt := EventConnectionError
		if err == io.EOF {
			evt = EventConnectionClosed
		}
		n.logMsg(mylog.LevelDebug, fmt.Sprintf("event type %d\n", evt))

		// emit EventConnectionError
		event := &ConnEvent{
			EventType: evt,
			Conn:      conn,
			Data:      err,
		}
		n.events <- event
	}
	return err
}
func (n *SimpleNet) handleRead(conn *Connection) {
	defer func() {
		err := recover()
		if err != nil {
			n.logMsg(mylog.LevelError, fmt.Sprintf("handleRead panic: %s\n", err))
		}
	}()
	for {
		headlen := (uint32)(0)
		if conn.proto != nil {
			headlen = conn.proto.HeadLen()
		}
		if headlen <= 0 {
			buf := make([]byte, 1)
			count, err := conn.conn.Read(buf)
			if err = n.checkConnErr(count, err, conn); err != nil {
				return
			}
			n.logMsg(mylog.LevelInformational,
				fmt.Sprintf("read data, count = %d, remoteAddr: = %s\n",
					count, conn.conn.RemoteAddr()))

			// emit
			event := &ConnEvent{
				EventType: EventNewConnectionData,
				Conn:      conn,
				Data:      buf,
			}
			n.events <- event

		} else {
			head := make([]byte, headlen)
			count, err := conn.conn.Read(head)
			if err = n.checkConnErr(count, err, conn); err != nil {
				return
			}
			n.logMsg(mylog.LevelInformational,
				fmt.Sprintf("read data, count = %d, remoteAddr: = %s\n",
					count, conn.conn.RemoteAddr()))
			headmsg, bodylen, err := conn.proto.BodyLen(head)
			if err != nil {
				// emit EventConnectionError
				event := &ConnEvent{
					EventType: EventProtoError,
					Conn:      conn,
					Data:      err,
				}
				n.events <- event
				continue
			}

			body := make([]byte, bodylen)
			count, err = conn.conn.Read(body)
			if err = n.checkConnErr(count, err, conn); err != nil {
				return
			}
			n.logMsg(mylog.LevelInformational,
				fmt.Sprintf("read data, count = %d, remoteAddr: = %s\n",
					count, conn.conn.RemoteAddr()))

			data, err := conn.proto.Parse(headmsg, body)
			if err != nil {
				// emit EventConnectionError
				event := &ConnEvent{
					EventType: EventProtoError,
					Conn:      conn,
					Data:      err,
				}
				n.events <- event
				continue
			}
			// emit EventNewConnectionData
			event := &ConnEvent{
				EventType: EventNewConnectionData,
				Conn:      conn,
				Data:      data,
			}
			n.events <- event
		}
		conn.upTime = time.Now()
	}
}

func (n *SimpleNet) handleWrite(conn *Connection) {
	defer func() {
		err := recover()
		if err != nil {
			n.logMsg(mylog.LevelError,
				fmt.Sprintf("handleWrite panic: %s\n", err))
		}
	}()
	for {
		select {
		case msg, ok := <-conn.msgChan:
			{
				if !ok {
					return
				}
				count, err := conn.conn.Write(msg)
				if err = n.checkConnErr(count, err, conn); err != nil {
					return
				}
				conn.upTime = time.Now()
				n.logMsg(mylog.LevelInformational,
					fmt.Sprintf("send data, count = %d, remoteAddr = %s\n",
						count, conn.conn.RemoteAddr()))
			}
		}
	}
}

func (n *SimpleNet) listening(l *Listener) {
	defer func() {
		err := recover()
		if err != nil {
			n.logMsg(mylog.LevelError,
				fmt.Sprintf("listenning panic: %s\n", err))
		}
	}()
	for {
		newconn, err := l.listen.Accept()
		if err != nil {
			n.logMsg(mylog.LevelError,
				fmt.Sprintf("accept failed, err = %s\n", err))
			if l.status != StatusListenning {
				break
			}
			continue
		}

		conn := &Connection{
			net:        l.net,
			listen:     l,
			id:         atomic.AddInt64(&n.nextid, 1),
			status:     StatusConnected,
			conn:       newconn,
			msgChan:    make(chan []byte, 1024),
			localAddr:  newconn.LocalAddr().String(),
			remoteAddr: newconn.RemoteAddr().String(),
			proto:      l.proto,
			upTime:     time.Now(),
		}

		if conn.proto != nil {
			if !conn.proto.FilterAccept(conn) {
				continue
			}
		}

		n.syncAddClient(conn)

		// emit EventNewConnection
		event := &ConnEvent{
			EventType: EventNewConnection,
			Conn:      conn,
		}
		n.events <- event

		go n.handleRead(conn)
		go n.handleWrite(conn)

	}
}

// Listen 监听网络 addr 为监听地址
func (n *SimpleNet) Listen(addr string, proto IProto) (*Listener, error) {
	listen, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	l := &Listener{
		net: n,

		id:         atomic.AddInt64(&n.nextid, 1),
		status:     StatusListenning,
		listen:     listen,
		lockClient: &sync.Mutex{},

		proto: proto,
	}
	n.syncAddListen(l)

	go n.listening(l)

	return l, nil
}

// Connect 连接服务器器
func (n *SimpleNet) Connect(addr string, proto IProto) (*Connection, error) {
	newconn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	conn := &Connection{
		net:        n,
		id:         atomic.AddInt64(&n.nextid, 1),
		status:     StatusConnected,
		conn:       newconn,
		msgChan:    make(chan []byte, 1024),
		localAddr:  newconn.LocalAddr().String(),
		remoteAddr: newconn.RemoteAddr().String(),
		upTime:     time.Now(),
		proto:      proto,
	}
	n.syncAddClient(conn)

	go n.handleRead(conn)
	go n.handleWrite(conn)

	return conn, nil
}

// PollEvent 事件轮询
func (n *SimpleNet) PollEvent(timeout int) (*ConnEvent, error) {
	t := time.After(time.Millisecond * (time.Duration)(timeout))
	select {
	case event, ok := <-n.events:
		{
			if !ok {
				return nil, fmt.Errorf("SimpleNet destroyed")
			}
			return event, nil
		}
	case <-t:
		{
			evt := &ConnEvent{
				EventType: EventTimeout,
			}
			return evt, nil
		}
	}
}

// SendData 向connection发送数据，如果connection不支持，data为[]byte
func (n *SimpleNet) SendData(conn *Connection, data interface{}) error {
	if conn.status != StatusConnected {
		return fmt.Errorf("not connected connection")
	}
	if conn.proto == nil {
		msg, ok := (data).([]byte)
		if !ok {
			return fmt.Errorf("unexpect data type")
		}
		conn.msgChan <- msg
	} else {
		msg, err := conn.proto.Serialize(data)
		if err != nil {
			return err
		}
		conn.msgChan <- msg
	}
	return nil
}

// CloseConn 关闭连接
func (n *SimpleNet) CloseConn(conn *Connection) error {
	if conn.status == StatusConnected {
		conn.status = StatusBroken
		close(conn.msgChan)
		conn.conn.Close()

		n.syncDelClient(conn)
	}
	return nil
}

// CloseListen 关闭服务器
func (n *SimpleNet) CloseListen(listen *Listener) error {
	if listen.status == StatusListenning {
		for _, v := range listen.conns {
			n.CloseConn(v)
		}
		listen.status = StatusBroken
		listen.listen.Close()
	}

	return nil
}
