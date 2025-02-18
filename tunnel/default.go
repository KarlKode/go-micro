package tunnel

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/micro/go-micro/transport"
	"github.com/micro/go-micro/util/log"
)

// tun represents a network tunnel
type tun struct {
	options Options

	sync.RWMutex

	// tunnel token
	token string

	// to indicate if we're connected or not
	connected bool

	// the send channel for all messages
	send chan *message

	// close channel
	closed chan bool

	// a map of sockets based on Micro-Tunnel-Id
	sockets map[string]*socket

	// outbound links
	links map[string]*link

	// listener
	listener transport.Listener
}

type link struct {
	transport.Socket
	id       string
	loopback bool
}

// create new tunnel on top of a link
func newTunnel(opts ...Option) *tun {
	options := DefaultOptions()
	for _, o := range opts {
		o(&options)
	}

	return &tun{
		options: options,
		token:   uuid.New().String(),
		send:    make(chan *message, 128),
		closed:  make(chan bool),
		sockets: make(map[string]*socket),
		links:   make(map[string]*link),
	}
}

// Init initializes tunnel options
func (t *tun) Init(opts ...Option) error {
	for _, o := range opts {
		o(&t.options)
	}
	return nil
}

// getSocket returns a socket from the internal socket map.
// It does this based on the Micro-Tunnel-Id and Micro-Tunnel-Session
func (t *tun) getSocket(id, session string) (*socket, bool) {
	// get the socket
	t.RLock()
	s, ok := t.sockets[id+session]
	t.RUnlock()
	return s, ok
}

// newSocket creates a new socket and saves it
func (t *tun) newSocket(id, session string) (*socket, bool) {
	// hash the id
	h := sha256.New()
	h.Write([]byte(id))
	id = fmt.Sprintf("%x", h.Sum(nil))

	// new socket
	s := &socket{
		id:      id,
		session: session,
		closed:  make(chan bool),
		recv:    make(chan *message, 128),
		send:    t.send,
		wait:    make(chan bool),
	}

	// save socket
	t.Lock()
	_, ok := t.sockets[id+session]
	if ok {
		// socket already exists
		t.Unlock()
		return nil, false
	}

	t.sockets[id+session] = s
	t.Unlock()

	// return socket
	return s, true
}

// TODO: use tunnel id as part of the session
func (t *tun) newSession() string {
	return uuid.New().String()
}

// process outgoing messages sent by all local sockets
func (t *tun) process() {
	// manage the send buffer
	// all pseudo sockets throw everything down this
	for {
		select {
		case msg := <-t.send:
			newMsg := &transport.Message{
				Header: make(map[string]string),
				Body:   msg.data.Body,
			}

			for k, v := range msg.data.Header {
				newMsg.Header[k] = v
			}

			// set the tunnel id on the outgoing message
			newMsg.Header["Micro-Tunnel-Id"] = msg.id

			// set the session id
			newMsg.Header["Micro-Tunnel-Session"] = msg.session

			// set the tunnel token
			newMsg.Header["Micro-Tunnel-Token"] = t.token

			// send the message via the interface
			t.RLock()
			if len(t.links) == 0 {
				log.Debugf("Zero links to send to")
			}
			for _, link := range t.links {
				// TODO: error check and reconnect
				log.Debugf("Sending %+v to %s", newMsg, link.Remote())
				if link.loopback && msg.outbound {
					continue
				}
				link.Send(newMsg)
			}
			t.RUnlock()
		case <-t.closed:
			return
		}
	}
}

// process incoming messages
func (t *tun) listen(link *link) {
	// loopback flag
	var loopback bool

	for {
		// process anything via the net interface
		msg := new(transport.Message)
		err := link.Recv(msg)
		if err != nil {
			log.Debugf("Tunnel link %s receive error: %v", link.Remote(), err)
			return
		}

		switch msg.Header["Micro-Tunnel"] {
		case "connect":
			// check the Micro-Tunnel-Token
			token, ok := msg.Header["Micro-Tunnel-Token"]
			if !ok {
				continue
			}

			// are we connecting to ourselves?
			if token == t.token {
				loopback = true
				link.loopback = true
			}
			continue
		case "close":
			// TODO: handle the close message
			// maybe report io.EOF or kill the link
			continue
		}

		// the tunnel id
		id := msg.Header["Micro-Tunnel-Id"]
		delete(msg.Header, "Micro-Tunnel-Id")

		// the session id
		session := msg.Header["Micro-Tunnel-Session"]
		delete(msg.Header, "Micro-Tunnel-Session")

		// if the session id is blank there's nothing we can do
		// TODO: check this is the case, is there any reason
		// why we'd have a blank session? Is the tunnel
		// used for some other purpose?
		if len(id) == 0 || len(session) == 0 {
			continue
		}

		var s *socket
		var exists bool

		log.Debugf("Received %+v from %s", msg, link.Remote())

		switch {
		case loopback:
			s, exists = t.getSocket(id, "listener")
		default:
			// get the socket based on the tunnel id and session
			// this could be something we dialed in which case
			// we have a session for it otherwise its a listener
			s, exists = t.getSocket(id, session)
			if !exists {
				// try get it based on just the tunnel id
				// the assumption here is that a listener
				// has no session but its set a listener session
				s, exists = t.getSocket(id, "listener")
			}
		}
		// bail if no socket has been found
		if !exists {
			log.Debugf("Tunnel skipping no socket exists")
			// drop it, we don't care about
			// messages we don't know about
			continue
		}
		log.Debugf("Tunnel using socket %s %s", s.id, s.session)

		// is the socket closed?
		select {
		case <-s.closed:
			// closed
			delete(t.sockets, id)
			continue
		default:
			// process
		}

		// is the socket new?
		select {
		// if its new the socket is actually blocked waiting
		// for a connection. so we check if its waiting.
		case <-s.wait:
		// if its waiting e.g its new then we close it
		default:
			// set remote address of the socket
			s.remote = msg.Header["Remote"]
			close(s.wait)
		}

		// construct a new transport message
		tmsg := &transport.Message{
			Header: msg.Header,
			Body:   msg.Body,
		}

		// construct the internal message
		imsg := &message{
			id:      id,
			session: session,
			data:    tmsg,
		}

		// append to recv backlog
		// we don't block if we can't pass it on
		select {
		case s.recv <- imsg:
		default:
		}
	}
}

