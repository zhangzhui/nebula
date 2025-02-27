package nebula

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rcrowley/go-metrics"
	"github.com/sirupsen/logrus"
	"github.com/slackhq/nebula/cert"
	"github.com/slackhq/nebula/cidr"
	"github.com/slackhq/nebula/config"
	"github.com/slackhq/nebula/firewall"
)

const tcpACK = 0x10
const tcpFIN = 0x01

type FirewallInterface interface {
	AddRule(incoming bool, proto uint8, startPort int32, endPort int32, groups []string, host string, ip *net.IPNet, localIp *net.IPNet, caName string, caSha string) error
}

type conn struct {
	Expires time.Time // Time when this conntrack entry will expire
	Sent    time.Time // If tcp rtt tracking is enabled this will be when Seq was last set
	Seq     uint32    // If tcp rtt tracking is enabled this will be the seq we are looking for an ack

	// record why the original connection passed the firewall, so we can re-validate
	// after ruleset changes. Note, rulesVersion is a uint16 so that these two
	// fields pack for free after the uint32 above
	incoming     bool
	rulesVersion uint16
}

// TODO: need conntrack max tracked connections handling
type Firewall struct {
	Conntrack *FirewallConntrack

	InRules  *FirewallTable
	OutRules *FirewallTable

	InSendReject  bool
	OutSendReject bool

	//TODO: we should have many more options for TCP, an option for ICMP, and mimic the kernel a bit better
	// https://www.kernel.org/doc/Documentation/networking/nf_conntrack-sysctl.txt
	TCPTimeout     time.Duration //linux: 5 days max
	UDPTimeout     time.Duration //linux: 180s max
	DefaultTimeout time.Duration //linux: 600s

	// Used to ensure we don't emit local packets for ips we don't own
	localIps *cidr.Tree4[struct{}]

	rules        string
	rulesVersion uint16

	trackTCPRTT     bool
	metricTCPRTT    metrics.Histogram
	incomingMetrics firewallMetrics
	outgoingMetrics firewallMetrics

	l *logrus.Logger
}

type firewallMetrics struct {
	droppedLocalIP  metrics.Counter
	droppedRemoteIP metrics.Counter
	droppedNoRule   metrics.Counter
}

type FirewallConntrack struct {
	sync.Mutex

	Conns      map[firewall.Packet]*conn
	TimerWheel *TimerWheel[firewall.Packet]
}

type FirewallTable struct {
	TCP      firewallPort
	UDP      firewallPort
	ICMP     firewallPort
	AnyProto firewallPort
}

func newFirewallTable() *FirewallTable {
	return &FirewallTable{
		TCP:      firewallPort{},
		UDP:      firewallPort{},
		ICMP:     firewallPort{},
		AnyProto: firewallPort{},
	}
}

type FirewallCA struct {
	Any     *FirewallRule
	CANames map[string]*FirewallRule
	CAShas  map[string]*FirewallRule
}

type FirewallRule struct {
	// Any makes Hosts, Groups, CIDR and LocalCIDR irrelevant
	Any       bool
	Hosts     map[string]struct{}
	Groups    [][]string
	CIDR      *cidr.Tree4[struct{}]
	LocalCIDR *cidr.Tree4[struct{}]
}

// Even though ports are uint16, int32 maps are faster for lookup
// Plus we can use `-1` for fragment rules
type firewallPort map[int32]*FirewallCA

