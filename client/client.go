package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dnapier/bayeux-go/server"
	"github.com/dnapier/bayeux-go/shared"
)

const (
	defaultHost          = "localhost:4001/bayeux"
	defaultKeepAliveSecs = 30
)

const ( // iota is reset to 0
	stateWSDisconnected     = iota // == 0
	stateWSConnected        = iota
	stateBayeuxDisconnected = iota
	stateBayeuxConnected    = iota
)

// A Subscription is represents a subscription to a channel by the client
// Each sub has...
//   - a messageChan which is recieve any messages sent to it
type Subscription struct {
	channel   string // the channel path
	connected bool   // a connected indicator to indicate the state of the sub on the bayeux server
}

// A Message is a piece of data sent to a channel from the bayeux server
type Message struct {
	Channel string
	Data    map[string]interface{}
	Ext     map[string]interface{}
}

// A Client is a connection and a set of subscriptions
// TODO: remap MessageChan to a set of subscription message channels one per active subscription
type Client struct {
	Host          string
	MessageChan   chan Message // Messages recv'd by the client will be sent to this message channel
	conn          *Connection
	bayeuxState   int
	readyChan     chan bool
	clientID      string
	messageNumber int
	subscriptions []*Subscription
	keepAliveSecs int
	keepAliveChan chan bool
	core          shared.Core
}

// NewClient creates a new client for a given host and returns a pointer to it
func NewClient(host string) *Client {
	if len(host) == 0 {
		host = defaultHost
	}

	return &Client{
		Host:          host,
		MessageChan:   make(chan Message, 100),
		bayeuxState:   stateWSDisconnected,
		messageNumber: 0,
		keepAliveSecs: defaultKeepAliveSecs,
		keepAliveChan: make(chan bool),
		core:          shared.Core{},
	}
}

// SetKeepAliveIntervalSeconds sets the keep alive interval for the client
func (f *Client) SetKeepAliveIntervalSeconds(secs int) {
	f.keepAliveSecs = secs
}

// Start spins up the main client loop
func (f *Client) Start(ready chan bool) error {
	err := f.connectToServer()
	if err != nil {
		return err
	}

	// kick off the connection handshake
	f.readyChan = ready
	f.handshake()
	return nil
}

// ReaderDisconnect is called by the connection handler if the reader connection is dropped by the loss of a server connection
func (f *Client) ReaderDisconnect() {
	f.readyChan <- false
}

// Write sends a message to the bayeux server over the websocket connection
func (f *Client) Write(msg string) error {
	f.conn.send <- []byte(msg)
	return nil
}

// HandleMessage parses and interprets a bayeux message response
func (f *Client) HandleMessage(message []byte) error {
	// parse the bayeux message and interprets the logic to set client state appropriately
	var resp []server.BayeuxResponse
	if err := json.Unmarshal(message, &resp); err != nil {
		return fmt.Errorf("error marshalling json: %s", err)
	}

	for i := range resp {
		switch resp[i].Channel {
		case shared.ChannelHandshake:
			f.clientID = resp[i].ClientID
			f.connect() // send bayeux connect message
			f.bayeuxState = stateBayeuxConnected
			f.readyChan <- true

		case shared.ChannelConnect:
			//fmt.Println("Recv'd connect response")

		case shared.ChannelDisconnect:
			f.bayeuxState = stateBayeuxDisconnected
			f.disconnectFromServer()

		case shared.ChannelSubscribe:
			f.updateSubscription(resp[i].Subscription, resp[i].Successful)

		case shared.ChannelUnsubscribe:
			if resp[i].Successful {
				f.removeSubscription(resp[i].Subscription)
			}

		default:
			if resp[i].Data != nil {
				if resp[i].ClientID == f.clientID {
					return nil
				}
				var data map[string]interface{}
				var ext map[string]interface{}

				if resp[i].Data != nil {
					data = resp[i].Data.(map[string]interface{})
				}

				if resp[i].Ext != nil {
					ext = resp[i].Ext.(map[string]interface{})
				}

				// tell the client we got a message on a channel
				go func(d, e map[string]interface{}) {
					select {
					case f.MessageChan <- Message{Channel: resp[i].Channel, Data: d, Ext: e}:
						return
					case <-time.After(100 * time.Millisecond):
						return
					}
				}(data, ext)
			}
		}
	}

	return nil
}

// Subscribe sends a message to the server to subscribe to the given channel
func (f *Client) Subscribe(channel string) error {
	if len(channel) == 0 {
		return errors.New("Channel must have a value.")
	}
	f.addSubscription(channel)
	return f.subscribe(channel)
}

// Unsubscribe sends a message to the server to unsubscribe from the given channel
func (f *Client) Unsubscribe(channel string) error {
	if len(channel) == 0 {
		return errors.New("Channel must have a value.")
	}
	return f.unsubscribe(channel)
}

// Publish sends a message to the server to publish a message
func (f *Client) Publish(channel string, data map[string]interface{}) error {
	return f.publish(channel, data)
}

// Disconnect disconnects from the Bayeux server
func (f *Client) Disconnect() {
	f.disconnect()
}

/********************************************************
 * 				Bayeux protocol messages  				*
 ********************************************************/

