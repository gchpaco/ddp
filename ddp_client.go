package ddp

import (
	"fmt"
	"io"
	"time"

	"golang.org/x/net/websocket"
)

// Client represents a DDP client connection. The DDP client establish a DDP
// session and acts as a message pump for other tools.
type Client struct {
	// HeartbeatInterval is the time between heartbeats to send
	HeartbeatInterval time.Duration
	// HeartbeatTimeout is the time for a heartbeat ping to timeout
	HeartbeatTimeout time.Duration
	// ReconnectInterval is the time between reconnections on bad connections
	ReconnectInterval time.Duration

	// reconnects in the number of reconnections the client has made
	reconnects int64

	// session contains the DDP session token (can be used for reconnects and debugging).
	session string
	// version contains the negotiated DDP protocol version in use.
	version string
	// serverID the cluster node ID for the server we connected to
	serverID string
	// ws is the underlying websocket being used.
	ws *websocket.Conn
	// url the URL the websocket is connected to
	url string
	// origin is the origin for the websocket connection
	origin string
	// inbox is an incoming message channel
	inbox chan map[string]interface{}
	// errors is an incoming errors channel
	errors chan error
	// pingTimer is a timer for sending regular pings to the server
	pingTimer *time.Timer
	// pings tracks inflight pings based on each ping ID.
	pings map[string][]*pingTracker
	// calls tracks method invocations that are still in flight
	calls map[string]*Call
	// subs tracks active subscriptions. Map contains name->args
	subs map[string]*Call
	// collections contains all the collections currently subscribed
	collections map[string]Collection

	// idManager tracks IDs for ddp messages
	idManager
}

// NewClient creates a default client (using an internal websocket) to the
// provided URL using the origin for the connection. The client will
// automatically connect, upgrade to a websocket, and establish a DDP
// connection session before returning the client. The client will
// automatically and internally handle heartbeats and reconnects.
//
// TBD create an option to use an external websocket (aka htt.Transport)
// TBD create an option to substitute heartbeat and reconnect behavior (aka http.Tranport)
// TBD create an option to hijack the connection (aka http.Hijacker)
// TBD create profiling features (aka net/http/pprof)
func NewClient(url, origin string) (*Client, error) {
	ws, err := websocket.Dial(url, "", origin)
	if err != nil {
		return nil, err
	}
	c := &Client{
		HeartbeatInterval: 45 * time.Second, // Meteor impl default + 10 (we ping last)
		HeartbeatTimeout:  15 * time.Second, // Meteor impl default
		ReconnectInterval: 5 * time.Second,
		collections:       map[string]Collection{},
		ws:                ws,
		url:               url,
		origin:            origin,
		inbox:             make(chan map[string]interface{}, 100),
		errors:            make(chan error, 100),
		pings:             map[string][]*pingTracker{},
		calls:             map[string]*Call{},
		subs:              map[string]*Call{},

		idManager: *newidManager(),
	}

	// We spin off an inbox processing goroutine
	go c.inboxManager()

	// Start DDP connection
	c.start(ws, NewConnect())

	return c, nil
}

// Session returns the negotiated session token for the connection.
func (c *Client) Session() string {
	return c.session
}

// Version returns the negotiated protocol version in use by the client.
func (c *Client) Version() string {
	return c.version
}

// Reconnect attempts to reconnect the client to the server on the existing
// DDP session.
//
// TODO needs a reconnect backoff so we don't trash a down server
// TODO reconnect should not allow more reconnects while a reconnection is already in progress.
func (c *Client) Reconnect() {

	c.Close()

	c.reconnects++

	// Reconnect
	ws, err := websocket.Dial(c.url, "", c.origin)
	if err != nil {
		log.WithField("target", c.url).WithField("origin", c.origin).WithError(err).Warn("Dial error")
		// Reconnect again after set interval
		time.AfterFunc(c.ReconnectInterval, c.Reconnect)
		return
	}

	c.start(ws, NewReconnect(c.session))

	// --------------------------------------------------------------------
	// We resume inflight or ongoing subscriptions - we don't have to wait
	// for connection confirmation (messages can be pipelined).
	// --------------------------------------------------------------------

	// Send calls that haven't been confirmed - may not have been sent
	// and effects should be idempotent
	for _, call := range c.calls {
		log.WithField("method", call.ServiceMethod).Info("resending inflight method")
		c.Send(NewMethod(call.ID, call.ServiceMethod, call.Args))
	}

	// Resend subscriptions and patch up collections
	for _, sub := range c.subs {
		log.WithField("method", sub.ServiceMethod).Info("restarting active subscription")
		c.Send(NewSub(sub.ID, sub.ServiceMethod, sub.Args))
	}
	// Patching up the collections right now is just resetting them. There
	// must be a better way but this is quick and works.
	for _, collection := range c.collections {
		collection.Reset()
	}
}

