package coord

import "net"

// defaultListenTCP is the production implementation of netListen. Lives
// in its own file so the test can override netListen without redefining
// the production helper.
func defaultListenTCP(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}
