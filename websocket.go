package stream_chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

type ConnectRequest struct {
	ServerDeterminesID bool `json:"server_determines_connection_id"`
	UserDetails        User `json:"user_details" validate:"required,dive"`
}

type WebsocketConn struct {
	id      atomic.Value // from hello message
	handler EventHandler
	url     string

	context.Context
	context.CancelFunc

	net.Conn

	err error // sticky error

	maxReconnectAttempts int
	started              uint32
}

func NewWebsocketConn(url string, handler EventHandler, maxReconnectAttempts int) *WebsocketConn {
	ctx, cancel := context.WithCancel(context.Background())
	return &WebsocketConn{
		Context:              ctx,
		CancelFunc:           cancel,
		url:                  url,
		Conn:                 nil,
		handler:              handler,
		maxReconnectAttempts: maxReconnectAttempts,
	}
}

func (wsConn *WebsocketConn) Connect() error {
	conn, _, _, err := ws.DefaultDialer.Dial(wsConn.Context, wsConn.url)
	if err != nil {
		return err
	}
	wsConn.Conn = conn

	var buf bytes.Buffer
	if err := wsConn.readEvent(&buf); err != nil {
		return err
	}

	var event Event
	if err := json.NewDecoder(&buf).Decode(&event); err != nil {
		return err
	}
	wsConn.id.Store(event.ConnectionID)
	return nil
}

//
func (wsConn *WebsocketConn) monitorHealth() error {
	errCh := make(chan error)

	run := func() {
		errCh <- wsConn.run()
	}
	go run()
	var attemptNumber int
	for {
		select {
		case <-errCh: // either read or write return an error
		// called on disconnect
		case <-wsConn.Context.Done():
			return wsConn.Context.Err()
		default:
			attemptNumber++
			for i := 0; i < wsConn.maxReconnectAttempts; i++ {
				wsConn.Context, wsConn.CancelFunc = context.WithCancel(context.Background())
				if err := wsConn.Connect(); err != nil {
					log.Println("failed to reconnect ", err)
					continue
				}
				go run()
			}
			return fmt.Errorf("failed to reconnect:%v ")
		}
	}
}
func (wsConn *WebsocketConn) run() error {
	defer wsConn.Close()
	go wsConn.writePingLoop()
	return wsConn.readLoop()
}

func (wsConn *WebsocketConn) ID() string {
	return wsConn.id.Load().(string)
}

func (wsConn *WebsocketConn) readLoop() error {
	var buf bytes.Buffer
	for {
		select {
		case <-wsConn.Done():
			return wsConn.Err()
		default:
		}
		buf.Reset()
		if err := wsConn.readEvent(&buf); err != nil {
			return err
		}
		var event Event
		if err := json.NewDecoder(&buf).Decode(&event); err != nil {
			log.Println(err)
			return err
		}
		switch event.Type {
		case EventHealthCheck:
			wsConn.id.Store(event.ConnectionID)
		default:
			wsConn.handler(&event)
		}
	}
}

var ping = []byte("ping")

func (wsConn *WebsocketConn) writePingLoop() error {
	for {
		select {
		case <-wsConn.Done():
			return wsConn.Err()
		default:
		}

		if err := wsConn.SetWriteDeadline(time.Now().Add(time.Second * 8)); err != nil {
			return err
		}
		if err := wsutil.WriteClientBinary(wsConn, ping); err != nil {
			return err
		}
		time.Sleep(time.Second * 28)
	}
}

func (wsConn *WebsocketConn) readEvent(buffer *bytes.Buffer) error {
	if err := wsConn.SetReadDeadline(time.Now().Add(time.Second * 35)); err != nil {
		return err
	}
	controlHandler := wsutil.ControlFrameHandler(wsConn, ws.StateClientSide)

	rd := wsutil.Reader{
		Source:          wsConn,
		State:           ws.StateClientSide,
		CheckUTF8:       true,
		SkipHeaderCheck: false,
		OnIntermediate:  controlHandler,
	}

	for {
		hdr, err := rd.NextFrame()
		if err != nil {
			return err
		}
		if hdr.OpCode.IsControl() {
			if err := controlHandler(hdr, &rd); err != nil {
				return err
			}
			continue
		}
		if hdr.OpCode&ws.OpText == 0 {
			if err := rd.Discard(); err != nil {
				return err
			}
			continue
		}
		_, err = buffer.ReadFrom(&rd)
		return err
	}
}
