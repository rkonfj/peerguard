package vpn

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/rkonfj/peerguard/p2p"
	"github.com/rkonfj/peerguard/peer"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.zx2c4.com/wireguard/tun"
)

const (
	IPPacketOffset = 16
)

type Config struct {
	MTU     int
	TunName string
	Network string
	Peermap []string
	IPv4    string
}

type VPN struct {
	cfg      Config
	outbound chan []byte
	inbound  chan []byte
	exitSig  chan struct{}
	bufPool  sync.Pool
}

func NewVPN(cfg Config) *VPN {
	return &VPN{
		cfg:      cfg,
		outbound: make(chan []byte, 1000),
		inbound:  make(chan []byte, 1000),
		exitSig:  make(chan struct{}),
		bufPool: sync.Pool{New: func() any {
			buf := make([]byte, cfg.MTU+IPPacketOffset)
			return &buf
		}},
	}
}

func (vpn *VPN) Run(ctx context.Context) {
	device, err := tun.CreateTUN(vpn.cfg.TunName, vpn.cfg.MTU)
	if err != nil {
		panic(err)
	}
	defer device.Close()
	packetConn, err := p2p.ListenPacket(vpn.cfg.Network, vpn.cfg.Peermap, p2p.ListenPeerID(vpn.cfg.IPv4))
	if err != nil {
		panic(err)
	}
	go vpn.runTunReadEventLoop(device)
	go vpn.runTunWriteEventLoop(device)
	go vpn.runPacketConnReadEventLoop(packetConn)
	go vpn.runPacketConnWriteEventLoop(packetConn)
	<-ctx.Done()
}

func (vpn *VPN) runTunReadEventLoop(device tun.Device) {
	bufs := make([][]byte, 1000)
	sizes := make([]int, 1000)

	for i := range bufs {
		bufs[i] = make([]byte, vpn.cfg.MTU)
	}

	for {
		n, err := device.Read(bufs, sizes, 0)
		if err != nil && strings.Contains(err.Error(), os.ErrClosed.Error()) {
			return
		}
		if err != nil {
			panic(err)
		}
		for i := 0; i < n; i++ {
			packet := vpn.bufPool.Get().(*[]byte)
			copy(*packet, bufs[i][:sizes[i]])
			vpn.outbound <- (*packet)[:sizes[i]]
		}
	}
}

func (vpn *VPN) runTunWriteEventLoop(device tun.Device) {
	for {
		select {
		case <-vpn.exitSig:
			return
		case pkt := <-vpn.inbound:
			_, err := device.Write([][]byte{pkt}, IPPacketOffset)
			if err != nil {
				slog.Error("write to tun", "err", err.Error())
			}
		}
	}
}

func (vpn *VPN) runPacketConnReadEventLoop(packetConn net.PacketConn) {
	buf := make([]byte, vpn.cfg.MTU)
	for {
		select {
		case <-vpn.exitSig:
			return
		default:
		}
		n, _, err := packetConn.ReadFrom(buf)
		if err != nil {
			panic(err)
		}
		pkt := *vpn.bufPool.Get().(*[]byte)
		copy(pkt[IPPacketOffset:], buf[:n])
		vpn.inbound <- pkt[:n+IPPacketOffset]
	}
}

func (vpn *VPN) runPacketConnWriteEventLoop(packetConn net.PacketConn) {
	for {
		select {
		case <-vpn.exitSig:
			return
		case pkt := <-vpn.outbound:
			if pkt[0]>>4 == 4 {
				header, err := ipv4.ParseHeader(pkt)
				if err != nil {
					panic(err)
				}
				_, err = packetConn.WriteTo(pkt, peer.PeerID(header.Dst.String()))
				if err != nil {
					panic(err)
				}
				break
			}
			if pkt[0]>>4 == 6 {
				header, err := ipv6.ParseHeader(pkt)
				if err != nil {
					panic(err)
				}
				_, err = packetConn.WriteTo(pkt, peer.PeerID(header.Dst.String()))
				if err != nil {
					panic(err)
				}
				break
			}
			slog.Warn("Received invalid packet", "packet", hex.EncodeToString(pkt))
		}
	}
}