package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/florianl/go-nfqueue"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

const (
	state_new         = 2
	state_established = 3
)

type FilterManager struct {
	queueNum uint16
	ipv6     bool

	logger *zap.Logger

	dnsRespNF *nfqueue.Nfqueue

	filters []*filter
}

type filter struct {
	opts *FilterOptions

	logger *zap.Logger

	dnsReqNF  *nfqueue.Nfqueue
	genericNF *nfqueue.Nfqueue

	connections *TimedCache
	// TODO: replace with net/netaddr when it gets released in the
	// standard library (1.18?)
	allowedIPs          *TimedCache
	additionalHostnames *TimedCache
}

func StartFilters(ctx context.Context, logger *zap.Logger, config *Config) (*FilterManager, error) {
	f := FilterManager{
		queueNum: config.InboundDNSQueue,
		ipv6:     config.IPv6,
		logger:   logger,
		filters:  make([]*filter, len(config.Filters)),
	}

	for i, filterOpt := range config.Filters {
		filter, err := startFilter(ctx, logger, &filterOpt)
		if err != nil {
			return nil, err
		}

		f.filters[i] = filter
	}

	nf, err := startNfQueue(ctx, logger, config.InboundDNSQueue, config.IPv6, newDNSResponseCallback(&f))
	if err != nil {
		return nil, err
	}
	f.dnsRespNF = nf

	return &f, nil
}

func (f *FilterManager) Stop() {
	for i := range f.filters {
		f.filters[i].close()
	}

	f.dnsRespNF.Close()
}

func startFilter(ctx context.Context, logger *zap.Logger, opts *FilterOptions) (*filter, error) {
	f := filter{
		opts:                opts,
		logger:              logger,
		connections:         NewTimedCache(logger),
		allowedIPs:          NewTimedCache(logger),
		additionalHostnames: NewTimedCache(logger),
	}

	dnsNF, err := startNfQueue(ctx, logger, opts.DNSQueue, opts.IPv6, newDNSRequestCallback(&f))
	if err != nil {
		return nil, fmt.Errorf("error opening nfqueue: %v", err)
	}
	f.dnsReqNF = dnsNF

	genericNF, err := startNfQueue(ctx, logger, opts.TrafficQueue, opts.IPv6, newGenericCallback(&f))
	if err != nil {
		return nil, fmt.Errorf("error opening nfqueue: %v", err)
	}
	f.genericNF = genericNF

	return &f, nil
}

func startNfQueue(ctx context.Context, logger *zap.Logger, queueNum uint16, ipv6 bool, hook nfqueue.HookFunc) (*nfqueue.Nfqueue, error) {
	afFamily := unix.AF_INET
	if ipv6 {
		afFamily = unix.AF_INET6
	}

	nfqConf := nfqueue.Config{
		NfQueue:      queueNum,
		MaxPacketLen: 0xffff,
		MaxQueueLen:  0xffff,
		AfFamily:     uint8(afFamily),
		Copymode:     nfqueue.NfQnlCopyPacket,
		Flags:        nfqueue.NfQaCfgFlagConntrack,
	}

	nf, err := nfqueue.Open(&nfqConf)
	if err != nil {
		return nil, fmt.Errorf("error opening nfqueue: %v", err)
	}

	if err := nf.RegisterWithErrorFunc(ctx, hook, newErrorCallback(logger)); err != nil {
		nf.Close()
		return nil, fmt.Errorf("error registering nfqueue: %v", err)
	}
	logger.Info("started nfqueue", zap.Uint16("nfqueue", queueNum))

	return nf, nil
}

func (f *filter) close() {
	f.dnsReqNF.Close()
	f.genericNF.Close()
}

func newDNSRequestCallback(f *filter) nfqueue.HookFunc {
	logger := f.logger.With(zap.Uint16("queue.num", f.opts.DNSQueue))

	return func(attr nfqueue.Attribute) int {
		if attr.PacketID == nil {
			return 0
		}
		if attr.CtInfo == nil {
			return 0
		}
		if attr.Payload == nil {
			return 0
		}

		// verify that a DNS request is from a new connection
		if *attr.CtInfo != state_new {
			logger.Warn("dropping DNS request with that is not from a new connection")

			if err := f.dnsReqNF.SetVerdict(*attr.PacketID, nfqueue.NfDrop); err != nil {
				logger.Error("error setting verdict", zap.String("error", err.Error()))
			}
			return 0
		}

		dns, connID, err := parseDNSPacket(*attr.Payload, f.opts.IPv6, false)
		if err != nil {
			logger.Error("error parsing DNS packet", zap.String("error", err.Error()))
			return 0
		}

		// validate DNS request questions are for allowed
		// hostnames, drop them otherwise
		if !f.validateDNSQuestions(logger, dns) {
			if err := f.dnsReqNF.SetVerdict(*attr.PacketID, nfqueue.NfDrop); err != nil {
				logger.Error("error setting verdict", zap.String("error", err.Error()))
			}
			return 0
		}

		if err := f.dnsReqNF.SetVerdict(*attr.PacketID, nfqueue.NfAccept); err != nil {
			logger.Error("error setting verdict", zap.String("error", err.Error()))
			return 0
		}

		questions := make([]string, len(dns.Questions))
		for i := range dns.Questions {
			questions[i] = string(dns.Questions[i].Name) + ": " + dns.Questions[i].Type.String()
		}
		logger.Info("allowing DNS request", zap.Strings("questions", questions))

		// give DNS connections a minute to finish max
		logger.Debug("adding connection", zap.String("conn_id", connID))
		f.connections.AddEntry(connID, true, time.Minute)

		return 0
	}
}

