package internal

import "github.com/Comcast/gots/v2/packet"

// PacketModifier provides in-place rewrites of TS packets during the emit pass.
type PacketModifier interface {
	// Applies reports whether Rewrite should be called on the packet at pktNr.
	Applies(pktNr int64) bool
	// Done reports that this modifier is finished and will not influence any
	// further packets.
	Done(pktNr int64) bool
	// Rewrite performs the actual rewrite of pkt. It signals drop if the packet
	// should not be output. It is typically stateful and must be called
	// sequentially on packets.
	Rewrite(pktNr int64, pkt *packet.Packet) (drop bool, err error)
}