// NewFirewall creates a new Firewall object. A TimerWheel is created for you from the provided timeouts.
func NewFirewall(l *logrus.Logger, tcpTimeout, UDPTimeout, defaultTimeout time.Duration, c *cert.NebulaCertificate) *Firewall {
	//TODO: error on 0 duration
	var min, max time.Duration

	if tcpTimeout < UDPTimeout {
		min = tcpTimeout
		max = UDPTimeout
	} else {
		min = UDPTimeout
		max = tcpTimeout
	}

	if defaultTimeout < min {
		min = defaultTimeout
	} else if defaultTimeout > max {
		max = defaultTimeout
	}

	localIps := cidr.NewTree4[struct{}]()
	for _, ip := range c.Details.Ips {
		localIps.AddCIDR(&net.IPNet{IP: ip.IP, Mask: net.IPMask{255, 255, 255, 255}}, struct{}{})
	}

	for _, n := range c.Details.Subnets {
		localIps.AddCIDR(n, struct{}{})
	}

	return &Firewall{
		Conntrack: &FirewallConntrack{
			Conns:      make(map[firewall.Packet]*conn),
			TimerWheel: NewTimerWheel[firewall.Packet](min, max),
		},
		InRules:        newFirewallTable(),
		OutRules:       newFirewallTable(),
		TCPTimeout:     tcpTimeout,
		UDPTimeout:     UDPTimeout,
		DefaultTimeout: defaultTimeout,
		localIps:       localIps,
		l:              l,

		metricTCPRTT: metrics.GetOrRegisterHistogram("network.tcp.rtt", nil, metrics.NewExpDecaySample(1028, 0.015)),
		incomingMetrics: firewallMetrics{
			droppedLocalIP:  metrics.GetOrRegisterCounter("firewall.incoming.dropped.local_ip", nil),
			droppedRemoteIP: metrics.GetOrRegisterCounter("firewall.incoming.dropped.remote_ip", nil),
			droppedNoRule:   metrics.GetOrRegisterCounter("firewall.incoming.dropped.no_rule", nil),
		},
		outgoingMetrics: firewallMetrics{
			droppedLocalIP:  metrics.GetOrRegisterCounter("firewall.outgoing.dropped.local_ip", nil),
			droppedRemoteIP: metrics.GetOrRegisterCounter("firewall.outgoing.dropped.remote_ip", nil),
			droppedNoRule:   metrics.GetOrRegisterCounter("firewall.outgoing.dropped.no_rule", nil),
		},
	}
}

func NewFirewallFromConfig(l *logrus.Logger, nc *cert.NebulaCertificate, c *config.C) (*Firewall, error) {
	fw := NewFirewall(
		l,
		c.GetDuration("firewall.conntrack.tcp_timeout", time.Minute*12),
		c.GetDuration("firewall.conntrack.udp_timeout", time.Minute*3),
		c.GetDuration("firewall.conntrack.default_timeout", time.Minute*10),
		nc,
		//TODO: max_connections
	)

	inboundAction := c.GetString("firewall.inbound_action", "drop")
	switch inboundAction {
	case "reject":
		fw.InSendReject = true
	case "drop":
		fw.InSendReject = false
	default:
		l.WithField("action", inboundAction).Warn("invalid firewall.inbound_action, defaulting to `drop`")
		fw.InSendReject = false
	}

	outboundAction := c.GetString("firewall.outbound_action", "drop")
	switch outboundAction {
	case "reject":
		fw.OutSendReject = true
	case "drop":
		fw.OutSendReject = false
	default:
		l.WithField("action", inboundAction).Warn("invalid firewall.outbound_action, defaulting to `drop`")
		fw.OutSendReject = false
	}

	err := AddFirewallRulesFromConfig(l, false, c, fw)
	if err != nil {
		return nil, err
	}

	err = AddFirewallRulesFromConfig(l, true, c, fw)
	if err != nil {
		return nil, err
	}

	return fw, nil
}

// AddRule properly creates the in memory rule structure for a firewall table.
func (f *Firewall) AddRule(incoming bool, proto uint8, startPort int32, endPort int32, groups []string, host string, ip *net.IPNet, localIp *net.IPNet, caName string, caSha string) error {
	// Under gomobile, stringing a nil pointer with fmt causes an abort in debug mode for iOS
	// https://github.com/golang/go/issues/14131
	sIp := ""
	if ip != nil {
		sIp = ip.String()
	}
	lIp := ""
	if localIp != nil {
		lIp = localIp.String()
	}

	// We need this rule string because we generate a hash. Removing this will break firewall reload.
	ruleString := fmt.Sprintf(
		"incoming: %v, proto: %v, startPort: %v, endPort: %v, groups: %v, host: %v, ip: %v, localIp: %v, caName: %v, caSha: %s",
		incoming, proto, startPort, endPort, groups, host, sIp, lIp, caName, caSha,
	)
	f.rules += ruleString + "\n"

	direction := "incoming"
	if !incoming {
		direction = "outgoing"
	}
	f.l.WithField("firewallRule", m{"direction": direction, "proto": proto, "startPort": startPort, "endPort": endPort, "groups": groups, "host": host, "ip": sIp, "localIp": lIp, "caName": caName, "caSha": caSha}).
		Info("Firewall rule added")

	var (
		ft *FirewallTable
		fp firewallPort
	)

	if incoming {
		ft = f.InRules
	} else {
		ft = f.OutRules
	}

	switch proto {
	case firewall.ProtoTCP:
		fp = ft.TCP
	case firewall.ProtoUDP:
		fp = ft.UDP
	case firewall.ProtoICMP:
		fp = ft.ICMP
	case firewall.ProtoAny:
		fp = ft.AnyProto
	default:
		return fmt.Errorf("unknown protocol %v", proto)
	}

	return fp.addRule(startPort, endPort, groups, host, ip, localIp, caName, caSha)
}