func parseDNSPacket(packet []byte, ipv6, inbound bool) (*layers.DNS, string, error) {
	var (
		ip4     layers.IPv4
		ip6     layers.IPv6
		udp     layers.UDP
		tcp     layers.TCP
		dns     layers.DNS
		parser  *gopacket.DecodingLayerParser
		decoded = make([]gopacket.LayerType, 0, 3)
	)

	// parse DNS packet
	if !ipv6 {
		parser = gopacket.NewDecodingLayerParser(layers.LayerTypeIPv4, &ip4, &udp, &tcp, &dns)
	} else {
		parser = gopacket.NewDecodingLayerParser(layers.LayerTypeIPv6, &ip6, &udp, &tcp, &dns)
	}

	if err := parser.DecodeLayers(packet, &decoded); err != nil {
		return nil, "", err
	}
	if len(decoded) != 3 {
		return nil, "", errors.New("not all layers were parsed")
	}

	var (
		connIDBuilder    strings.Builder
		srcIP, dstIP     string
		srcPort, dstPort string
	)

	// build connection ID so dns requests/responses can be correlated
	if decoded[0] == layers.LayerTypeIPv4 {
		srcIP = ip4.SrcIP.String()
		dstIP = ip4.DstIP.String()
	} else {
		srcIP = ip6.SrcIP.String()
		dstIP = ip6.DstIP.String()
	}
	if decoded[1] == layers.LayerTypeUDP {
		srcPort = strconv.Itoa(int(udp.SrcPort))
		dstPort = strconv.Itoa(int(udp.DstPort))
	} else {
		srcPort = strconv.Itoa(int(tcp.SrcPort))
		dstPort = strconv.Itoa(int(tcp.DstPort))
	}

	connIDBuilder.WriteRune(rune(decoded[1]))
	connIDBuilder.WriteByte('-')
	if inbound {
		connIDBuilder.WriteString(dstIP)
		connIDBuilder.WriteByte(':')
		connIDBuilder.WriteString(dstPort)
		connIDBuilder.WriteByte('-')
		connIDBuilder.WriteString(srcIP)
		connIDBuilder.WriteByte(':')
		connIDBuilder.WriteString(srcPort)
	} else {
		connIDBuilder.WriteString(srcIP)
		connIDBuilder.WriteByte(':')
		connIDBuilder.WriteString(srcPort)
		connIDBuilder.WriteByte('-')
		connIDBuilder.WriteString(dstIP)
		connIDBuilder.WriteByte(':')
		connIDBuilder.WriteString(dstPort)
	}

	return &dns, connIDBuilder.String(), nil
}

func (f *filter) validateDNSQuestions(logger *zap.Logger, dns *layers.DNS) bool {
	if dns.QDCount == 0 {
		// drop DNS requests with no questions; this probably
		// doesn't happen in practice but doesn't hurt to
		// handle this case
		logger.Info("dropping dns request with no questions")
		return false
	}

	var allowed bool
	for i := range dns.Questions {
		allowed = false
		for j := range f.opts.Hostnames {
			// check if the question has an allowed hostname as a
			// suffix to allow access to subdomains
			qName := string(dns.Questions[i].Name)
			if strings.HasSuffix(qName, f.opts.Hostnames[j]) || f.additionalHostnames.EntryExists(qName) {
				allowed = true
				break
			}
		}

		// bail out if any of the questions don't contain an allowed
		// hostname
		if !allowed {
			logger.Info("dropping DNS request", zap.ByteString("question", dns.Questions[i].Name))
			return false
		}
	}

	return true
}