// Subscribe subscribes to data updates.
func (c *Client) Subscribe(subName string, args []interface{}, done chan *Call) *Call {
	call := new(Call)
	call.ID = c.newID()
	call.ServiceMethod = subName
	call.Args = args
	call.Owner = c
	if done == nil {
		done = make(chan *Call, 10) // buffered.
	} else {
		// If caller passes done != nil, it must arrange that
		// done has enough buffer for the number of simultaneous
		// RPCs that will be using that channel.  If the channel
		// is totally unbuffered, it's best not to run at all.
		if cap(done) == 0 {
			log.Panic("ddp.rpc: done channel is unbuffered")
		}
	}
	call.Done = done
	c.subs[call.ID] = call

	// Save this subscription to the client so we can reconnect
	subArgs := make([]interface{}, len(args))
	copy(subArgs, args)

	c.Send(NewSub(call.ID, subName, args))

	return call
}

// Sub sends a synchronous subscription request to the server.
func (c *Client) Sub(subName string, args []interface{}) error {
	call := <-c.Subscribe(subName, args, make(chan *Call, 1)).Done
	return call.Error
}

// Go invokes the function asynchronously.  It returns the Call structure representing
// the invocation.  The done channel will signal when the call is complete by returning
// the same Call object.  If done is nil, Go will allocate a new channel.
// If non-nil, done must be buffered or Go will deliberately crash.
//
// Go and Call are modeled after the standard `net/rpc` package versions.
func (c *Client) Go(serviceMethod string, args []interface{}, done chan *Call) *Call {

	call := new(Call)
	call.ID = c.newID()
	call.ServiceMethod = serviceMethod
	call.Args = args
	call.Owner = c
	if done == nil {
		done = make(chan *Call, 10) // buffered.
	} else {
		// If caller passes done != nil, it must arrange that
		// done has enough buffer for the number of simultaneous
		// RPCs that will be using that channel.  If the channel
		// is totally unbuffered, it's best not to run at all.
		if cap(done) == 0 {
			log.Panic("ddp.rpc: done channel is unbuffered")
		}
	}
	call.Done = done
	c.calls[call.ID] = call

	c.Send(NewMethod(call.ID, serviceMethod, args))

	return call
}

// Call invokes the named function, waits for it to complete, and returns its error status.
func (c *Client) Call(serviceMethod string, args []interface{}) (interface{}, error) {
	call := <-c.Go(serviceMethod, args, make(chan *Call, 1)).Done
	return call.Reply, call.Error
}

// Ping sends a heartbeat signal to the server. The Ping doesn't look for
// a response but may trigger the connection to reconnect if the ping timesout.
// This is primarily useful for reviving an unresponsive Client connection.
func (c *Client) Ping() {
	c.PingPong(c.newID(), c.HeartbeatTimeout, func(err error) {
		if err != nil {
			// Is there anything else we should or can do?
			go c.Reconnect()
		}
	})
}

// PingPong sends a heartbeat signal to the server and calls the provided
// function when a pong is received. An optional id can be sent to help
// track the responses - or an empty string can be used. It is the
// responsibility of the caller to respond to any errors that may occur.
func (c *Client) PingPong(id string, timeout time.Duration, handler func(error)) {
	err := c.Send(NewPing(id))
	if err != nil {
		handler(err)
		return
	}
	pings, ok := c.pings[id]
	if !ok {
		pings = make([]*pingTracker, 0, 5)
	}
	tracker := &pingTracker{handler: handler, timeout: timeout, timer: time.AfterFunc(timeout, func() {
		handler(fmt.Errorf("ping timeout"))
	})}
	c.pings[id] = append(pings, tracker)
}

// Send transmits messages to the server. The msg parameter must be json
// encoder compatible.
func (c *Client) Send(msg interface{}) error {
	log.WithField("message", msg).Debug("send")
	if c.ws == nil {
		return fmt.Errorf("Tried to send message on a nil socket")
	} else {
		return websocket.JSON.Send(c.ws, msg)
	}
}

// Close implements the io.Closer interface.
func (c *Client) Close() {
	// Shutdown out all outstanding pings
	c.pingTimer.Stop()
	// Close websocket
	if c.ws != nil {
		c.ws.Close()
		c.ws = nil
	}
}

// ResetStats resets the statistics for the client.
func (c *Client) ResetStats() {
	c.reconnects = 0
}

// CollectionByName retrieves a collection by it's name.
func (c *Client) CollectionByName(name string) Collection {
	collection, ok := c.collections[name]
	if !ok {
		collection = NewCollection(name)
		c.collections[name] = collection
	}
	return collection
}

// CollectionByNameWithDefault retrieves a collection by it's name,
// and if one did not exist defaults to the one returned by the given
// function.
func (c *Client) CollectionByNameWithDefault(name string, makeDefault func(string) Collection) Collection {
	collection, ok := c.collections[name]
	if !ok {
		collection = makeDefault(name)
		c.collections[name] = collection
	}
	return collection
}