/*
type BayeuxResponse struct {
	Channel                  string            `json:"channel,omitempty"`
	Successful               bool              `json:"successful,omitempty"`
	Version                  string            `json:"version,omitempty"`
	SupportedConnectionTypes []string          `json:"supportedConnectionTypes,omitempty"`
	ClientID                 string            `json:"clientID,omitempty"`
	Advice                   map[string]string `json:"advice,omitempty"`
	Subscription             string            `json:"subscription,omitempty"`
	Error                    string            `json:"error,omitempty"`
	Id                       string            `json:"id,omitempty"`
	Data                     interface{}       `json:"data,omitempty"`
}
*/

// addSubscription adds a subscription to the client
func (f *Client) addSubscription(channel string) {
	c := Subscription{channel: channel, connected: false}
	f.subscriptions = append(f.subscriptions, &c)
}

// removeSubscription removes a subscription from the client
func (f *Client) removeSubscription(channel string) {
	for i, sub := range f.subscriptions {
		if channel == sub.channel {
			f.subscriptions = append(f.subscriptions[:i], f.subscriptions[i+1:]...)
		}
	}
}

// updateSubscription updates the connected state of a subscription
func (f *Client) updateSubscription(channel string, connected bool) {
	s := f.getSubscription(channel)
	s.connected = connected
}

// getSubscription returns a subscription for a given channelj
func (f *Client) getSubscription(channel string) *Subscription {
	for i, sub := range f.subscriptions {
		if channel == sub.channel {
			return f.subscriptions[i]
		}
	}
	return nil
}

// resubscribeSubscriptions resubscribes all subscriptions to the server
func (f *Client) resubscribeSubscriptions() {
	for _, sub := range f.subscriptions {
		fmt.Println("resubscribe: ", sub.channel)
		if err := f.subscribe(sub.channel); err != nil {
			fmt.Println("Error: could not resubscribe to channels: ", err)
		}
	}
}

// connectToServer opens the websocket connection to the bayeux server and initialize the client state
func (f *Client) connectToServer() error {
	u, err := url.Parse("ws://" + f.Host)
	if err != nil {
		return err
	}

	c, resp, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}

	f.bayeuxState = stateWSConnected

	if resp != nil {
		fmt.Println("Resp: ", resp)
	}

	conn := NewConnection(c)

	f.conn = conn
	f.conn.writerConnected = true
	f.conn.readerConnected = true
	go conn.writer()
	go conn.reader(f)

	// close keep alive channel to stop any running keep alive
	close(f.keepAliveChan)
	f.keepAliveChan = make(chan bool)
	go f.keepAlive()

	return nil
}

// keepalive opens and run loop on a ticker and sends keepalive messages to the server
func (f *Client) keepAlive() {
	c := time.Tick(time.Duration(f.keepAliveSecs) * time.Second)
	for {
		select {
		case _, ok := <-f.keepAliveChan:
			if !ok {
				fmt.Println("exit keep alive")
				return
			}
		case <-c:
			fmt.Println("Send keep-alive: ", time.Now())
			f.connect()
		}

	}
}

// disconnectFromServer closes the websocket connection and set the bayeux client state
func (f *Client) disconnectFromServer() {
	f.bayeuxState = stateWSDisconnected
	f.conn.exit <- true
	if err := f.conn.ws.Close(); err != nil {
		fmt.Println("Error closing websocket: ", err)
	}
}

func (f *Client) handshake() {
	message := server.BayeuxResponse{Channel: shared.ChannelHandshake, Version: "1.0", SupportedConnectionTypes: []string{"websocket"}}
	if err := f.write(message); err != nil {
		fmt.Println("Error generating handshake message")
	}
}

// connect to Bayeux and send a connect message
func (f *Client) connect() {
	message := server.BayeuxResponse{Channel: shared.ChannelConnect, ClientID: f.clientID, ConnectionType: "websocket"}
	if err := f.write(message); err != nil {
		fmt.Println("Error generating connect message")
	}
}

// disconnect sends the disconnect message to the server
func (f *Client) disconnect() {
	message := server.BayeuxResponse{Channel: shared.ChannelDisconnect, ClientID: f.clientID}
	if err := f.write(message); err != nil {
		fmt.Println("Error generating connect message")
	}
}

// subscribe the client to a channel
func (f *Client) subscribe(channel string) error {
	message := server.BayeuxResponse{Channel: shared.ChannelSubscribe, ClientID: f.clientID, Subscription: channel}
	if err := f.write(message); err != nil {
		return fmt.Errorf("error generating subscribe message: %s", err)
	}
	return nil
}

// unsubscribe from a channel
func (f *Client) unsubscribe(channel string) error {
	message := server.BayeuxResponse{Channel: shared.ChannelUnsubscribe, ClientID: f.clientID, Subscription: channel}
	if err := f.write(message); err != nil {
		return fmt.Errorf("error generating unsubscribe message: %s", err)
	}
	return nil
}

// publish sends a message to a channel
func (f *Client) publish(channel string, data map[string]interface{}) error {
	message := server.BayeuxResponse{Channel: channel, ClientID: f.clientID, ID: f.core.NextMessageID(), Data: data}
	if err := f.write(message); err != nil {
		return fmt.Errorf("error generating unsubscribe message: %s", err)
	}
	return nil
}

// writeMessage encodes the json and send the message over the wire.
func (f *Client) write(message server.BayeuxResponse) error {
	if !f.conn.Connected() {
		// reconnect
		fmt.Println("RECONNECT")
		cerr := f.connectToServer()
		if cerr != nil {
			return cerr
		}
		if !f.conn.Connected() {
			return errors.New("Not Connected, Reconnect Failed.")
		}

	}

	j, err := json.Marshal(message)
	if err != nil {
		return err
	}

	return f.Write(string(j))
}
