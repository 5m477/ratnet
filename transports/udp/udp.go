package udp

import (
	"bufio"
	"encoding/gob"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	kcp "github.com/xtaci/kcp-go"

	"github.com/awgh/ratnet"
	"github.com/awgh/ratnet/api"
)

var cachedSessions map[string]*kcp.UDPSession

func init() {
	ratnet.Transports["udp"] = NewFromMap // register this module by name (for deserialization support)

	cachedSessions = make(map[string]*kcp.UDPSession)
}

// NewFromMap : Makes a new instance of this transport module from a map of arguments (for deserialization support)
func NewFromMap(node api.Node, t map[string]interface{}) api.Transport {
	return New(node)
}

// New : Makes a new instance of this transport module
func New(node api.Node) *Module {

	instance := new(Module)
	instance.node = node

	instance.byteLimit = 8000 * 1024 //125000

	return instance
}

// Module : UDP Implementation of a Transport module
type Module struct {
	node      api.Node
	isRunning bool
	wg        sync.WaitGroup
	byteLimit int64
}

// Name : Returns name of module
func (m *Module) Name() string {
	return "udp"
}

// MarshalJSON : Create a serialied representation of the config of this module
func (m *Module) MarshalJSON() (b []byte, e error) {
	return json.Marshal(map[string]interface{}{
		"Transport": "udp"})
}

// ByteLimit - get limit on bytes per bundle for this transport
func (m *Module) ByteLimit() int64 { return m.byteLimit }

// SetByteLimit - set limit on bytes per bundle for this transport
func (m *Module) SetByteLimit(limit int64) { m.byteLimit = limit }

// Listen : opens a UDP socket and listens
func (m *Module) Listen(listen string, adminMode bool) {
	// make sure we dont run twice
	if m.isRunning {
		return
	}
	lis, err := kcp.ListenWithOptions(listen, nil, 10, 0) //disabled FEC
	if err != nil {
		log.Println(err.Error())
		return
	}
	m.isRunning = true
	m.wg.Add(1)

	// read loop
	go func() {
		defer lis.Close() // make sure the socket closes when we're done with it
		defer m.wg.Done()

		// read from socket
		for m.isRunning {
			conn, err := lis.Accept()
			if err != nil {
				log.Println(err)
				continue
			}

			reader := bufio.NewReader(conn)
			writer := bufio.NewWriter(conn)

			for m.isRunning { // read multiple messages on the same connection

				var a api.RemoteCall
				//Use default gob decoder
				dec := gob.NewDecoder(reader)
				if err = dec.Decode(&a); err != nil {
					log.Println("listen gob decode failed: " + err.Error())
					break
				}

				var result interface{}
				if adminMode {
					result, err = m.node.AdminRPC(m, a)
				} else {
					result, err = m.node.PublicRPC(m, a)
				}
				//log.Printf("result type %T \n", result)

				rr := api.RemoteResponse{}
				if err != nil {
					rr.Error = err.Error()
				}
				if result != nil { // gob cannot encode typed Nils, only interface{} Nils...wtf?
					rr.Value = result
				}

				enc := gob.NewEncoder(writer)
				if err := enc.Encode(rr); err != nil {
					log.Println("listen gob encode failed: " + err.Error())
					break
				}
				_ = writer.Flush()
			}
		}
	}()
}

// RPC : transmit data via UDP
func (m *Module) RPC(host string, method string, args ...interface{}) (interface{}, error) {

	conn, ok := cachedSessions[host]
	if !ok {
		// open client socket
		var err error
		conn, err = kcp.DialWithOptions(host, nil, 10, 0) // disabled FEC
		if err != nil {
			return nil, err
		}
		conn.SetStreamMode(false)
		conn.SetWindowSize(512, 512)
		conn.SetNoDelay(1, 40, 2, 1)
		conn.SetACKNoDelay(false)

		cachedSessions[host] = conn
	}
	conn.SetReadDeadline(time.Now().Add(35 * time.Second))
	conn.SetWriteDeadline(time.Now().Add(35 * time.Second))

	//	defer conn.Close()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	var a api.RemoteCall
	a.Action = method
	a.Args = args

	//use default gob encoder
	enc := gob.NewEncoder(writer)
	if err := enc.Encode(a); err != nil {
		//log.Println("rpc gob encode failed: " + err.Error())
		return nil, err
	}
	writer.Flush()

	var rr api.RemoteResponse
	dec := gob.NewDecoder(reader)
	if err := dec.Decode(&rr); err != nil {
		//log.Println("rpc gob decode failed: " + err.Error())
		return nil, err
	}

	//log.Printf("dirty rx in rpc: %+v\n", rr.Value)

	if rr.IsErr() {
		return nil, errors.New(rr.Error)
	}
	if rr.IsNil() {
		return nil, nil
	}
	return rr.Value, nil
}

// Stop : Stops module
func (m *Module) Stop() {
	m.isRunning = false

	m.wg.Wait()
}