// start starts a new client connection on the provided websocket
func (c *Client) start(ws *websocket.Conn, connect *Connect) {
	c.ws = ws

	// We spin off an inbox stuffing goroutine
	go c.inboxWorker(ws)

	c.Send(connect)
}

// inboxManager pulls messages from the inbox and routes them to appropriate
// handlers.
func (c *Client) inboxManager() {
	for {
		select {
		case msg := <-c.inbox:
			log.WithField("message", msg).Info("inbox")
			// Message!
			mtype, ok := msg["msg"]
			if ok {
				switch mtype.(string) {

				// Connection management
				case "connected":
					c.version = "1" // Currently the only version we support
					c.session = msg["session"].(string)
					// Start automatic heartbeats
					c.pingTimer = time.AfterFunc(c.HeartbeatInterval, func() {
						if c.ws != nil {
							c.Ping()
							c.pingTimer.Reset(c.HeartbeatInterval)
						}
					})
				case "failed":
					log.WithField("version", msg["version"]).Fatal("IM Failed to connect, we only support version 1")

				// Heartbeats
				case "ping":
					// We received a ping - need to respond with a pong
					id, ok := msg["id"]
					if ok {
						c.Send(NewPong(id.(string)))
					} else {
						c.Send(NewPong(""))
					}
				case "pong":
					// XXX WEIRD
					// We received a pong - we can clear the ping tracker and call its handler
					id, ok := msg["id"]
					var key string
					if ok {
						key = id.(string)
					}
					pings, ok := c.pings[key]
					if ok && len(pings) > 0 {
						ping := pings[0]
						pings = pings[1:]
						if len(key) == 0 || len(pings) > 0 {
							c.pings[key] = pings
						}
						ping.timer.Stop()
						ping.handler(nil)
					}

				// Live Data
				case "nosub":
					log.WithField("message", msg).Info("Subscription returned a nosub error")
					// Clear related subscriptions=
					sub, ok := msg["id"]
					if ok {
						delete(c.subs, sub.(string))
					}
				case "ready":
					// Run 'done' callbacks on all ready subscriptions
					subs, ok := msg["subs"]
					if ok {
						for _, sub := range subs.([]interface{}) {
							call, ok := c.subs[sub.(string)]
							if ok {
								call.done()
							}
						}
					}
				case "added":
					c.collectionBy(msg).Added(msg)
				case "changed":
					c.collectionBy(msg).Changed(msg)
				case "removed":
					c.collectionBy(msg).Removed(msg)
				case "addedBefore":
					c.collectionBy(msg).AddedBefore(msg)
				case "movedBefore":
					c.collectionBy(msg).MovedBefore(msg)

				// RPC
				case "result":
					id, ok := msg["id"]
					if ok {
						call := c.calls[id.(string)]
						if call != nil {
							delete(c.calls, id.(string))
							e, ok := msg["error"]
							if ok {
								call.Error = fmt.Errorf(e.(string))
							} else {
								call.Reply = msg["result"]
							}
							call.done()
						}
					}
				case "updated":
					// We currently don't do anything with updated status

				default:
					// Ignore?
					log.WithField("message", msg).Warn("Server sent unexpected message")
				}
			} else {
				// Current Meteor server sends an undocumented DDP message
				// (looks like clustering "hint"). We will register and
				// ignore rather than log an error.
				serverID, ok := msg["server_id"]
				if ok {
					switch ID := serverID.(type) {
					case string:
						c.serverID = ID
					default:
						log.WithField("server_id", serverID).Warn("Server cluster node")
					}
				} else {
					log.WithField("message", msg).Warn("Server sent message with no `msg` field")
				}
			}
		case err := <-c.errors:
			log.WithField("target", c.url).WithField("origin", c.origin).WithError(err).Error("Websocket error")
		}
	}
}

func (c *Client) collectionBy(msg map[string]interface{}) Collection {
	n, ok := msg["collection"]
	if !ok {
		return NewMockCollection()
	}
	switch name := n.(type) {
	case string:
		return c.CollectionByName(name)
	default:
		return NewMockCollection()
	}
}

// inboxWorker pulls messages from a websocket, decodes JSON packets, and
// stuffs them into a message channel.
func (c *Client) inboxWorker(ws *websocket.Conn) {
	context := log.WithField("reconnects", c.reconnects).WithField("target", c.url).WithField("source", c.origin)
	for {
		var event interface{}

		if err := websocket.JSON.Receive(ws, &event); err != nil {
			if err != io.EOF {
				c.errors <- err
			}
			break
		}
		if c.pingTimer != nil {
			c.pingTimer.Reset(c.HeartbeatInterval)
		}
		if event == nil {
			context.Warn("Inbox worker found nil event.  Unclear why, as an error should have been triggered.")
		} else {
			c.inbox <- event.(map[string]interface{})
		}
	}

	c.Close()

	// Spawn a reconnect
	time.AfterFunc(c.ReconnectInterval, c.Reconnect)
}
