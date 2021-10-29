package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"

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

var (
	// TODO: parse from TOML config
	allowedHostnames = []string{
		"deb.debian.org",
		"download.mono-project.com",
		"download.docker.com",
		"packages.microsoft.com",
	}

	// TODO: replace with net/netaddr when it gets released in the
	// standard library (1.18?)
	allowedIPs          *TimedCache
	additionalHostnames *TimedCache
)

type hookCreator func(*zap.Logger, uint16, bool, *nfqueue.Nfqueue) nfqueue.HookFunc

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		log.Fatalf("error creating logger: %v", err)
	}

	allowedIPs = NewTimedCache(logger)
	additionalHostnames = NewTimedCache(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	dnsNF, err := startNfQueue(ctx, logger, 1000, false, newDNSCallback)
	if err != nil {
		logger.Fatal("error opening nfqueue", zap.String("error", err.Error()))
	}
	defer dnsNF.Close()

	genericNF, err := startNfQueue(ctx, logger, 1001, false, newGenericCallback)
	if err != nil {
		logger.Fatal("error opening nfqueue", zap.String("error", err.Error()))
	}
	defer genericNF.Close()

	logger.Info("started")

	<-ctx.Done()
	logger.Info("stopping")
}

func startNfQueue(ctx context.Context, logger *zap.Logger, queueNum uint16, ipv6 bool, newHook hookCreator) (*nfqueue.Nfqueue, error) {
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

	if err := nf.RegisterWithErrorFunc(ctx, newHook(logger, queueNum, ipv6, nf), newErrorCallback(logger)); err != nil {
		nf.Close()
		return nil, fmt.Errorf("error registering nfqueue: %v", err)
	}

	return nf, nil
}

func newDNSCallback(l *zap.Logger, queueNum uint16, ipv6 bool, nf *nfqueue.Nfqueue) nfqueue.HookFunc {
	logger := l.With(zap.Uint16("queue.num", queueNum))

	return func(attr nfqueue.Attribute) int {
		if attr.PacketID == nil {
			return 0
		}
		if attr.Payload == nil {
			return 0
		}
		if attr.CtInfo == nil {
			return 0
		}

		// validate connection state
		if *attr.CtInfo != state_new && *attr.CtInfo != state_established {
			logger.Warn("dropping packer with unknown connection state")

			if err := nf.SetVerdict(*attr.PacketID, nfqueue.NfDrop); err != nil {
				logger.Error("error setting verdict", zap.String("error", err.Error()))
			}
			return 0
		}

		var (
			ip4     layers.IPv4
			ip6     layers.IPv6
			udp     layers.UDP
			tcp     layers.TCP
			dns     layers.DNS
			parser  *gopacket.DecodingLayerParser
			decoded = make([]gopacket.LayerType, 3)
		)

		// parse packet
		if !ipv6 {
			parser = gopacket.NewDecodingLayerParser(layers.LayerTypeIPv4, &ip4, &udp, &tcp, &dns)
		} else {
			parser = gopacket.NewDecodingLayerParser(layers.LayerTypeIPv6, &ip6, &udp, &tcp, &dns)
		}

		if err := parser.DecodeLayers(*attr.Payload, &decoded); err != nil {
			logger.Error("error parsing DNS packet", zap.String("error", err.Error()))
			return 0
		}

		// if this is a packet from a new connection, assume this is a
		// DNS request
		if *attr.CtInfo == state_new {
			// validate DNS request questions are for allowed
			// hostnames, drop them otherwise
			if dns.QDCount > 0 {
				if !validateDNSQuestions(logger, &dns) {
					if err := nf.SetVerdict(*attr.PacketID, nfqueue.NfDrop); err != nil {
						logger.Error("error setting verdict", zap.String("error", err.Error()))
					}
					return 0
				}
			} else {
				// drop DNS requests with no questions; this probably
				// doesn't happen in practice but doesn't hurt to
				// handle this case
				logger.Info("dropping dns request with no questions")

				if err := nf.SetVerdict(*attr.PacketID, nfqueue.NfDrop); err != nil {
					logger.Error("error setting verdict", zap.String("error", err.Error()))
				}
				return 0
			}

			questions := make([]string, len(dns.Questions))
			for i := range dns.Questions {
				questions[i] = string(dns.Questions[i].Name) + ": " + dns.Questions[i].Type.String()
			}
			logger.Info("allowing DNS request", zap.Strings("questions", questions))
		}

		// if this is a packet from an established connection, assume
		// this is a DNS request
		if *attr.CtInfo == state_established {
			// since DNS requests are filtered above, we only process
			// DNS responses of established packets to make sure a
			// local attacker can't connect to disallowed IPs by
			// sending a DNS response with an attacker specified IP
			if dns.ANCount > 0 {
				for _, answer := range dns.Answers {
					if answer.Type == layers.DNSTypeA || answer.Type == layers.DNSTypeAAAA {
						ipStr := answer.IP.String()
						logger.Info("allowing IP from DNS reply", zap.String("answer.ip", ipStr), zap.Uint32("answer.ttl", answer.TTL))

						allowedIPs.AddRecord(ipStr, answer.TTL)
					} else if answer.Type == layers.DNSTypeSRV {
						logger.Info("allowing hostname from DNS reply", zap.ByteString("answer.name", answer.SRV.Name), zap.Uint32("answer.ttl", answer.TTL))

						additionalHostnames.AddRecord(string(answer.SRV.Name), answer.TTL)
					}
				}
			}
		}

		if err := nf.SetVerdict(*attr.PacketID, nfqueue.NfAccept); err != nil {
			logger.Error("error setting verdict", zap.String("error", err.Error()))
		}

		return 0
	}
}

func validateDNSQuestions(logger *zap.Logger, dns *layers.DNS) bool {
	var allowed bool
	for i := range dns.Questions {
		allowed = false
		for j := range allowedHostnames {
			if strings.HasSuffix(string(dns.Questions[i].Name), allowedHostnames[j]) ||
				additionalHostnames.RecordExists(string(dns.Questions[i].Name)) {
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

func newGenericCallback(l *zap.Logger, queueNum uint16, ipv6 bool, nf *nfqueue.Nfqueue) nfqueue.HookFunc {
	logger := l.With(zap.Uint16("queue.num", queueNum))

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
		if !ipv6 {
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
		if !validateIPs(src, dst) {
			logger.Info("dropping packet", zap.String("src_ip", src), zap.String("dst_ip", dst))
			verdict = nfqueue.NfDrop
		} else {
			logger.Info("allowing packet", zap.String("src_ip", src), zap.String("dst_ip", dst))
			verdict = nfqueue.NfAccept
		}

		if err := nf.SetVerdict(*attr.PacketID, verdict); err != nil {
			logger.Error("error setting verdict", zap.String("error", err.Error()))
		}

		return 0
	}
}

func validateIPs(src, dst string) bool {
	allowed := allowedIPs.RecordExists(src)

	// only check the destination IP if the source is not allowed
	if !allowed {
		allowed = allowedIPs.RecordExists(dst)
	}

	return allowed
}

func newErrorCallback(logger *zap.Logger) nfqueue.ErrorFunc {
	return func(err error) int {
		logger.Error("netlink error", zap.String("error", err.Error()))

		return 0
	}
}
