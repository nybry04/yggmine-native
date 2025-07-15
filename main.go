package main

import "C"
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
	"unsafe"
)

type yggdrasil struct {
	core   *core.Core
	net    *netstack.YggdrasilNetstack
	socks5 struct {
		listener net.Listener
	}
}

const (
	errNoConfig            = C.int(1)
	errAlreadyInitialized  = C.int(2)
	errConfigParse         = C.int(3)
	errWhenCoreCreate      = C.int(4)
	errWhenNetstackCreate  = C.int(5)
	errProxyAlreadyStarted = C.int(6)
	errCantStartProxy      = C.int(7)
	errProxyAlreadyStopped = C.int(8)
	errProxyStopError      = C.int(9)
	errYggNotInitialized   = C.int(10)
)

var logger *log.Logger
var ygg yggdrasil

//export Init
func Init(conf *C.char) C.int {
	if logger == nil {
		logger = log.New(os.Stdout, "", log.Flags())
	}

	if conf == nil {
		return errNoConfig
	}

	if ygg.core != nil {
		return errAlreadyInitialized
	}

	reader := strings.NewReader(C.GoString(conf))
	cfg := config.GenerateConfig()

	if _, err := cfg.ReadFrom(reader); err != nil {
		return errConfigParse
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
			return errWhenCoreCreate
		}
	}

	if ygg.net, err = netstack.CreateYggdrasilNetstack(ygg.core); err != nil {
		return errWhenNetstackCreate
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
func NewPrivateKey(buf *C.char, bufLen C.int) C.int {
	FillBuffer(hex.EncodeToString(config.GenerateConfig().PrivateKey), buf, bufLen)
	return 0
}

//export StartSocks5Proxy
func StartSocks5Proxy(buf *C.int) C.int {
	if ygg.socks5.listener != nil {
		return errProxyAlreadyStarted
	}
	socksOptions := []socks5.Option{
		socks5.WithDial(ygg.net.DialContext),
	}
	server := socks5.NewServer(socksOptions...)

	var err error
	if ygg.socks5.listener, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		return errCantStartProxy
	}

	// Пропускаем обработку ошибок, т.к. не можем передать ее в программу
	go server.Serve(ygg.socks5.listener)

	FillBufferInt(int32(ygg.socks5.listener.Addr().(*net.TCPAddr).Port), buf)

	return 0
}

//export StopSocks5Proxy
func StopSocks5Proxy() C.int {
	if ygg.socks5.listener == nil {
		return errProxyAlreadyStopped
	}
	if err := ygg.socks5.listener.Close(); err != nil {
		return errProxyStopError
	}
	return 0
}

////export CreateProxyServerTCP
//func CreateProxyServerTCP(addressPtr *byte) C.int {
//	if ygg.net == nil {
//		return errYggNotInitialized
//	}
//
//	listenerAddr, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:0") // выбрать свободный порт
//	listener, err := net.ListenTCP("tcp", listenerAddr)
//	if err != nil {
//		logger.Println("Failed listen")
//		return -2
//	}
//
//	go func() {
//		defer listener.Close()
//		c, err := listener.Accept()
//		if err != nil {
//			logger.Println("Failed to accept")
//			return
//		}
//		defer c.Close()
//		address := cStringToGoString(addressPtr)
//		logger.Println("Trying resolve " + address)
//		addr, err := net.ResolveTCPAddr("tcp", address)
//		if err != nil {
//			logger.Println("Failed to resolve tcp addr")
//			return
//		}
//		r, err := ygg.net.DialTCP(addr)
//		if err != nil {
//			logger.Println("Failed to dial")
//			return
//		}
//		defer r.Close()
//		logger.Println("New socat at " + listener.Addr().String())
//		defer func() {
//			logger.Println("Socat closed " + listener.Addr().String())
//		}()
//		ProxyTCP(ygg.core.MTU(), c, r)
//	}()
//
//	return int32(listener.Addr().(*net.TCPAddr).Port)
//}

// FillBuffer Заполняем буфер на стороне приложения, чтобы предотвратить утечки памяти
func FillBuffer(data string, buf *C.char, bufSize C.int) {
	n := len(data)
	if n > int(bufSize) {
		n = int(bufSize) // Не записываем больше буфера
	}
	copy((*[1 << 30]byte)(unsafe.Pointer(buf))[:n:n], data)
}

// FillBufferInt нужно заполнять буфер, чтоб была возможность возвращать код ошибки
func FillBufferInt(number int32, buf *C.int) {
	*(*C.int)(unsafe.Pointer(buf)) = C.int(number)
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
