package main

// Portions of this file are modified after github.com/go-ping
// Copyright (c) 2016 Cameron Sparr and contributors.
// Copyright (c) 2022 Ain Ghazal

import (
	"encoding/binary"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"

	"github.com/kalikaneko/minivpn/vpn"
)

type packet struct {
	bytes  []byte
	rtt    time.Duration
	nbytes int
	ttl    int
	id     int
}

type stats struct {
	packetsRecv int
	packetsSent int
	packetLoss  float64
	rtts        []time.Duration
	maxRtt      time.Duration
	minRtt      time.Duration
	AvgRtt      time.Duration
}

func NewPinger(c *vpn.Client, host string, done chan bool) *Pinger {
	// TODO validate host ip / domain
	id := os.Getpid() & 0xffff
	return &Pinger{
		c: c, host: host,
		Count: 3, Interval: 1, Id: id,
		TTL:  64,
		done: done,
	}
}

type Pinger struct {
	// this should be an interface that defines the methods
	// that we use...
	c    *vpn.Client
	dc   chan []byte
	done chan bool

	host string

	Count    int
	Interval time.Duration
	Id       int

	statsMu sync.RWMutex

	TTL         int
	PacketsSent int
	PacketsRecv int
}

func (p *Pinger) Init() {
	p.dc = p.c.GetDataChannel()
	go p.SendPayloads()
	go p.ConsumeData()
}

func (p *Pinger) ConsumeData() {
	for {
		select {
		case data := <-p.dc:
			go p.handleIncoming(data)
		}
	}
}

func (p *Pinger) SendPayloads() {
	src := p.c.GetTunnelIP()
	srcIP := net.ParseIP(src)
	dstIP := net.ParseIP(p.host)
	for i := 0; i < p.Count; i++ {
		go p.craftAndSendICMP(&srcIP, &dstIP, p.TTL, i)
		time.Sleep(time.Second * 1)
	}

}

func (p *Pinger) craftAndSendICMP(src, dst *net.IP, ttl, seq int) {
	buf := newIcmpData(src, dst, 8, ttl, seq, p.Id)
	p.c.SendData(buf)
}

// XXX refactor into Send/Receive functions
func (p *Pinger) handleIncoming(d []byte) {
	var ip layers.IPv4
	var udp layers.UDP
	var icmp layers.ICMPv4
	var payload gopacket.Payload

	decoded := []gopacket.LayerType{}
	parser := gopacket.NewDecodingLayerParser(layers.LayerTypeIPv4, &ip, &icmp, &udp, &payload)

	err := parser.DecodeLayers(d, &decoded)
	if err != nil {
		log.Println("error decoding:", err)
		return
	}

	for _, layerType := range decoded {
		switch layerType {
		case layers.LayerTypeIPv4:
			if ip.DstIP.String() != p.c.GetTunnelIP() {
				log.Println("warn: icmp response with wrong dst")
				return
			}
			if ip.SrcIP.String() != p.host {
				log.Println("warn: icmp response with wrong src")
				return
			}
		case layers.LayerTypeUDP:
			// what's here? XXX
		case layers.LayerTypeICMPv4:
			if icmp.Id != uint16(p.Id) {
				log.Println("warn: icmp response with wrong id")
				return
			}
			//log.Println("seq:  ", icmp.Seq)
		}
	}
	// TODO extract ttl
	// TODO extract time
	// TODO keep statistics
	log.Printf("reply from %s: icmp_seq=%d ttl=0 time=0", ip.SrcIP, icmp.Seq)
	// 'reply from %s: icmp_seq=%d ttl=%d time=%.1fms'
	go p.updateStats()
}

func (p *Pinger) updateStats() {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	p.PacketsRecv += 1
	if p.PacketsRecv >= p.Count {
		p.done <- true
	}
}

func newIcmpData(src, dest *net.IP, typeCode, ttl, seq, id int) (data []byte) {
	ip := &layers.IPv4{}
	ip.Version = 4
	ip.Protocol = layers.IPProtocolICMPv4
	ip.SrcIP = *src
	ip.DstIP = *dest

	ip.Length = 20
	ip.TTL = uint8(ttl)

	icmp := &layers.ICMPv4{}
	icmp.TypeCode = layers.ICMPv4TypeCode(uint16(typeCode) << 8)
	icmp.Id = uint16(id)
	icmp.Seq = uint16(seq)
	icmp.Checksum = 0

	opts := gopacket.SerializeOptions{}
	opts.ComputeChecksums = true
	opts.FixLengths = true

	now := time.Now().UnixNano()
	var payload = make([]byte, 8)
	binary.LittleEndian.PutUint64(payload, uint64(now))

	buf := gopacket.NewSerializeBuffer()
	gopacket.SerializeLayers(buf, opts, ip, icmp, gopacket.Payload(payload))

	return buf.Bytes()
}
