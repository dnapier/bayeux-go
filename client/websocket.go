package client

import (
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

/*
 * Initial constants based on websocket example code from github.com/gorilla/websocket/blob/master/examples/chat/conn.go
 */
const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 512
)

// A BayeuxHandler is responsible for parsing bayeux messages
type BayeuxHandler interface {
	HandleMessage(message []byte) error
	ReaderDisconnect()
}

// A Connection is a websocket connnection and state
type Connection struct {
	ws              *websocket.Conn
	readerConnected bool
	writerConnected bool
	send            chan []byte
	exit            chan bool
}

// NewConnection instantiates and returns a new Connection object
func NewConnection(ws *websocket.Conn) *Connection {
	return &Connection{send: make(chan []byte, 256), ws: ws, exit: make(chan bool)}
}

// Connected returns a bool indicating the connection state of both the reader and writer on the connection
func (c *Connection) Connected() bool {
	return c.readerConnected && c.writerConnected
}

// reader reads messages from the websocket connection
func (c *Connection) reader(f BayeuxHandler) {
	c.readerConnected = true

	defer func() {
		if err := c.ws.Close(); err != nil {
			fmt.Println("READ ERROR: ", err)
		}
		c.readerConnected = false
		f.ReaderDisconnect()
	}()
	c.ws.SetReadLimit(maxMessageSize)
	if err := c.ws.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		fmt.Println("READ ERROR: ", err)
	}
	c.ws.SetPongHandler(
		func(string) error {
			return c.ws.SetReadDeadline(time.Now().Add(pongWait))
		},
	)

	for {
		_, message, err := c.ws.ReadMessage()
		if err != nil {
			fmt.Println("READ ERROR: ", err)
			break
		}

		if err = f.HandleMessage(message); err != nil {
			fmt.Println("READ ERROR: ", err)
			break
		}
	}

	fmt.Println("reader exited.")
}

// write messages to the websocket connection
func (c *Connection) write(mt int, payload []byte) error {
	if err := c.ws.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		return fmt.Errorf("could not set write deadline: %v", err)
	}
	return c.ws.WriteMessage(mt, payload)
}

// writer starts a loop that writes messages to the websocket connection
func (c *Connection) writer() {
	c.writerConnected = true

	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		if err := c.ws.Close(); err != nil {
			fmt.Println("WRITE ERROR: ", err)
		}
		c.writerConnected = false
	}()

	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				if err := c.write(websocket.CloseMessage, []byte{}); err != nil {
					fmt.Println("could not write close message to websocket: ", err)
				}
				return
			}
			if err := c.write(websocket.TextMessage, message); err != nil {
				fmt.Println("could not write text message to websocket: ", err)
				return
			}
		case <-ticker.C:
			if err := c.write(websocket.PingMessage, []byte{}); err != nil {
				fmt.Println("could not write ping message to websocket: ", err)
				return
			}
		case <-c.exit:
			fmt.Println("exiting writer...")
			return
		}
	}
}
