package rcenter

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 4096
	Handshake      = 1
	HandshakeOk    = 2
)

func checkSameOrigin(r *http.Request) bool {
	origin := r.Header["Origin"]
	if len(origin) == 0 {
		return true
	}
	u, err := url.Parse(origin[0])
	if err != nil {
		return false
	}
	log.Println("origin is ", origin, u.Host, r.Host)
	return u.Host == r.Host
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     checkSameOrigin,
	Subprotocols:    []string{"tunnel-protocol"},
}

func (devConn *DeviceConn) CloseInner(s *ServerHttpd) {
	// if isClosed == 1
	if !atomic.CompareAndSwapInt32(&devConn.isClosed, 0, 1) {
		return
	}
	devConn.mainWs.Close()
	close(devConn.writeMsg)

	if len(devConn.u) > 0 {
		if userMgr, ok := s.bus.users[devConn.u]; ok {
			userMgr.UnregistDevice(devConn.deviceId)
			delete(s.bus.users, devConn.u)
		}
	}
	for k, v := range devConn.wsMap {
		//TODO close userConn
		v.ws.Close()
		close(v.writeMsg)
		delete(devConn.wsMap, k)
	}
}

func (devConn *DeviceConn) Close(s *ServerHttpd) {
	s.bus.CallNoWait(devConn.CloseInner, s)
}

func (devConn *DeviceConn) write(mt int, payload []byte) error {
	devConn.mainWs.SetWriteDeadline(time.Now().Add(writeWait))
	return devConn.mainWs.WriteMessage(mt, payload)
}

func (devConn *DeviceConn) RegistDeviceInner(s *ServerHttpd, user string, pass string) {
	if usrMgr, ok := s.bus.users[user]; ok {
		usrMgr.RegistDevice(devConn)
	} else {
		usrMgr = NewUser(s.bus, user, pass)
		s.bus.users[user] = usrMgr
		usrMgr.RegistDevice(devConn)
	}
}

func (devConn *DeviceConn) registDev(s *ServerHttpd, user string, pass string) {
	s.bus.Call(devConn.RegistDeviceInner, s, user, pass)
}

func (devConn *DeviceConn) writePump(s *ServerHttpd) {
	defer devConn.Close(s)

	for {
		select {
		case msg, ok := <-devConn.writeMsg:
			if ok {
				log.Println("begin write back message", len(msg))
				err := devConn.write(websocket.BinaryMessage, msg)
				if nil != err {
					log.Println("write message error ", err.Error())
					return
				}
			} else {
				log.Println("got write channel error ")
			}
		}
	}
}

//TODO http://stackoverflow.com/questions/23174362/packing-struct-in-golang-in-bytes-to-talk-with-c-application
func (devConn *DeviceConn) readPump(s *ServerHttpd) {
	defer devConn.Close(s)

	devConn.mainWs.SetReadLimit(maxMessageSize)
	devConn.mainWs.SetReadDeadline(time.Now().Add(pongWait))
	devConn.mainWs.SetPingHandler(func(string) error {
		devConn.mainWs.SetReadDeadline(time.Now().Add(pongWait))
		s.context.Logger.Debug("websocket got ping")
		return nil
	})

	for {
		_, b, err := devConn.mainWs.ReadMessage()
		if err != nil {
			s.context.Logger.Info("read message error %v\n", err)
			break
		}

		var mHeader MessageHeader
		if len(b) < int(unsafe.Sizeof(mHeader)) {
			s.context.Logger.Error("ignore error package %v\n", err)
			continue
		}

		//parse message header
		buf := bytes.NewBuffer(b)
		//dec := gob.NewDecoder(buffer)
		if nil != binary.Read(buf, binary.BigEndian, &mHeader) {
			s.context.Logger.Error("error read header %v\n", err)
			continue
		}

		if mHeader.Proto == 1 && mHeader.MType == Handshake {
			var handshake MsgHandshake
			if nil != binary.Read(buf, binary.BigEndian, &handshake) {
				s.context.Logger.Error("error read info %v\n", err)
				continue
			}
			user := string(handshake.Username[:handshake.Ulen])
			pass := string(handshake.Pass[:handshake.Plen])
			deviceId := string(handshake.DeviceId[:handshake.Dlen])
			devConn.u = user
			devConn.deviceId = deviceId
			devConn.registDev(s, user, pass)

			mHeader.MType = HandshakeOk
			var buf2 bytes.Buffer
			writer := bufio.NewWriter(&buf2)
			binary.Write(writer, binary.BigEndian, &mHeader)
			writer.Flush()
			devConn.writeMsg <- buf2.Bytes()
		} else {
			msg := &Message{mHeader, b[int(unsafe.Sizeof(mHeader)):]}
			s.bus.resp <- msg
		}

	}
}

func serveMainChannel(s *ServerHttpd, w http.ResponseWriter, r *http.Request) (int, error) {
	//log.Println("in serveMainChannel")
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return 404, fmt.Errorf("Upgrade error")
	}
	s.context.Logger.Info("Got new connection\n")

	devConn := &DeviceConn{mainWs: ws, wsMap: make(map[string]*UserConn), writeMsg: make(chan []byte, 100), isClosed: 0}
	go devConn.writePump(s)
	devConn.readPump(s)

	return 200, nil
}