// GetRuleHash returns a hash representation of all inbound and outbound rules
func (f *Firewall) GetRuleHash() string {
	sum := sha256.Sum256([]byte(f.rules))
	return hex.EncodeToString(sum[:])
}

// GetRuleHashFNV returns a uint32 FNV-1 hash representation the rules, for use as a metric value
func (f *Firewall) GetRuleHashFNV() uint32 {
	h := fnv.New32a()
	h.Write([]byte(f.rules))
	return h.Sum32()
}

// GetRuleHashes returns both the sha256 and FNV-1 hashes, suitable for logging
func (f *Firewall) GetRuleHashes() string {
	return "SHA:" + f.GetRuleHash() + ",FNV:" + strconv.FormatUint(uint64(f.GetRuleHashFNV()), 10)
}

func AddFirewallRulesFromConfig(l *logrus.Logger, inbound bool, c *config.C, fw FirewallInterface) error {
	var table string
	if inbound {
		table = "firewall.inbound"
	} else {
		table = "firewall.outbound"
	}

	r := c.Get(table)
	if r == nil {
		return nil
	}

	rs, ok := r.([]interface{})
	if !ok {
		return fmt.Errorf("%s failed to parse, should be an array of rules", table)
	}

	for i, t := range rs {
		var groups []string
		r, err := convertRule(l, t, table, i)
		if err != nil {
			return fmt.Errorf("%s rule #%v; %s", table, i, err)
		}

		if r.Code != "" && r.Port != "" {
			return fmt.Errorf("%s rule #%v; only one of port or code should be provided", table, i)
		}

		if r.Host == "" && len(r.Groups) == 0 && r.Group == "" && r.Cidr == "" && r.LocalCidr == "" && r.CAName == "" && r.CASha == "" {
			return fmt.Errorf("%s rule #%v; at least one of host, group, cidr, local_cidr, ca_name, or ca_sha must be provided", table, i)
		}

		if len(r.Groups) > 0 {
			groups = r.Groups
		}

		if r.Group != "" {
			// Check if we have both groups and group provided in the rule config
			if len(groups) > 0 {
				return fmt.Errorf("%s rule #%v; only one of group or groups should be defined, both provided", table, i)
			}

			groups = []string{r.Group}
		}

		var sPort, errPort string
		if r.Code != "" {
			errPort = "code"
			sPort = r.Code
		} else {
			errPort = "port"
			sPort = r.Port
		}

		startPort, endPort, err := parsePort(sPort)
		if err != nil {
			return fmt.Errorf("%s rule #%v; %s %s", table, i, errPort, err)
		}

		var proto uint8
		switch r.Proto {
		case "any":
			proto = firewall.ProtoAny
		case "tcp":
			proto = firewall.ProtoTCP
		case "udp":
			proto = firewall.ProtoUDP
		case "icmp":
			proto = firewall.ProtoICMP
		default:
			return fmt.Errorf("%s rule #%v; proto was not understood; `%s`", table, i, r.Proto)
		}

		var cidr *net.IPNet
		if r.Cidr != "" {
			_, cidr, err = net.ParseCIDR(r.Cidr)
			if err != nil {
				return fmt.Errorf("%s rule #%v; cidr did not parse; %s", table, i, err)
			}
		}

		var localCidr *net.IPNet
		if r.LocalCidr != "" {
			_, localCidr, err = net.ParseCIDR(r.LocalCidr)
			if err != nil {
				return fmt.Errorf("%s rule #%v; local_cidr did not parse; %s", table, i, err)
			}
		}

		err = fw.AddRule(inbound, proto, startPort, endPort, groups, r.Host, cidr, localCidr, r.CAName, r.CASha)
		if err != nil {
			return fmt.Errorf("%s rule #%v; `%s`", table, i, err)
		}
	}

	return nil
}

