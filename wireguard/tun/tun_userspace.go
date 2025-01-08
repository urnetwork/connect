/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2023 WireGuard LLC. All Rights Reserved.
 */

package tun

/* Implementation of the TUN device interface for linux
 */

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/userwireguard/conn"
	"github.com/urnetwork/userwireguard/logger"
	"github.com/urnetwork/userwireguard/tun"
)

type NATKey struct {
	IP   string
	Port int
}

type NATValue struct {
	IP net.IP
}

type UserspaceTun struct {
	closeOnce sync.Once
	events    chan tun.Event // device related events
	natRcv    chan []byte    // channel to receive packets from NAT
	log       *logger.Logger

	writeOpMu sync.Mutex // writeOpMu guards toWrite
	toWrite   []int

	natTableMu sync.Mutex
	natTable   map[NATKey]NATValue

	nat       *connect.LocalUserNat
	natCancel context.CancelFunc

	publicIP struct { // used to NAT outgoing packets
		v4 *net.IP
		v6 *net.IP
	}
}

func (tun *UserspaceTun) MTU() int {
	return 0
}

func (tun *UserspaceTun) Events() <-chan tun.Event {
	return tun.events
}

func (tun *UserspaceTun) AddEvent(event tun.Event) {
	tun.events <- event
}

func (tun *UserspaceTun) BatchSize() int {
	return 1
}

func (tun *UserspaceTun) Close() error {
	tun.closeOnce.Do(func() {
		if tun.events != nil {
			close(tun.events)
		}
		if tun.natRcv != nil {
			close(tun.natRcv)
		}
	})
	if tun.nat != nil {
		tun.natCancel()
		tun.nat = nil
		tun.natCancel = nil
	}
	return nil
}

func (tun *UserspaceTun) Write(bufs [][]byte, offset int) (int, error) {
	tun.writeOpMu.Lock()
	defer tun.writeOpMu.Unlock()
	var (
		errs  error
		total int
	)
	tun.toWrite = tun.toWrite[:0]
	for i := range bufs {
		tun.toWrite = append(tun.toWrite, i)
	}
	for _, bufsI := range tun.toWrite {
		packetData := bufs[bufsI][offset:]
		packet := gopacket.NewPacket(packetData, layers.LayerTypeIPv4, gopacket.Default)

		count, err := tun.processWritePacket(packet)
		if err != nil {
			errs = errors.Join(errs, err)
		}
		total += count
	}
	return total, errs
}

// processWritePacket modifies the packet and sends it through the NAT.
// It returns the number of packets sent and an error if any.
func (tun *UserspaceTun) processWritePacket(packet gopacket.Packet) (int, error) {
	var networkLayer gopacket.NetworkLayer // store either IPv4 or IPv6 layer
	var localSrcIP NATValue

	if ipv4Layer := packet.Layer(layers.LayerTypeIPv4); ipv4Layer != nil {
		// NAT IPv4 packet
		ipv4 := ipv4Layer.(*layers.IPv4)
		localSrcIP = NATValue{IP: ipv4.SrcIP}
		if tun.publicIP.v4 == nil {
			return 0, errors.New("cannot send IPv4 packet: no public IPv4 address set")
		}
		ipv4.SrcIP = *tun.publicIP.v4
		ipv4.TTL -= 1
		networkLayer = ipv4
	} else if ipv6Layer := packet.Layer(layers.LayerTypeIPv6); ipv6Layer != nil {
		// NAT IPv6 packet
		ipv6 := ipv6Layer.(*layers.IPv6)
		localSrcIP = NATValue{IP: ipv6.SrcIP}
		if tun.publicIP.v6 == nil {
			return 0, errors.New("cannot send IPv6 packet: no public IPv6 address set")
		}
		ipv6.SrcIP = *tun.publicIP.v6
		ipv6.HopLimit -= 1
		networkLayer = ipv6
	} else {
		return 0, fmt.Errorf("packet has no IPv4/IPv6 layer")
	}

	transportLayer := packet.TransportLayer()
	if transportLayer == nil {
		return 0, nil // NOTE: ignore packet if no transport layer found (e.g. ICMP)
	}

	natKey := NATKey{
		IP: networkLayer.NetworkFlow().Src().String(),
	}

	// set source port and update checksum
	switch t := transportLayer.(type) {
	case *layers.TCP:
		t.SetNetworkLayerForChecksum(networkLayer)
		natKey.Port = int(t.SrcPort)
	case *layers.UDP:
		t.SetNetworkLayerForChecksum(networkLayer)
		natKey.Port = int(t.SrcPort)
	default:
		return 0, fmt.Errorf("unsupported transport layer type: %T", t)
	}

	// serialize modified packet
	buffer := gopacket.NewSerializeBuffer()
	options := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	err := gopacket.SerializeLayers(buffer, options,
		networkLayer.(gopacket.SerializableLayer),
		transportLayer.(gopacket.SerializableLayer),
		gopacket.Payload(transportLayer.LayerPayload()))
	if err != nil {
		return 0, fmt.Errorf("failed to serialize modified packet: %w", err)
	}

	// send packet through NAT
	modifiedPacket := buffer.Bytes()
	ok := tun.nat.SendPacket(connect.TransferPath{}, protocol.ProvideMode_Network, modifiedPacket, 1*time.Second)
	if !ok {
		return 0, errors.New("failed to send packet through NAT")
	}

	// add nat entry
	tun.natTableMu.Lock()
	tun.natTable[natKey] = localSrcIP
	tun.natTableMu.Unlock()

	return 1, nil
}

