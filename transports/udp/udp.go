package udp

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
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
			c, err := lis.Accept()
			if err != nil {
				log.Println(err)
				continue
			}

			//log.Println("UDP accepted new connection")

			c.SetReadDeadline(time.Now().Add(35 * time.Second))
			c.SetWriteDeadline(time.Now().Add(35 * time.Second))

			go func(conn net.Conn) {
				reader := bufio.NewReader(conn)
				writer := bufio.NewWriter(conn)

				for m.isRunning { // read multiple messages on the same connection

					// read
					blen := make([]byte, 4)
					n, err := reader.Read(blen)
					if n != 4 {
						log.Println("Listen remote read len underflow: n =", n)
						break
					}
					if err != nil {
						log.Println("Listen remote read len failed: " + err.Error())
						break
					}
					rlen := binary.LittleEndian.Uint32(blen)
					buf := make([]byte, rlen)
					n, err = reader.Read(buf)
					if uint32(n) != rlen {
						log.Println("Listen remote read underflow: n =", n)
						break
					}
					if err != nil {
						log.Println("Listen remote read failed: " + err.Error())
						break
					}
					//

					a, err := api.RemoteCallFromBytes(buf)
					if err != nil {
						log.Println("Listen remote deserialize failed: " + err.Error())
						break
					}

					var result interface{}
					if adminMode {
						result, err = m.node.AdminRPC(m, *a)
					} else {
						result, err = m.node.PublicRPC(m, *a)
					}
					//log.Printf("result type %T \n", result)
					rr := api.RemoteResponse{}
					if err != nil {
						rr.Error = err.Error()
					}
					if result != nil { //
						rr.Value = result
					}

					rbytes := api.RemoteResponseToBytes(&rr)
					// write
					wlen := make([]byte, 4)
					binary.LittleEndian.PutUint32(wlen, uint32(len(rbytes)))
					rbytes = append(wlen, rbytes...)
					if _, err := writer.Write(rbytes); err != nil {
						log.Println("Listen remote write failed: " + err.Error())
						break
					}
					writer.Flush()
					//
				}
			}(c)
		}
	}()
}

// RPC : transmit data via UDP
func (m *Module) RPC(host string, method string, args ...interface{}) (interface{}, error) {

	//log.Printf("\n***\n***RPC %s called: %s  with: %v\n***\n", method, host, args)

	conn, ok := cachedSessions[host]
	if !ok {
		// open client socket
		var err error
		conn, err = kcp.DialWithOptions(host, nil, 10, 0) // disabled FEC
		if err != nil {
			log.Println("kcp dial error in udp:", err)
			return nil, err
		}
		conn.SetStreamMode(false)
		conn.SetWindowSize(512, 512)
		conn.SetNoDelay(1, 20, 2, 1)
		conn.SetACKNoDelay(true)

		cachedSessions[host] = conn
	}
	conn.SetReadDeadline(time.Now().Add(35 * time.Second))
	conn.SetWriteDeadline(time.Now().Add(35 * time.Second))

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	var a api.RemoteCall
	a.Action = method
	a.Args = args

	rbytes := api.RemoteCallToBytes(&a)

	// write
	rlen := make([]byte, 4)
	binary.LittleEndian.PutUint32(rlen, uint32(len(rbytes)))
	rbytes = append(rlen, rbytes...)
	if _, err := writer.Write(rbytes); err != nil {
		log.Println("RPC remote write failed: " + err.Error())
		delete(cachedSessions, host) // something's wrong, make a new session next attempt
		_ = conn.Close()
		return nil, err
	}
	writer.Flush()
	//

	// read
	blen := make([]byte, 4)
	n, err := reader.Read(blen)
	if n != 4 {
		log.Println("RPC remote read len underflow: n =", n)
		delete(cachedSessions, host) // something's wrong, make a new session next attempt
		_ = conn.Close()
		return nil, err
	}
	if err != nil {
		log.Println("RPC remote read len failed: " + err.Error())
		delete(cachedSessions, host) // something's wrong, make a new session next attempt
		_ = conn.Close()
		return nil, err
	}
	wlen := binary.LittleEndian.Uint32(blen)
	buf := make([]byte, wlen)
	n, err = reader.Read(buf)
	if uint32(n) != wlen {
		log.Println("RPC remote read underflow: n =", n)
		delete(cachedSessions, host) // something's wrong, make a new session next attempt
		_ = conn.Close()
		return nil, err
	}
	if err != nil {
		log.Println("RPC remote read failed: " + err.Error())
		delete(cachedSessions, host) // something's wrong, make a new session next attempt
		_ = conn.Close()
		return nil, err
	}
	//
	rr, err := api.RemoteResponseFromBytes(buf)
	if err == io.EOF {
		rr = nil
	}
	if err != nil {
		log.Println("RPC decode failed: " + err.Error())
		delete(cachedSessions, host) // something's wrong, make a new session next attempt
		_ = conn.Close()
		return nil, err
	}

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

	for k, v := range cachedSessions {
		delete(cachedSessions, k)
		_ = v.Close()
	}
}