var ErrInvalidRemoteIP = errors.New("remote IP is not in remote certificate subnets")
var ErrInvalidLocalIP = errors.New("local IP is not in list of handled local IPs")
var ErrNoMatchingRule = errors.New("no matching rule in firewall table")

// Drop returns an error if the packet should be dropped, explaining why. It
// returns nil if the packet should not be dropped.
func (f *Firewall) Drop(packet []byte, fp firewall.Packet, incoming bool, h *HostInfo, caPool *cert.NebulaCAPool, localCache firewall.ConntrackCache) error {
	// Check if we spoke to this tuple, if we did then allow this packet
	if f.inConns(packet, fp, incoming, h, caPool, localCache) {
		return nil
	}

	// Make sure remote address matches nebula certificate
	if remoteCidr := h.remoteCidr; remoteCidr != nil {
		ok, _ := remoteCidr.Contains(fp.RemoteIP)
		if !ok {
			f.metrics(incoming).droppedRemoteIP.Inc(1)
			return ErrInvalidRemoteIP
		}
	} else {
		// Simple case: Certificate has one IP and no subnets
		if fp.RemoteIP != h.vpnIp {
			f.metrics(incoming).droppedRemoteIP.Inc(1)
			return ErrInvalidRemoteIP
		}
	}

	// Make sure we are supposed to be handling this local ip address
	ok, _ := f.localIps.Contains(fp.LocalIP)
	if !ok {
		f.metrics(incoming).droppedLocalIP.Inc(1)
		return ErrInvalidLocalIP
	}

	table := f.OutRules
	if incoming {
		table = f.InRules
	}

	// We now know which firewall table to check against
	if !table.match(fp, incoming, h.ConnectionState.peerCert, caPool) {
		f.metrics(incoming).droppedNoRule.Inc(1)
		return ErrNoMatchingRule
	}

	// We always want to conntrack since it is a faster operation
	f.addConn(packet, fp, incoming)

	return nil
}

func (f *Firewall) metrics(incoming bool) firewallMetrics {
	if incoming {
		return f.incomingMetrics
	} else {
		return f.outgoingMetrics
	}
}

// Destroy cleans up any known cyclical references so the object can be free'd my GC. This should be called if a new
// firewall object is created
func (f *Firewall) Destroy() {
	//TODO: clean references if/when needed
}

func (f *Firewall) EmitStats() {
	conntrack := f.Conntrack
	conntrack.Lock()
	conntrackCount := len(conntrack.Conns)
	conntrack.Unlock()
	metrics.GetOrRegisterGauge("firewall.conntrack.count", nil).Update(int64(conntrackCount))
	metrics.GetOrRegisterGauge("firewall.rules.version", nil).Update(int64(f.rulesVersion))
	metrics.GetOrRegisterGauge("firewall.rules.hash", nil).Update(int64(f.GetRuleHashFNV()))
}

func (f *Firewall) inConns(packet []byte, fp firewall.Packet, incoming bool, h *HostInfo, caPool *cert.NebulaCAPool, localCache firewall.ConntrackCache) bool {
	if localCache != nil {
		if _, ok := localCache[fp]; ok {
			return true
		}
	}
	conntrack := f.Conntrack
	conntrack.Lock()

	// Purge every time we test
	ep, has := conntrack.TimerWheel.Purge()
	if has {
		f.evict(ep)
	}

	c, ok := conntrack.Conns[fp]

	if !ok {
		conntrack.Unlock()
		return false
	}

	if c.rulesVersion != f.rulesVersion {
		// This conntrack entry was for an older rule set, validate
		// it still passes with the current rule set
		table := f.OutRules
		if c.incoming {
			table = f.InRules
		}

		// We now know which firewall table to check against
		if !table.match(fp, c.incoming, h.ConnectionState.peerCert, caPool) {
			if f.l.Level >= logrus.DebugLevel {
				h.logger(f.l).
					WithField("fwPacket", fp).
					WithField("incoming", c.incoming).
					WithField("rulesVersion", f.rulesVersion).
					WithField("oldRulesVersion", c.rulesVersion).
					Debugln("dropping old conntrack entry, does not match new ruleset")
			}
			delete(conntrack.Conns, fp)
			conntrack.Unlock()
			return false
		}

		if f.l.Level >= logrus.DebugLevel {
			h.logger(f.l).
				WithField("fwPacket", fp).
				WithField("incoming", c.incoming).
				WithField("rulesVersion", f.rulesVersion).
				WithField("oldRulesVersion", c.rulesVersion).
				Debugln("keeping old conntrack entry, does match new ruleset")
		}

		c.rulesVersion = f.rulesVersion
	}

	switch fp.Protocol {
	case firewall.ProtoTCP:
		c.Expires = time.Now().Add(f.TCPTimeout)
		if incoming {
			f.checkTCPRTT(c, packet)
		} else {
			setTCPRTTTracking(c, packet)
		}
	case firewall.ProtoUDP:
		c.Expires = time.Now().Add(f.UDPTimeout)
	default:
		c.Expires = time.Now().Add(f.DefaultTimeout)
	}

	conntrack.Unlock()

	if localCache != nil {
		localCache[fp] = struct{}{}
	}

	return true
}

