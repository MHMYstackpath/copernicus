// Copyright (c) 2013-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wire

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// ProtocolVersion is the latest protocol version this package supports.
	ProtocolVersion uint32 = 70013

	// MultipleAddressVersion is the protocol version which added multiple
	// addresses per message (pver >= MultipleAddressVersion).
	MultipleAddressVersion uint32 = 209

	// GetHeadersVersion is where 'getheaders' was introduced.
	GetHeadersVersion uint32 = 31800

	// NetAddressTimeVersion is the protocol version which added the
	// timestamp field (pver >= NetAddressTimeVersion).
	NetAddressTimeVersion uint32 = 31402

	// BIP0031Version is the protocol version AFTER which a pong message
	// and nonce field in ping were added (pver > BIP0031Version).
	BIP0031Version uint32 = 60000

	// BIP0035Version is the protocol version which added the mempool
	// message (pver >= BIP0035Version).
	BIP0035Version uint32 = 60002

	// BIP0037Version is the protocol version which added new connection
	// bloom filtering related messages and extended the version message
	// with a relay flag (pver >= BIP0037Version).
	BIP0037Version uint32 = 70001

	// RejectVersion is the protocol version which added a new reject
	// message.
	RejectVersion uint32 = 70002

	// BIP0111Version is the protocol version which added the SFNodeBloom
	// service flag.
	BIP0111Version uint32 = 70011

	// SendHeadersVersion is the protocol version which added a new
	// sendheaders message.
	SendHeadersVersion uint32 = 70012

	// FeeFilterVersion is the protocol version which added a new
	// feefilter message.
	FeeFilterVersion uint32 = 70013

	// ShortIdsBlocksVersion is the version which short-id-based block download starts with
	ShortIdsBlocksVersion uint32 = 70014

	//InvalidCBNoBanVersion is the version which not banning for invalid compact blocks starts with
	InvalidCBNoBanVersion uint32 = 70015
)

// ServiceFlag identifies services supported by a bitcoin peer.
type ServiceFlag uint64

const (
	// SFNodeNetwork is a flag used to indicate a peer is a full node.
	SFNodeNetwork ServiceFlag = 1 << iota

	// SFNodeGetUTXO is a flag used to indicate a peer supports the
	// getutxos and utxos commands (BIP0064).
	SFNodeGetUTXO

	// SFNodeBloom is a flag used to indicate a peer supports bloom
	// filtering.
	SFNodeBloom

	// SFNodeXthin means the node supports Xtreme Thinblocks. If this is turned
	// off then the node will not service nor make xthin requests.
	SFNodeXthin

	// SFNodeCash means the node supports Bitcoin Cash and the
	// associated consensus rule changes.
	// This service bit is intended to be used prior until some time after the
	// UAHF activation when the Bitcoin Cash network has adequately separated.
	// TODO: remove (free up) the SFNodeCash service bit once no longer
	// needed.
	SFNodeCash

	// Bits 24-31 are reserved for temporary experiments. Just pick a bit that
	// isn't getting used, or one not being used much, and notify the
	// bitcoin-development mailing list. Remember that service bits are just
	// unauthenticated advertisements, so your code must be robust against
	// collisions and other cases where nodes may be advertising a service they
	// do not actually support. Other service bits should be allocated via the
	// BIP process.
)

// Map of service flags back to their constant names for pretty printing.
var sfStrings = map[ServiceFlag]string{
	SFNodeNetwork: "SFNodeNetwork",
	SFNodeGetUTXO: "SFNodeGetUTXO",
	SFNodeBloom:   "SFNodeBloom",
	SFNodeXthin:   "SFNodeXthin",
	SFNodeCash:    "SFNodeCash",
}

// orderedSFStrings is an ordered list of service flags from highest to
// lowest.
var orderedSFStrings = []ServiceFlag{
	SFNodeNetwork,
	SFNodeGetUTXO,
	SFNodeBloom,
	SFNodeXthin,
	SFNodeCash,
}

// String returns the ServiceFlag in human-readable form.
func (f ServiceFlag) String() string {
	// No flags are set.
	if f == 0 {
		return "0x0"
	}

	// Add individual bit flags.
	s := ""
	for _, flag := range orderedSFStrings {
		if f&flag == flag {
			s += sfStrings[flag] + "|"
			f -= flag
		}
	}

	// Add any remaining flags which aren't accounted for as hex.
	s = strings.TrimRight(s, "|")
	if f != 0 {
		s += "|0x" + strconv.FormatUint(uint64(f), 16)
	}
	s = strings.TrimLeft(s, "|")
	return s
}

// BitcoinNet represents which bitcoin network a message belongs to.
type BitcoinNet uint32

// Constants used to indicate the message bitcoin network.  They can also be
// used to seek to the next message when a stream's state is unknown, but
// this package does not provide that functionality since it's generally a
// better idea to simply disconnect clients that are misbehaving over TCP.
const (
	// MainNet represents the main bitcoin network.
	MainNet       BitcoinNet = 0xe8f3e1e3
	MainDiskMagic BitcoinNet = 0xd9b4bef9

	// RegTestNet represents the regression test network.
	RegTestNet   BitcoinNet = 0xfabfb5da
	RegDiskMagic BitcoinNet = 0xdab5bffa

	// TestNet3 represents the test network (version 3).
	TestNet3      BitcoinNet = 0xf4f3e5f4
	TestDiskMagic BitcoinNet = 0x0709110b
)

// bnStrings is a map of bitcoin networks back to their constant names for
// pretty printing.
var bnStrings = map[BitcoinNet]string{
	MainNet:    "MainNet",
	RegTestNet: "RegTestNet",
	TestNet3:   "TestNet3",
}

// String returns the BitcoinNet in human-readable form.
func (n BitcoinNet) String() string {
	if s, ok := bnStrings[n]; ok {
		return s
	}

	return fmt.Sprintf("Unknown BitcoinNet (%d)", uint32(n))
}
