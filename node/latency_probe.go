package node

import (
	"errors"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	tcpLatencyProbeHoldTime       = time.Second
	tcpLatencyProbeMaxConnections = 128
)

// tcpLatencyProbe gives TCP-only client latency checks a handshake endpoint
// for nodes whose real protocol listener uses UDP. It never carries proxy
// traffic: accepted connections are closed immediately after the handshake.
type tcpLatencyProbe struct {
	listener net.Listener
	done     chan struct{}
	stop     chan struct{}
	slots    chan struct{}
	clients  sync.WaitGroup
	close    sync.Once
}

func startTCPLatencyProbe(listenIP string, port int) (*tcpLatencyProbe, error) {
	address := net.JoinHostPort(listenIP, strconv.Itoa(port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	probe := &tcpLatencyProbe{
		listener: listener,
		done:     make(chan struct{}),
		stop:     make(chan struct{}),
		slots:    make(chan struct{}, tcpLatencyProbeMaxConnections),
	}
	go probe.serve()
	return probe, nil
}

func (p *tcpLatencyProbe) serve() {
	defer close(p.done)
	for {
		connection, err := p.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		select {
		case p.slots <- struct{}{}:
			p.clients.Add(1)
			go p.hold(connection)
		default:
			// The TCP handshake has already completed. Close excess idle probes
			// immediately instead of allowing them to exhaust server resources.
			_ = connection.Close()
		}
	}
}

func (p *tcpLatencyProbe) hold(connection net.Conn) {
	defer p.clients.Done()
	defer func() { <-p.slots }()
	defer connection.Close()

	timer := time.NewTimer(tcpLatencyProbeHoldTime)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-p.stop:
	}
}

func (p *tcpLatencyProbe) Close() error {
	var err error
	p.close.Do(func() {
		close(p.stop)
		err = p.listener.Close()
		<-p.done
		p.clients.Wait()
	})
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