func (f *Firewall) addConn(packet []byte, fp firewall.Packet, incoming bool) {
	var timeout time.Duration
	c := &conn{}

	switch fp.Protocol {
	case firewall.ProtoTCP:
		timeout = f.TCPTimeout
		if !incoming {
			setTCPRTTTracking(c, packet)
		}
	case firewall.ProtoUDP:
		timeout = f.UDPTimeout
	default:
		timeout = f.DefaultTimeout
	}

	conntrack := f.Conntrack
	conntrack.Lock()
	if _, ok := conntrack.Conns[fp]; !ok {
		conntrack.TimerWheel.Advance(time.Now())
		conntrack.TimerWheel.Add(fp, timeout)
	}

	// Record which rulesVersion allowed this connection, so we can retest after
	// firewall reload
	c.incoming = incoming
	c.rulesVersion = f.rulesVersion
	c.Expires = time.Now().Add(timeout)
	conntrack.Conns[fp] = c
	conntrack.Unlock()
}

// Evict checks if a conntrack entry has expired, if so it is removed, if not it is re-added to the wheel
// Caller must own the connMutex lock!
func (f *Firewall) evict(p firewall.Packet) {
	//TODO: report a stat if the tcp rtt tracking was never resolved?
	// Are we still tracking this conn?
	conntrack := f.Conntrack
	t, ok := conntrack.Conns[p]
	if !ok {
		return
	}

	newT := t.Expires.Sub(time.Now())

	// Timeout is in the future, re-add the timer
	if newT > 0 {
		conntrack.TimerWheel.Advance(time.Now())
		conntrack.TimerWheel.Add(p, newT)
		return
	}

	// This conn is done
	delete(conntrack.Conns, p)
}

func (ft *FirewallTable) match(p firewall.Packet, incoming bool, c *cert.NebulaCertificate, caPool *cert.NebulaCAPool) bool {
	if ft.AnyProto.match(p, incoming, c, caPool) {
		return true
	}

	switch p.Protocol {
	case firewall.ProtoTCP:
		if ft.TCP.match(p, incoming, c, caPool) {
			return true
		}
	case firewall.ProtoUDP:
		if ft.UDP.match(p, incoming, c, caPool) {
			return true
		}
	case firewall.ProtoICMP:
		if ft.ICMP.match(p, incoming, c, caPool) {
			return true
		}
	}

	return false
}

func (fp firewallPort) addRule(startPort int32, endPort int32, groups []string, host string, ip *net.IPNet, localIp *net.IPNet, caName string, caSha string) error {
	if startPort > endPort {
		return fmt.Errorf("start port was lower than end port")
	}

	for i := startPort; i <= endPort; i++ {
		if _, ok := fp[i]; !ok {
			fp[i] = &FirewallCA{
				CANames: make(map[string]*FirewallRule),
				CAShas:  make(map[string]*FirewallRule),
			}
		}

		if err := fp[i].addRule(groups, host, ip, localIp, caName, caSha); err != nil {
			return err
		}
	}

	return nil
}

func (fp firewallPort) match(p firewall.Packet, incoming bool, c *cert.NebulaCertificate, caPool *cert.NebulaCAPool) bool {
	// We don't have any allowed ports, bail
	if fp == nil {
		return false
	}

	var port int32

	if p.Fragment {
		port = firewall.PortFragment
	} else if incoming {
		port = int32(p.LocalPort)
	} else {
		port = int32(p.RemotePort)
	}

	if fp[port].match(p, c, caPool) {
		return true
	}

	return fp[firewall.PortAny].match(p, c, caPool)
}