// connect the tunnel to all the nodes and listen for incoming tunnel connections
func (t *tun) connect() error {
	l, err := t.options.Transport.Listen(t.options.Address)
	if err != nil {
		return err
	}

	// save the listener
	t.listener = l

	go func() {
		// accept inbound connections
		err := l.Accept(func(sock transport.Socket) {
			log.Debugf("Tunnel accepted connection from %s", sock.Remote())
			// save the link
			id := uuid.New().String()
			t.Lock()
			link := &link{
				Socket: sock,
				id:     id,
			}
			t.links[id] = link
			t.Unlock()

			// delete the link
			defer func() {
				log.Debugf("Deleting connection from %s", sock.Remote())
				t.Lock()
				delete(t.links, id)
				t.Unlock()
			}()

			// listen for inbound messages
			t.listen(link)
		})

		t.Lock()
		defer t.Unlock()

		// still connected but the tunnel died
		if err != nil && t.connected {
			log.Logf("Tunnel listener died: %v", err)
		}
	}()

	for _, node := range t.options.Nodes {
		// skip zero length nodes
		if len(node) == 0 {
			continue
		}

		log.Debugf("Tunnel dialing %s", node)
		// TODO: reconnection logic is required to keep the tunnel established
		c, err := t.options.Transport.Dial(node)
		if err != nil {
			log.Debugf("Tunnel failed to connect to %s: %v", node, err)
			continue
		}
		log.Debugf("Tunnel connected to %s", node)

		if err := c.Send(&transport.Message{
			Header: map[string]string{
				"Micro-Tunnel":       "connect",
				"Micro-Tunnel-Token": t.token,
			},
		}); err != nil {
			continue
		}

		// save the link
		id := uuid.New().String()
		link := &link{
			Socket: c,
			id:     id,
		}
		t.links[id] = link

		// process incoming messages
		go t.listen(link)
	}

	// process outbound messages to be sent
	// process sends to all links
	go t.process()

	return nil
}

// Connect the tunnel
func (t *tun) Connect() error {
	t.Lock()
	defer t.Unlock()

	// already connected
	if t.connected {
		return nil
	}

	// send the connect message
	if err := t.connect(); err != nil {
		return err
	}

	// set as connected
	t.connected = true
	// create new close channel
	t.closed = make(chan bool)

	return nil
}

func (t *tun) close() error {
	// close all the links
	for id, link := range t.links {
		link.Send(&transport.Message{
			Header: map[string]string{
				"Micro-Tunnel":       "close",
				"Micro-Tunnel-Token": t.token,
			},
		})
		link.Close()
		delete(t.links, id)
	}

	// close the listener
	return t.listener.Close()
}

// Close the tunnel
func (t *tun) Close() error {
	t.Lock()
	defer t.Unlock()

	if !t.connected {
		return nil
	}

	select {
	case <-t.closed:
		return nil
	default:
		// close all the sockets
		for _, s := range t.sockets {
			s.Close()
		}
		// close the connection
		close(t.closed)
		t.connected = false

		// send a close message
		// we don't close the link
		// just the tunnel
		return t.close()
	}

	return nil
}

// Dial an address
func (t *tun) Dial(addr string) (Conn, error) {
	log.Debugf("Tunnel dialing %s", addr)
	c, ok := t.newSocket(addr, t.newSession())
	if !ok {
		return nil, errors.New("error dialing " + addr)
	}
	// set remote
	c.remote = addr
	// set local
	c.local = "local"
	// outbound socket
	c.outbound = true

	return c, nil
}

// Accept a connection on the address
func (t *tun) Listen(addr string) (Listener, error) {
	log.Debugf("Tunnel listening on %s", addr)
	// create a new socket by hashing the address
	c, ok := t.newSocket(addr, "listener")
	if !ok {
		return nil, errors.New("already listening on " + addr)
	}

	// set remote. it will be replaced by the first message received
	c.remote = "remote"
	// set local
	c.local = addr

	tl := &tunListener{
		addr: addr,
		// the accept channel
		accept: make(chan *socket, 128),
		// the channel to close
		closed: make(chan bool),
		// tunnel closed channel
		tunClosed: t.closed,
		// the connection
		conn: c,
		// the listener socket
		socket: c,
	}

	// this kicks off the internal message processor
	// for the listener so it can create pseudo sockets
	// per session if they do not exist or pass messages
	// to the existign sessions
	go tl.process()

	// return the listener
	return tl, nil
}
