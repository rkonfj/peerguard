package peer

const (
	CONTROL_RELAY               = 0
	CONTROL_PRE_NAT_TRAVERSAL   = 1
	CONTROL_REQUEST_PUBLIC_ADDR = 2
	CONTROL_NAT_TRAVERSAL       = 3
)

type NetworkID string
type PeerID string

func (id PeerID) String() string {
	return string(id)
}

func (id PeerID) Network() string {
	return "p2p"
}

func (id PeerID) Len() byte {
	return byte(len(id))
}

func (id PeerID) Bytes() []byte {
	return []byte(id)
}