func (fc *FirewallCA) addRule(groups []string, host string, ip, localIp *net.IPNet, caName, caSha string) error {
	fr := func() *FirewallRule {
		return &FirewallRule{
			Hosts:     make(map[string]struct{}),
			Groups:    make([][]string, 0),
			CIDR:      cidr.NewTree4[struct{}](),
			LocalCIDR: cidr.NewTree4[struct{}](),
		}
	}

	if caSha == "" && caName == "" {
		if fc.Any == nil {
			fc.Any = fr()
		}

		return fc.Any.addRule(groups, host, ip, localIp)
	}

	if caSha != "" {
		if _, ok := fc.CAShas[caSha]; !ok {
			fc.CAShas[caSha] = fr()
		}
		err := fc.CAShas[caSha].addRule(groups, host, ip, localIp)
		if err != nil {
			return err
		}
	}

	if caName != "" {
		if _, ok := fc.CANames[caName]; !ok {
			fc.CANames[caName] = fr()
		}
		err := fc.CANames[caName].addRule(groups, host, ip, localIp)
		if err != nil {
			return err
		}
	}

	return nil
}

func (fc *FirewallCA) match(p firewall.Packet, c *cert.NebulaCertificate, caPool *cert.NebulaCAPool) bool {
	if fc == nil {
		return false
	}

	if fc.Any.match(p, c) {
		return true
	}

	if t, ok := fc.CAShas[c.Details.Issuer]; ok {
		if t.match(p, c) {
			return true
		}
	}

	s, err := caPool.GetCAForCert(c)
	if err != nil {
		return false
	}

	return fc.CANames[s.Details.Name].match(p, c)
}

func (fr *FirewallRule) addRule(groups []string, host string, ip *net.IPNet, localIp *net.IPNet) error {
	if fr.Any {
		return nil
	}

	if fr.isAny(groups, host, ip, localIp) {
		fr.Any = true
		// If it's any we need to wipe out any pre-existing rules to save on memory
		fr.Groups = make([][]string, 0)
		fr.Hosts = make(map[string]struct{})
		fr.CIDR = cidr.NewTree4[struct{}]()
		fr.LocalCIDR = cidr.NewTree4[struct{}]()
	} else {
		if len(groups) > 0 {
			fr.Groups = append(fr.Groups, groups)
		}

		if host != "" {
			fr.Hosts[host] = struct{}{}
		}

		if ip != nil {
			fr.CIDR.AddCIDR(ip, struct{}{})
		}

		if localIp != nil {
			fr.LocalCIDR.AddCIDR(localIp, struct{}{})
		}
	}

	return nil
}

func (fr *FirewallRule) isAny(groups []string, host string, ip, localIp *net.IPNet) bool {
	if len(groups) == 0 && host == "" && ip == nil && localIp == nil {
		return true
	}

	for _, group := range groups {
		if group == "any" {
			return true
		}
	}

	if host == "any" {
		return true
	}

	if ip != nil && ip.Contains(net.IPv4(0, 0, 0, 0)) {
		return true
	}

	if localIp != nil && localIp.Contains(net.IPv4(0, 0, 0, 0)) {
		return true
	}

	return false
}

func (fr *FirewallRule) match(p firewall.Packet, c *cert.NebulaCertificate) bool {
	if fr == nil {
		return false
	}

	// Shortcut path for if groups, hosts, or cidr contained an `any`
	if fr.Any {
		return true
	}

	// Need any of group, host, or cidr to match
	for _, sg := range fr.Groups {
		found := false

		for _, g := range sg {
			if _, ok := c.Details.InvertedGroups[g]; !ok {
				found = false
				break
			}

			found = true
		}

		if found {
			return true
		}
	}

	if fr.Hosts != nil {
		if _, ok := fr.Hosts[c.Details.Name]; ok {
			return true
		}
	}

	if fr.CIDR != nil {
		ok, _ := fr.CIDR.Contains(p.RemoteIP)
		if ok {
			return true
		}
	}

	if fr.LocalCIDR != nil {
		ok, _ := fr.LocalCIDR.Contains(p.LocalIP)
		if ok {
			return true
		}
	}

	// No host, group, or cidr matched, bye bye
	return false
}