func (tun *UserspaceTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	packetData, ok := <-tun.natRcv
	if !ok {
		return 0, os.ErrClosed // channel has been closed
	}

	readInto := bufs[0][offset:]
	n := copy(readInto, packetData) // copy packet data into the buffer

	if n > len(readInto) {
		return 0, fmt.Errorf("packet too large for buffer")
	}

	sizes[0] = n
	return 1, nil
}

// CreateTUN creates a Device using userspace sockets.
//
// TODO: add arguments for UserLocalNat from bringyour/connect.
func CreateUserspaceTUN(logger *logger.Logger, publicIPv4 *net.IP, publicIPv6 *net.IP) (tun.Device, error) {
	tun := &UserspaceTun{
		events:   make(chan tun.Event, 5),
		toWrite:  make([]int, 0, conn.IdealBatchSize),
		natTable: make(map[NATKey]NATValue),
		natRcv:   make(chan []byte),
		log:      logger,
	}
	tun.publicIP.v4 = publicIPv4
	tun.publicIP.v6 = publicIPv6

	clientId := "test-client-id"
	cancelCtx, cancel := context.WithCancel(context.Background())
	tun.nat = connect.NewLocalUserNatWithDefaults(
		cancelCtx,
		clientId,
	)
	removeCallback := tun.nat.AddReceivePacketCallback(tun.natReceive)
	tun.natCancel = func() {
		removeCallback()
		cancel()
	}

	return tun, nil
}

// natReceive is a callback for tun.nat to receive packets.
func (tun *UserspaceTun) natReceive(source connect.TransferPath, ipProtocol connect.IpProtocol, packet []byte) {
	pkt := gopacket.NewPacket(packet, layers.LayerTypeIPv4, gopacket.Default)
	tun.processNatReceivedPacket(pkt)
}

// processNatReceivedPacket NATs received packets.
func (tun *UserspaceTun) processNatReceivedPacket(packet gopacket.Packet) {
	var networkLayer gopacket.NetworkLayer // store either IPv4 or IPv6 layer

	if ipv4Layer := packet.Layer(layers.LayerTypeIPv4); ipv4Layer != nil {
		networkLayer = ipv4Layer.(*layers.IPv4)
	} else if ipv6Layer := packet.Layer(layers.LayerTypeIPv6); ipv6Layer != nil {
		networkLayer = ipv6Layer.(*layers.IPv6)
	} else {
		tun.log.Verbosef("NatReceive: packet has no IPv4/IPv6 layer")
		return
	}

	transportLayer := packet.TransportLayer()
	if transportLayer == nil {
		return // NOTE: ignore packet if no transport layer found (e.g. ICMP)
	}

	// compute nat key
	natKey := NATKey{
		IP: networkLayer.NetworkFlow().Dst().String(),
	}
	switch t := transportLayer.(type) {
	case *layers.TCP:
		natKey.Port = int(t.DstPort)
	case *layers.UDP:
		natKey.Port = int(t.DstPort)
	default:
		tun.log.Verbosef("NatReceive: unsupported transport layer type: %T", t)
		return
	}

	// find NAT entry
	localDstIP, found := tun.natTable[natKey]
	if !found {
		tun.log.Verbosef("NatReceive: no NAT entry found for %s:%d", natKey.IP, natKey.Port)
		return
	}

	// modify packet based on NAT entry
	switch ip := networkLayer.(type) {
	case *layers.IPv4:
		ip.DstIP = localDstIP.IP
	case *layers.IPv6:
		ip.DstIP = localDstIP.IP
	default:
		tun.log.Verbosef("NatReceive: unsupported network layer type: %T", ip)
		return
	}

	// update checksum
	switch t := transportLayer.(type) {
	case *layers.TCP:
		t.SetNetworkLayerForChecksum(networkLayer)
	case *layers.UDP:
		t.SetNetworkLayerForChecksum(networkLayer)
	default:
		tun.log.Verbosef("NatReceive: unsupported transport layer type: %T", t)
		return
	}

	// serialize modified packet
	buffer := gopacket.NewSerializeBuffer()
	options := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	err := gopacket.SerializeLayers(buffer, options, networkLayer.(gopacket.SerializableLayer), transportLayer.(gopacket.SerializableLayer), gopacket.Payload(transportLayer.LayerPayload()))
	if err != nil {
		tun.log.Verbosef("NatReceive: failed to serialize modified IPv4 packet: %v", err)
		return
	}

	// send modified packet to tun
	modifiedPacket := buffer.Bytes()
	tun.natRcv <- modifiedPacket
}
