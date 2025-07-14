package main

import (
	"encoding/hex"
	"github.com/gologme/log"
	"github.com/things-go/go-socks5"
	"github.com/yggdrasil-network/yggdrasil-go/src/config"
	"github.com/yggdrasil-network/yggdrasil-go/src/core"
	"github.com/yggdrasil-network/yggstack/src/netstack"
	"net"
	"os"
	"strings"
	"sync"
	"unsafe"
)

type yggdrasil struct {
	core   *core.Core
	net    *netstack.YggdrasilNetstack
	socks5 struct {
		listener net.Listener
	}
}

var logger *log.Logger
var ygg yggdrasil
var mutex sync.Mutex

//export Init
func Init(confPtr *byte) int32 {
	mutex.Lock()
	defer mutex.Unlock()
	if logger == nil {
		logger = log.New(os.Stdout, "", log.Flags())
	}

	if confPtr == nil {
		return -1
	}

	if ygg.core != nil {
		return -2
	}

	reader := strings.NewReader(cStringToGoString(confPtr))
	cfg := config.GenerateConfig()

	if _, err := cfg.ReadFrom(reader); err != nil {
		return -2
	}

	var err error
	{
		options := []core.SetupOption{
			core.NodeInfo(cfg.NodeInfo),
			core.NodeInfoPrivacy(cfg.NodeInfoPrivacy),
		}
		for _, peer := range cfg.Peers {
			options = append(options, core.Peer{URI: peer})
		}
		if ygg.core, err = core.New(cfg.Certificate, logger, options...); err != nil {
			return -3
		}
	}

	ygg.net, err = netstack.CreateYggdrasilNetstack(ygg.core)
	if err != nil {
		return -4
	}

	return 0
}

//export Shutdown
func Shutdown() {
	if ygg.core != nil {
		ygg.core = nil
	}
	if ygg.net != nil {
		ygg.net = nil
	}
}

//export NewPrivateKey
func NewPrivateKey(buf unsafe.Pointer, bufLen int32) {
	copyStrToBuf(hex.EncodeToString(config.GenerateConfig().PrivateKey), buf, bufLen)
}

//export StartSocks5Proxy
func StartSocks5Proxy() int32 {
	if ygg.socks5.listener != nil {
		logger.Errorln("Proxy server already started")
		return 0
	}
	socksOptions := []socks5.Option{
		socks5.WithDial(ygg.net.DialContext),
	}
	server := socks5.NewServer(socksOptions...)

	var err error
	ygg.socks5.listener, err = net.Listen("tcp", "127.0.0.1:0") // свободный порт
	if err != nil {
		logger.Fatalln("Can't start socks5 proxy")
		return 0
	}

	go func() {
		defer logger.Println("Socks5 proxy stopped")
		err := server.Serve(ygg.socks5.listener)
		if err != nil {
			logger.Println(err)
		}
	}()

	return int32(ygg.socks5.listener.Addr().(*net.TCPAddr).Port)
}

//export StopSocks5Proxy
func StopSocks5Proxy() {
	if ygg.socks5.listener != nil {
		_ = ygg.socks5.listener.Close()
	}
}

//export CreateProxyServerTCP
func CreateProxyServerTCP(addressPtr *byte) int32 {
	mutex.Lock()
	defer mutex.Unlock()
	if ygg.net == nil {
		return -1
	}

	listenerAddr, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:0") // выбрать свободный порт
	listener, err := net.ListenTCP("tcp", listenerAddr)
	if err != nil {
		logger.Println("Failed listen")
		return -2
	}

	go func() {
		defer listener.Close()
		c, err := listener.Accept()
		if err != nil {
			logger.Println("Failed to accept")
			return
		}
		defer c.Close()
		address := cStringToGoString(addressPtr)
		logger.Println("Trying resolve " + address)
		addr, err := net.ResolveTCPAddr("tcp", address)
		if err != nil {
			logger.Println("Failed to resolve tcp addr")
			return
		}
		r, err := ygg.net.DialTCP(addr)
		if err != nil {
			logger.Println("Failed to dial")
			return
		}
		defer r.Close()
		logger.Println("New socat at " + listener.Addr().String())
		defer func() {
			logger.Println("Socat closed " + listener.Addr().String())
		}()
		ProxyTCP(ygg.core.MTU(), c, r)
	}()

	return int32(listener.Addr().(*net.TCPAddr).Port)
}

func cStringToGoString(cstr *byte) string {
	if cstr == nil {
		return ""
	}
	ptr := uintptr(unsafe.Pointer(cstr))
	var bytes []byte
	for {
		b := *(*byte)(unsafe.Pointer(ptr))
		if b == 0 {
			break
		}
		bytes = append(bytes, b)
		ptr++
	}
	return string(bytes)
}

func copyStrToBuf(msg string, buf unsafe.Pointer, bufLen int32) {
	bytes := []byte(msg)

	if len(bytes) >= int(bufLen) {
		bytes = bytes[:bufLen-1]
	}

	copy((*[1 << 30]byte)(buf)[:len(bytes):len(bytes)], bytes)
	(*[1 << 30]byte)(buf)[len(bytes)] = 0
}
func tcpProxyFunc(mtu uint64, dst, src net.Conn) error {
	buf := make([]byte, mtu)
	for {
		n, err := src.Read(buf[:])
		if err != nil {
			return err
		}
		if n > 0 {
			n, err = dst.Write(buf[:n])
			if err != nil {
				return err
			}
		}
	}
}

func ProxyTCP(mtu uint64, c1, c2 net.Conn) error {
	//header := &proxyproto.Header{
	//	Version:           2,
	//	Command:           proxyproto.PROXY,
	//	TransportProtocol: proxyproto.TCPv4,
	//	SourceAddr: &net.TCPAddr{
	//		IP:   net.ParseIP("46.172.127.225"),
	//		Port: 1000,
	//	},
	//	DestinationAddr: &net.TCPAddr{
	//		IP:   net.ParseIP("80.242.59.124"),
	//		Port: 25565,
	//	},
	//}
	//_, err := header.WriteTo(c2)
	//if err != nil {
	//	fmt.Println(err)
	//}
	// Start proxying
	errCh := make(chan error, 2)
	go func() { errCh <- tcpProxyFunc(mtu, c1, c2) }()
	go func() { errCh <- tcpProxyFunc(mtu, c2, c1) }()

	// Wait
	for i := 0; i < 2; i++ {
		e := <-errCh
		if e != nil {
			c1.Close()
			c2.Close()
			return e
		}
	}

	return nil
}

func main() {}