type rule struct {
	Port      string
	Code      string
	Proto     string
	Host      string
	Group     string
	Groups    []string
	Cidr      string
	LocalCidr string
	CAName    string
	CASha     string
}

func convertRule(l *logrus.Logger, p interface{}, table string, i int) (rule, error) {
	r := rule{}

	m, ok := p.(map[interface{}]interface{})
	if !ok {
		return r, errors.New("could not parse rule")
	}

	toString := func(k string, m map[interface{}]interface{}) string {
		v, ok := m[k]
		if !ok {
			return ""
		}
		return fmt.Sprintf("%v", v)
	}

	r.Port = toString("port", m)
	r.Code = toString("code", m)
	r.Proto = toString("proto", m)
	r.Host = toString("host", m)
	r.Cidr = toString("cidr", m)
	r.LocalCidr = toString("local_cidr", m)
	r.CAName = toString("ca_name", m)
	r.CASha = toString("ca_sha", m)

	// Make sure group isn't an array
	if v, ok := m["group"].([]interface{}); ok {
		if len(v) > 1 {
			return r, errors.New("group should contain a single value, an array with more than one entry was provided")
		}

		l.Warnf("%s rule #%v; group was an array with a single value, converting to simple value", table, i)
		m["group"] = v[0]
	}
	r.Group = toString("group", m)

	if rg, ok := m["groups"]; ok {
		switch reflect.TypeOf(rg).Kind() {
		case reflect.Slice:
			v := reflect.ValueOf(rg)
			r.Groups = make([]string, v.Len())
			for i := 0; i < v.Len(); i++ {
				r.Groups[i] = v.Index(i).Interface().(string)
			}
		case reflect.String:
			r.Groups = []string{rg.(string)}
		default:
			r.Groups = []string{fmt.Sprintf("%v", rg)}
		}
	}

	return r, nil
}

func parsePort(s string) (startPort, endPort int32, err error) {
	if s == "any" {
		startPort = firewall.PortAny
		endPort = firewall.PortAny

	} else if s == "fragment" {
		startPort = firewall.PortFragment
		endPort = firewall.PortFragment

	} else if strings.Contains(s, `-`) {
		sPorts := strings.SplitN(s, `-`, 2)
		sPorts[0] = strings.Trim(sPorts[0], " ")
		sPorts[1] = strings.Trim(sPorts[1], " ")

		if len(sPorts) != 2 || sPorts[0] == "" || sPorts[1] == "" {
			return 0, 0, fmt.Errorf("appears to be a range but could not be parsed; `%s`", s)
		}

		rStartPort, err := strconv.Atoi(sPorts[0])
		if err != nil {
			return 0, 0, fmt.Errorf("beginning range was not a number; `%s`", sPorts[0])
		}

		rEndPort, err := strconv.Atoi(sPorts[1])
		if err != nil {
			return 0, 0, fmt.Errorf("ending range was not a number; `%s`", sPorts[1])
		}

		startPort = int32(rStartPort)
		endPort = int32(rEndPort)

		if startPort == firewall.PortAny {
			endPort = firewall.PortAny
		}

	} else {
		rPort, err := strconv.Atoi(s)
		if err != nil {
			return 0, 0, fmt.Errorf("was not a number; `%s`", s)
		}
		startPort = int32(rPort)
		endPort = startPort
	}

	return
}

// TODO: write tests for these
func setTCPRTTTracking(c *conn, p []byte) {
	if c.Seq != 0 {
		return
	}

	ihl := int(p[0]&0x0f) << 2

	// Don't track FIN packets
	if p[ihl+13]&tcpFIN != 0 {
		return
	}

	c.Seq = binary.BigEndian.Uint32(p[ihl+4 : ihl+8])
	c.Sent = time.Now()
}

func (f *Firewall) checkTCPRTT(c *conn, p []byte) bool {
	if c.Seq == 0 {
		return false
	}

	ihl := int(p[0]&0x0f) << 2
	if p[ihl+13]&tcpACK == 0 {
		return false
	}

	// Deal with wrap around, signed int cuts the ack window in half
	// 0 is a bad ack, no data acknowledged
	// positive number is a bad ack, ack is over half the window away
	if int32(c.Seq-binary.BigEndian.Uint32(p[ihl+8:ihl+12])) >= 0 {
		return false
	}

	f.metricTCPRTT.Update(time.Since(c.Sent).Nanoseconds())
	c.Seq = 0
	return true
}