func newDNSResponseCallback(f *FilterManager) nfqueue.HookFunc {
	logger := f.logger.With(zap.Uint16("queue.num", f.queueNum))

	return func(attr nfqueue.Attribute) int {
		if attr.PacketID == nil {
			return 0
		}
		if attr.CtInfo == nil {
			return 0
		}
		if attr.Payload == nil {
			return 0
		}

		// since DNS requests are filtered above, we only process
		// DNS responses of established packets to make sure a
		// local attacker can't connect to disallowed IPs by
		// sending a DNS response with an attacker specified IP
		// as an answer, thereby allowing that IP
		if *attr.CtInfo != state_established {
			logger.Warn("dropping DNS response with that is not from an established connection")

			if err := f.dnsRespNF.SetVerdict(*attr.PacketID, nfqueue.NfDrop); err != nil {
				logger.Error("error setting verdict", zap.String("error", err.Error()))
			}
			return 0
		}

		dns, connID, err := parseDNSPacket(*attr.Payload, f.ipv6, true)
		if err != nil {
			logger.Error("error parsing DNS packet", zap.String("error", err.Error()))
			return 0
		}

		// TODO: optimize
		var connFilter *filter
		for _, filter := range f.filters {
			if filter.connections.EntryExists(connID) {
				connFilter = filter
			}
		}
		if connFilter == nil {
			logger.Warn("dropping DNS request from unknown connection")

			if err := f.dnsRespNF.SetVerdict(*attr.PacketID, nfqueue.NfDrop); err != nil {
				logger.Error("error setting verdict", zap.String("error", err.Error()))
			}
			return 0
		}
		logger.Debug("removing connection", zap.String("conn_id", connID))
		connFilter.connections.RemoveEntry(connID)

		// validate DNS response questions are for allowed
		// hostnames, drop them otherwise; responses for disallowed
		// hostnames should never happen in theory, because we
		// block requests for disallowed hostnames but it doesn't
		// hurt to check
		if !connFilter.validateDNSQuestions(logger, dns) {
			if err := f.dnsRespNF.SetVerdict(*attr.PacketID, nfqueue.NfDrop); err != nil {
				logger.Error("error setting verdict", zap.String("error", err.Error()))
			}
			return 0
		}

		if dns.ANCount > 0 {
			for _, answer := range dns.Answers {
				if answer.Type == layers.DNSTypeA || answer.Type == layers.DNSTypeAAAA {
					// temporarily add A and AAAA answers to
					// allowed IP list
					ipStr := answer.IP.String()
					logger.Info("allowing IP from DNS reply", zap.String("answer.ip", ipStr), zap.Uint32("answer.ttl", answer.TTL))

					connFilter.allowedIPs.AddEntry(ipStr, false, time.Duration(answer.TTL)*time.Second)
				} else if answer.Type == layers.DNSTypeSRV {
					// temporarily add SRV answers to allowed
					// hostnames list
					logger.Info("allowing hostname from DNS reply", zap.ByteString("answer.name", answer.SRV.Name), zap.Uint32("answer.ttl", answer.TTL))

					connFilter.additionalHostnames.AddEntry(string(answer.SRV.Name), false, time.Duration(answer.TTL)*time.Second)
				}
			}
		}

		if err := f.dnsRespNF.SetVerdict(*attr.PacketID, nfqueue.NfAccept); err != nil {
			logger.Error("error setting verdict", zap.String("error", err.Error()))
			return 0
		}

		return 0
	}
}

func newGenericCallback(f *filter) nfqueue.HookFunc {
	logger := f.logger.With(zap.Uint16("queue.num", f.opts.TrafficQueue))

	return func(attr nfqueue.Attribute) int {
		if attr.PacketID == nil {
			return 0
		}
		if attr.Payload == nil {
			return 0
		}

		var (
			ip4     layers.IPv4
			ip6     layers.IPv6
			parser  *gopacket.DecodingLayerParser
			decoded = make([]gopacket.LayerType, 1)
		)

		// parse packet
		if !f.opts.IPv6 {
			parser = gopacket.NewDecodingLayerParser(layers.LayerTypeIPv4)
			parser.IgnoreUnsupported = true
			parser.SetDecodingLayerContainer(gopacket.DecodingLayerArray(nil))
			parser.AddDecodingLayer(&ip4)
		} else {
			parser = gopacket.NewDecodingLayerParser(layers.LayerTypeIPv6)
			parser.IgnoreUnsupported = true
			parser.SetDecodingLayerContainer(gopacket.DecodingLayerArray(nil))
			parser.AddDecodingLayer(&ip6)
		}

		if err := parser.DecodeLayers(*attr.Payload, &decoded); err != nil {
			logger.Error("error parsing packet", zap.String("error", err.Error()))
			return 0
		}

		// get source and destination IP
		var src, dst string
		if decoded[0] == layers.LayerTypeIPv4 {
			src = ip4.SrcIP.String()
			dst = ip4.DstIP.String()
		} else if decoded[0] == layers.LayerTypeIPv6 {
			src = ip6.SrcIP.String()
			dst = ip6.DstIP.String()
		}

		// validate that either the source or destination IP is allowed
		var verdict int
		if !f.validateIPs(src, dst) {
			logger.Info("dropping packet", zap.String("src_ip", src), zap.String("dst_ip", dst))
			verdict = nfqueue.NfDrop
		} else {
			logger.Info("allowing packet", zap.String("src_ip", src), zap.String("dst_ip", dst))
			verdict = nfqueue.NfAccept
		}

		if err := f.genericNF.SetVerdict(*attr.PacketID, verdict); err != nil {
			logger.Error("error setting verdict", zap.String("error", err.Error()))
		}

		return 0
	}
}

func (f *filter) validateIPs(src, dst string) bool {
	allowed := f.allowedIPs.EntryExists(src)

	// only check the destination IP if the source is not allowed
	if !allowed {
		allowed = f.allowedIPs.EntryExists(dst)
	}

	return allowed
}

func newErrorCallback(logger *zap.Logger) nfqueue.ErrorFunc {
	return func(err error) int {
		logger.Error("netlink error", zap.String("error", err.Error()))

		return 0
	}
}
