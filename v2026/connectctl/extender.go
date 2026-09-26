package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	mathrand "math/rand"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/extender"
	"github.com/urnetwork/connect/v2026/gossip"
)

// The standalone extender (EXTENDER.md G4).
//
// It is the provider extender role of G2 and G3 without a provider: the
// extender server on its three carriers, the gossip node with the in-process
// listener and the feed server behind the reserved services (A8, D2, D4), and
// the activation loop that proves the carriers to the operator and publishes
// what comes back (G3).
//
// Everything is derived from one `--api_url`. The family activation urls come
// from it by the sdk's label suffix rule, and the extender dns name and the
// network host -- which names the gossip topic and gates which records this
// directory accepts (B2, D1) -- come from connect's shared label rule, so this
// command and an sdk url-only space key the same network the same way.
//
// One identity key is the whole identity (B1): it signs the certificate
// authority the carrier leaves are issued under, it is what the operator signs
// a record for, and the mesh peer id is derived from it. It is persisted at
// `--extender_key_file` and created there when absent.

// The directory file under --state_dir, beside the other dot files the sdk
// keeps (E1, F1).
const extenderDirectoryFileName = ".extenders"

// The identity key file under --state_dir when --extender_key_file is not
// given (B1).
const extenderKeyFileName = ".extender_key"

// Budget of the startup hello that seeds the root keys, and of the wait for the
// carriers to bind.
const extenderStartTimeout = 30 * time.Second

// How often the admission counts are read for the log (A12). A refusal alone
// does not wake the loop -- under a flood that would be a line per refused
// connection -- so the counts are logged when they changed, at most this
// often.
const extenderAdmissionLogTimeout = time.Minute

type extenderOptions struct {
	jwt      string
	apiUrl   string
	keyFile  string
	stateDir string

	tcpPort int
	udpPort int
	dnsPort int
	// also bind the dns carrier on 53, which needs privilege on most hosts
	// (L2). The bind is never required: a failure leaves the carrier on its
	// unprivileged port.
	dnsPrivilegedPort bool
	// operator patterns this extender may forward to, on top of the api host
	// and one wildcard level under it (A5)
	allowedHosts []string
	// the other extenders this one relays every forward to, which makes it an
	// NLayer extender (A11). Empty forwards to the destination. A private
	// hop's secret, read from its file at start, lives here and nowhere else.
	nlayerHops []*connect.ExtenderConfig
	// the admission limits of A12, which extenderOptionsFromOpts fills from
	// the extender defaults and the flags; zero disables each, which is what
	// a test that builds the options itself gets
	admissionSubnetsPerMinute          int
	admissionActionsPerSubnetPerMinute int
	// the source prefixes exempt from both limits: the fronts of an NLayer
	// hop, which rate-limit their own clients (A12)
	admissionUnlimitedSources []netip.Prefix

	// Listen and ListenPacket, when set, bind the carriers. The test binds
	// ephemeral loopback sockets through them; nil binds the configured ports.
	listen       func(network string, address string) (net.Listener, error)
	listenPacket func(network string, address string) (net.PacketConn, error)
	// dialContext, when set, is the inner dial of every control request this
	// command makes. The test maps its synthetic operator names to loopback.
	dialContext connect.DialContextFunction
	// configureNetworkClient, when set, adjusts the network client settings
	// before it is built. The test installs an in-process resolver so the dns
	// bootstrap never leaves the machine.
	configureNetworkClient func(settings *connect.ExtenderNetworkClientSettings)
	// onStart, when set, receives the running extender once every part is up.
	// The test reads the live objects through it.
	onStart func(run *extenderRun)
	// admissionLogTimeout, when set, replaces extenderAdmissionLogTimeout as
	// how often the admission counts are read for the log. The test reads
	// them without waiting out the minute.
	admissionLogTimeout time.Duration
}

// Reads the extender command's flags. Zero ports take the fixed carrier ports
// of A1.
func extenderOptionsFromOpts(opts docopt.Opts) (*extenderOptions, error) {
	jwt, err := opts.String("--jwt")
	if err != nil {
		return nil, fmt.Errorf("the extender needs --jwt")
	}
	apiUrl, err := opts.String("--api_url")
	if err != nil {
		apiUrl = DefaultApiUrl
	}
	defaultServerSettings := extender.DefaultExtenderSettings()
	options := &extenderOptions{
		jwt:                                jwt,
		apiUrl:                             apiUrl,
		tcpPort:                            connect.ExtenderTcpPort,
		udpPort:                            connect.ExtenderQuicPort,
		dnsPort:                            connect.ExtenderDnsPort,
		admissionSubnetsPerMinute:          defaultServerSettings.AdmissionSubnetsPerMinute,
		admissionActionsPerSubnetPerMinute: defaultServerSettings.AdmissionActionsPerSubnetPerMinute,
	}
	if keyFile, err := opts.String("--extender_key_file"); err == nil {
		options.keyFile = keyFile
	}
	if stateDir, err := opts.String("--state_dir"); err == nil {
		options.stateDir = stateDir
	}
	for flag, port := range map[string]*int{
		"--listen_tcp": &options.tcpPort,
		"--listen_udp": &options.udpPort,
		"--listen_dns": &options.dnsPort,
	} {
		value, err := opts.Int(flag)
		if err != nil {
			continue
		}
		if value <= 0 || 65535 < value {
			return nil, fmt.Errorf("%s must be a port", flag)
		}
		*port = value
	}
	if dnsPrivilegedPort, err := opts.Bool("--dns_privileged_port"); err == nil {
		options.dnsPrivilegedPort = dnsPrivilegedPort
	}
	if allowedHosts, ok := opts["--allowed_host"].([]string); ok {
		for _, allowedHost := range allowedHosts {
			if allowedHost = strings.TrimSpace(allowedHost); allowedHost != "" {
				options.allowedHosts = append(options.allowedHosts, allowedHost)
			}
		}
	}
	// in flag order, so the flag an error names does not depend on which of
	// two bad values was read first
	for _, limit := range []struct {
		flag  string
		value *int
	}{
		{flag: "--admission_subnets_per_minute", value: &options.admissionSubnetsPerMinute},
		{flag: "--admission_actions_per_subnet_per_minute", value: &options.admissionActionsPerSubnetPerMinute},
	} {
		value, err := opts.Int(limit.flag)
		if err != nil {
			if _, given := opts[limit.flag].(string); given {
				return nil, fmt.Errorf("%s must be a count", limit.flag)
			}
			continue
		}
		if value < 0 {
			return nil, fmt.Errorf("%s must be a count, 0 to disable", limit.flag)
		}
		*limit.value = value
	}
	if unlimitedSources, ok := opts["--admission_unlimited_source"].([]string); ok {
		for i, unlimitedSource := range unlimitedSources {
			prefix, err := netip.ParsePrefix(strings.TrimSpace(unlimitedSource))
			if err != nil {
				// the flag and its place, not the value
				return nil, fmt.Errorf("--admission_unlimited_source %d is not a cidr prefix", i)
			}
			options.admissionUnlimitedSources = append(options.admissionUnlimitedSources, prefix.Masked())
		}
	}
	if hopSpecs, ok := opts["--nlayer-hop"].([]string); ok {
		for i, hopSpec := range hopSpecs {
			hop, err := parseExtenderHopSpec(hopSpec)
			if err != nil {
				// the spec itself is not repeated, since it can carry a secret
				return nil, fmt.Errorf("--nlayer-hop %d: %w", i, err)
			}
			options.nlayerHops = append(options.nlayerHops, hop)
		}
	}
	return options, nil
}

// Reads one --nlayer-hop, an extender this one relays forwards to (A11):
//
//	[<carrier>://]<ip>[:<port>][?key=<hex>&secret_file=<path>&sni=<name>&tld=<tld>&fragment&reorder]
//
// The carrier is tcp, quic or dns, and tcp when omitted; the port defaults to
// the carrier's fixed port (A1). key pins the hop's identity leaf (B3), which
// is how a hop is known to be the extender it claims. The secret that signs
// the header of a private hop (A4) is the trimmed content of the file
// secret_file names, read here, once, at start: a process's arguments are
// readable by every user of the host, so a secret is never one, and a spec
// that carries one in its address is refused. sni is the outer server name,
// one random spoof name when omitted and no name at all while the spoof list
// is empty (A10). tld is the encoding tld of the dns carrier, with its trailing
// dot added when missing; fragment and reorder apply to tcp. A hop is dialed
// by address, so it is named by one. No error repeats the spec, and none
// carries what the secret file holds.
func parseExtenderHopSpec(hopSpec string) (*connect.ExtenderConfig, error) {
	trimmedSpec := strings.TrimSpace(hopSpec)
	if trimmedSpec == "" {
		return nil, fmt.Errorf("the hop is empty")
	}
	if !strings.Contains(trimmedSpec, "://") {
		trimmedSpec = connect.ExtenderCarrierTcp + "://" + trimmedSpec
	}
	// user information in the address is a secret in the arguments; it is
	// refused before the url is parsed, so no parse error can repeat it
	authority := trimmedSpec[strings.Index(trimmedSpec, "://")+len("://"):]
	if i := strings.IndexAny(authority, "/?#"); 0 <= i {
		authority = authority[:i]
	}
	if strings.Contains(authority, "@") {
		return nil, fmt.Errorf(
			"the hop carries user information, and a secret there is readable by every user of the host; put the secret in a file and name it with secret_file",
		)
	}
	hopUrl, err := url.Parse(trimmedSpec)
	if err != nil {
		// the url error repeats the input
		if urlErr, ok := err.(*url.Error); ok {
			return nil, urlErr.Err
		}
		return nil, fmt.Errorf("the hop is not a url")
	}

	connectMode, ok := connect.ExtenderConnectModeForCarrier(hopUrl.Scheme)
	if !ok {
		return nil, fmt.Errorf("carrier %q is not tcp, quic or dns", hopUrl.Scheme)
	}
	if hopUrl.Path != "" && hopUrl.Path != "/" {
		return nil, fmt.Errorf("the hop has a path")
	}
	if hopUrl.Fragment != "" {
		return nil, fmt.Errorf("the hop has a fragment")
	}
	ip, err := netip.ParseAddr(hopUrl.Hostname())
	if err != nil {
		return nil, fmt.Errorf("%q is not an ip address", hopUrl.Hostname())
	}
	profile := connect.ExtenderProfile{
		ConnectMode: connectMode,
	}
	switch connectMode {
	case connect.ExtenderConnectModeQuic:
		profile.Port = connect.ExtenderQuicPort
	case connect.ExtenderConnectModeDns:
		profile.Port = connect.ExtenderDnsPort
		profile.DnsTld = connect.DefaultExtenderDnsTld
	default:
		profile.Port = connect.ExtenderTcpPort
	}
	if portStr := hopUrl.Port(); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 || 65535 < port {
			return nil, fmt.Errorf("%q is not a port", portStr)
		}
		profile.Port = port
	}
	hop := &connect.ExtenderConfig{
		Ip: ip,
	}

	values, err := url.ParseQuery(hopUrl.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("the hop parameters do not parse")
	}
	serverNameSet := false
	secretFilePath := ""
	for name, parameterValues := range values {
		if len(parameterValues) != 1 {
			return nil, fmt.Errorf("parameter %q is given %d times", name, len(parameterValues))
		}
		value := strings.TrimSpace(parameterValues[0])
		switch name {
		case "key":
			publicKey, err := connect.ParseExtenderPublicKeyHex(value)
			if err != nil {
				return nil, fmt.Errorf("key: %w", err)
			}
			hop.PublicKey = publicKey
		case "secret_file":
			if value == "" {
				return nil, fmt.Errorf("secret_file names no file")
			}
			secretFilePath = value
		case "sni":
			profile.ServerName = value
			serverNameSet = true
		case "tld":
			if connectMode != connect.ExtenderConnectModeDns {
				return nil, fmt.Errorf("tld applies to the dns carrier only")
			}
			if value == "" {
				return nil, fmt.Errorf("tld is empty")
			}
			if !strings.HasSuffix(value, ".") {
				value += "."
			}
			profile.DnsTld = value
		case "fragment", "reorder":
			if connectMode != connect.ExtenderConnectModeTcpTls {
				return nil, fmt.Errorf("%s applies to the tcp carrier only", name)
			}
			// a bare flag is on
			enabled := true
			if value != "" {
				if enabled, err = strconv.ParseBool(value); err != nil {
					return nil, fmt.Errorf("%s is not a boolean", name)
				}
			}
			if name == "fragment" {
				profile.Fragment = enabled
			} else {
				profile.Reorder = enabled
			}
		default:
			return nil, fmt.Errorf("parameter %q is not known", name)
		}
	}
	if !serverNameSet {
		// the name a client dialer would present (A10): never the operator's,
		// and none at all while the list is empty
		if spoofDomains := connect.SpoofDomains(); 0 < len(spoofDomains) {
			profile.ServerName = spoofDomains[mathrand.Intn(len(spoofDomains))]
		}
	}
	if secretFilePath != "" {
		// read last, once the rest of the spec is known good, and kept in
		// memory only
		secretBytes, err := os.ReadFile(secretFilePath)
		if err != nil {
			// the path error names the path and the cause, never content
			var pathErr *fs.PathError
			if errors.As(err, &pathErr) {
				err = pathErr.Err
			}
			return nil, fmt.Errorf("secret_file %q cannot be read: %w", secretFilePath, err)
		}
		hop.Secret = strings.TrimSpace(string(secretBytes))
		if hop.Secret == "" {
			return nil, fmt.Errorf("secret_file %q holds no secret", secretFilePath)
		}
	}
	hop.Profile = profile
	return hop, nil
}

// One hop as the log names it: carrier and address, and whether it is private
// or pinned. The secret never appears.
func extenderHopDescription(hop *connect.ExtenderConfig) string {
	description := fmt.Sprintf(
		"%s %s",
		connect.ExtenderCarrierForConnectMode(hop.Profile.ConnectMode),
		net.JoinHostPort(hop.Ip.String(), strconv.Itoa(hop.Profile.Port)),
	)
	if hop.Secret != "" {
		description += " private"
	}
	if 0 < len(hop.PublicKey) {
		description += " pinned"
	}
	return description
}

// One running extender: every part, and the shutdown that releases them in the
// reverse order they were built.
type extenderRun struct {
	options *extenderOptions

	publicKey ed25519.PublicKey
	// the host of --api_url, and the space host under it, which is what the
	// records and the topic are keyed by
	apiHost     string
	networkHost string

	clientStrategy *connect.ClientStrategy
	directory      *connect.ExtenderDirectory
	networkClient  *connect.ExtenderNetworkClient
	listener       *gossip.InProcessListener
	feedServer     *gossip.FeedServer
	node           *gossip.Node
	server         *extender.ExtenderServer
	activator      *connect.ExtenderActivator
	pingReporter   *connect.ExtenderPingReporter
	peerPinger     *connect.ExtenderPeerPinger

	serveDone chan error
	closers   []func()
}

// extenderCommand is the command entry point: the flags, the signal context,
// and the exit status of a startup failure.
func extenderCommand(opts docopt.Opts) {
	options, err := extenderOptionsFromOpts(opts)
	if err != nil {
		Err.Printf("%s", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runExtender(ctx, options); err != nil {
		Err.Printf("extender: %s", err)
		os.Exit(1)
	}
}

// runExtender is the extender command. It serves until the context is done,
// which main wires to SIGINT and SIGTERM.
func runExtender(ctx context.Context, options *extenderOptions) error {
	run, err := newExtenderRun(ctx, options)
	if err != nil {
		return err
	}
	defer run.close()
	return run.run(ctx)
}

// Builds every part of the extender and starts it. On any failure the parts
// already built are released before returning.
func newExtenderRun(ctx context.Context, options *extenderOptions) (*extenderRun, error) {
	apiHost, err := connect.ExtenderApiHostName(options.apiUrl)
	if err != nil {
		return nil, err
	}
	run := &extenderRun{
		options:     options,
		apiHost:     apiHost,
		networkHost: connect.ExtenderNetworkHostName(apiHost),
		serveDone:   make(chan error, 1),
	}
	success := false
	defer func() {
		if !success {
			run.close()
		}
	}()

	keySeed, err := run.identityKeySeed()
	if err != nil {
		return nil, err
	}
	if run.publicKey, err = connect.ExtenderPublicKeyFromSeed(keySeed); err != nil {
		return nil, err
	}
	Out.Printf("extender public key: %s", hex.EncodeToString(run.publicKey))

	// direct only: an activation that crossed an extender would tell the
	// operator that extender's address, not this host's (C2)
	strategySettings := connect.DefaultClientStrategySettings()
	if options.dialContext != nil {
		strategySettings.ConnectSettings.DialContextSettings = &connect.DialContextSettings{
			DialContext: options.dialContext,
		}
	}
	run.clientStrategy = connect.NewDirectClientStrategy(ctx, strategySettings, 0)
	run.closers = append(run.closers, run.clientStrategy.Close)

	directorySettings := connect.DefaultExtenderDirectorySettings()
	directorySettings.NetworkHosts = run.networkHosts()
	if store := run.directoryStore(); store != nil {
		directorySettings.Store = store
	}
	run.directory = connect.NewExtenderDirectory(ctx, directorySettings)
	run.closers = append(run.closers, run.directory.Close)

	// the root keys must be in place before the first activation applies this
	// extender's own record; the network client keeps them refreshed after
	// that (B4, E3)
	if err := run.refreshRootKeys(ctx); err != nil {
		Err.Printf("extender root keys: %s", err)
	}

	run.listener = gossip.NewInProcessListener(ctx, gossip.DefaultInProcessListenerSettings())
	run.closers = append(run.closers, run.listener.Close)
	run.feedServer = gossip.NewFeedServer(
		ctx, run.directory, run.publicKey, gossip.DefaultFeedServerSettings())
	run.closers = append(run.closers, run.feedServer.Close)

	nodeSettings := gossip.DefaultNodeSettings(gossip.NodeRoleExtender)
	nodeSettings.NetworkHost = run.networkHost
	nodeSettings.Directory = run.directory
	nodeSettings.IdentityKeySeed = keySeed
	// the mesh addresses are published per activated family, which has not
	// happened yet (D2)
	nodeSettings.ExtenderListener = run.listener
	if run.node, err = gossip.NewNode(ctx, nodeSettings); err != nil {
		return nil, err
	}
	run.closers = append(run.closers, run.node.Close)

	// what this extender measures of its peers goes to the operator in
	// batches, under the same client credential the activation uses. The
	// pinger reports; what a provider measures of this extender the provider
	// reports itself (GEOMAP §2.5)
	reporterSettings := connect.DefaultExtenderPingReporterSettings()
	reporterSettings.ApiUrl = options.apiUrl
	reporterSettings.ByJwt = func() string { return options.jwt }
	reporterSettings.ClientStrategy = run.clientStrategy
	run.pingReporter = connect.NewExtenderPingReporter(ctx, reporterSettings)
	run.closers = append(run.closers, run.pingReporter.Close)

	serverSettings := extender.DefaultExtenderSettings()
	serverSettings.IdentityKeySeed = keySeed
	serverSettings.GossipConnHandler = run.listener.Handle
	serverSettings.FeedConnHandler = run.feedServer.Serve
	// a peer that pings this extender is judged against the records the
	// operator vouched for, the same directory the feed serves (GEOMAP §2.4)
	serverSettings.ProbePeerVerifier = run.directory.IsActiveKey
	serverSettings.ListenErrorHandler = func(carrier string, err error) {
		Err.Printf("extender %s carrier is not listening: %s", carrier, err)
	}
	serverSettings.Listen = options.listen
	serverSettings.ListenPacket = options.listenPacket
	serverSettings.DnsPrivilegedPort = options.dnsPrivilegedPort
	// an NLayer extender relays every forward to one of its hops instead of to
	// the destination; the services stay local (A11)
	serverSettings.NLayerHops = options.nlayerHops
	serverSettings.NLayerHoldHandler = func(index int, held bool, err error) {
		run.nlayerHoldChanged(index, held, err, serverSettings.NLayerHoldTimeout)
	}
	if 0 < len(options.nlayerHops) {
		Out.Printf("extender nlayer: relaying every forward to one of %d hops", len(options.nlayerHops))
		for index, hop := range options.nlayerHops {
			Out.Printf("extender nlayer hop %d: %s", index, extenderHopDescription(hop))
		}
	}
	// the admission limits of this instance (A12)
	serverSettings.AdmissionSubnetsPerMinute = options.admissionSubnetsPerMinute
	serverSettings.AdmissionActionsPerSubnetPerMinute = options.admissionActionsPerSubnetPerMinute
	serverSettings.AdmissionUnlimitedSources = options.admissionUnlimitedSources
	Out.Printf(
		"extender admission: %d subnets a minute, %d actions a subnet a minute, %d unlimited source prefixes",
		options.admissionSubnetsPerMinute,
		options.admissionActionsPerSubnetPerMinute,
		len(options.admissionUnlimitedSources),
	)
	// an operator activated extender is open: it accepts every header and
	// forwards only to the whitelist (A4, A5)
	run.server = extender.NewExtenderServer(
		ctx,
		nil,
		run.allowedHosts(),
		run.ports(),
		&net.Dialer{},
		serverSettings,
	)
	run.closers = append(run.closers, run.server.CloseAndWait)
	go func() {
		run.serveDone <- run.server.ListenAndServe()
	}()

	// the activation offers the carriers that bound, so it waits for the binds
	// to settle (G2)
	select {
	case <-run.server.Listening():
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(extenderStartTimeout):
		return nil, fmt.Errorf("the extender carriers did not bind")
	}
	select {
	case err := <-run.serveDone:
		if err != nil {
			return nil, err
		}
	default:
	}
	Out.Printf("extender carriers: %s", strings.Join(run.server.Carriers(), ","))

	activatorSettings := connect.DefaultExtenderActivatorSettings()
	activatorSettings.ApiUrlV4 = familyServiceUrl(options.apiUrl, 4)
	activatorSettings.ApiUrlV6 = familyServiceUrl(options.apiUrl, 6)
	// a url with no service label to suffix has no api-v4 or api-v6 host; the
	// plain url activates one family per cycle instead, which the operator
	// derives from the caller address (C2)
	activatorSettings.ApiUrl = options.apiUrl
	if activatorSettings.ApiUrlV4 == "" && activatorSettings.ApiUrlV6 == "" {
		Out.Printf(
			"extender: %s has no api-v4 or api-v6 host; activating one family per cycle",
			options.apiUrl)
	}
	activatorSettings.HelloUrl = options.apiUrl
	activatorSettings.ByJwt = func() string { return options.jwt }
	activatorSettings.ClientStrategy = run.clientStrategy
	activatorSettings.PublicKey = run.publicKey
	activatorSettings.TcpPort = options.tcpPort
	activatorSettings.UdpPort = options.udpPort
	activatorSettings.DnsPort = options.dnsPort
	// the ports that actually bound, which is what the operator probes and the
	// record lists (L2)
	activatorSettings.DnsPorts = run.server.DnsPorts
	activatorSettings.Carriers = run.server.Carriers
	activatorSettings.Directory = run.directory
	activatorSettings.OnActivated = run.activated
	run.activator = connect.NewExtenderActivator(ctx, activatorSettings)
	run.closers = append(run.closers, run.activator.Close)

	if networkClientSettings := run.networkClientSettings(); networkClientSettings != nil {
		run.networkClient = connect.NewExtenderNetworkClient(
			ctx, run.clientStrategy, run.directory, networkClientSettings)
		run.closers = append(run.closers, run.networkClient.Close)
	}

	// the peer pings are signed with the identity key, as this extender and
	// under the peer probe domain only; they start once this extender's own
	// record is active, since a peer refuses a pinger it does not know
	// (GEOMAP §2.1)
	identityPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(keySeed)
	if err != nil {
		return nil, err
	}
	pingerSettings := connect.DefaultExtenderPeerPingerSettings()
	pingerSettings.OwnPublicKey = run.publicKey
	pingerSettings.Attestor = connect.NewExtenderProbeExtenderAttestor(
		run.publicKey,
		connect.NewExtenderPeerProbeSigner(identityPrivateKey),
	)
	pingerSettings.Reporter = run.pingReporter
	run.peerPinger = connect.NewExtenderPeerPinger(ctx, run.clientStrategy, run.directory, pingerSettings)
	run.closers = append(run.closers, run.peerPinger.Close)

	success = true
	if options.onStart != nil {
		options.onStart(run)
	}
	return run, nil
}

// Serves until the context is done, logging every activation status change
// and every change to the peer ping counts. One family's line is printed only
// when it changes, so a change to the other family does not repeat it.
func (self *extenderRun) run(ctx context.Context) error {
	ipVersionLines := map[int]string{}
	pingerLine := ""
	admissionLine := ""
	admissionLogTimeout := extenderAdmissionLogTimeout
	if 0 < self.options.admissionLogTimeout {
		admissionLogTimeout = self.options.admissionLogTimeout
	}
	for {
		// subscribe before the read, so a change that lands while the lines
		// below are printed wakes the next wait rather than being lost
		_, change := self.activator.ChangeMonitor().Get()
		for _, family := range self.activator.Status().Families {
			line := extenderFamilyStatusLine(family)
			if line == "" || ipVersionLines[family.IpVersion] == line {
				continue
			}
			ipVersionLines[family.IpVersion] = line
			Out.Printf("%s", line)
		}
		// a nil channel never fires, which is a run with no pinger
		var pingerChange chan struct{}
		if self.peerPinger != nil {
			var pingerStatus connect.ExtenderPeerPingerStatus
			pingerStatus, pingerChange = self.peerPinger.StatusMonitor().Get()
			if line := extenderPeerPingerStatusLine(pingerStatus); line != "" && line != pingerLine {
				pingerLine = line
				Out.Printf("%s", line)
			}
		}
		if line := extenderAdmissionStatsLine(self.server.AdmissionStats()); line != "" && line != admissionLine {
			admissionLine = line
			Out.Printf("%s", line)
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-self.serveDone:
			// every carrier went away; there is nothing left to activate
			return err
		case <-change:
		case <-pingerChange:
		case <-time.After(admissionLogTimeout):
		}
	}
}

// The admission counts as a log line (A12), empty before the limits have
// refused or waved anything through.
func extenderAdmissionStatsLine(stats extender.ExtenderAdmissionStats) string {
	if stats == (extender.ExtenderAdmissionStats{}) {
		return ""
	}
	return fmt.Sprintf(
		"extender admission: limited %d by subnets, %d by source; %d unlimited",
		stats.LimitedBySubnetsCount,
		stats.LimitedBySourceCount,
		stats.UnlimitedCount,
	)
}

// Logs one change of an NLayer hop's hold, with what the server has counted of
// the hop so far, which is the only NLayer status this command reports (A11).
// It runs on the connection that saw the change, so it only formats and logs.
func (self *extenderRun) nlayerHoldChanged(index int, held bool, err error, holdTimeout time.Duration) {
	description := fmt.Sprintf("hop %d", index)
	if 0 <= index && index < len(self.options.nlayerHops) {
		description = fmt.Sprintf("hop %d (%s)", index, extenderHopDescription(self.options.nlayerHops[index]))
	}
	counts := ""
	if hopStats := self.server.NLayerStats(); 0 <= index && index < len(hopStats) {
		counts = fmt.Sprintf(
			" (relayed %d, refused %d, failed %d, limited %d)",
			hopStats[index].RelayCount,
			hopStats[index].RefusedCount,
			hopStats[index].FailedCount,
			hopStats[index].LimitedCount,
		)
	}
	if held {
		Err.Printf("extender nlayer %s held for %s: %s%s", description, holdTimeout, err, counts)
	} else {
		Out.Printf("extender nlayer %s released%s", description, counts)
	}
}

// The peer ping counts as a log line, empty before the pinger has a peer.
func extenderPeerPingerStatusLine(status connect.ExtenderPeerPingerStatus) string {
	if status.PeerCount == 0 && status.PingCount == 0 {
		return ""
	}
	return fmt.Sprintf(
		"extender peer pings: %d peers, %d pings (%d cosigned, %d rejected, %d unknown, %d unattested, %d failed)",
		status.PeerCount,
		status.PingCount,
		status.CosignedCount,
		status.RejectedCount,
		status.UnknownCount,
		status.UnattestedCount,
		status.FailedCount,
	)
}

// One family's activation state as a log line, empty before its first attempt.
// A family of 0 is an outcome the operator named no family for, which only the
// plain api url can produce.
func extenderFamilyStatusLine(family *connect.ExtenderFamilyActivationStatus) string {
	name := "extender"
	if 0 < family.IpVersion {
		name = fmt.Sprintf("extender v%d", family.IpVersion)
	}
	switch {
	case family.Activated:
		return fmt.Sprintf(
			"%s activated at %s until %s",
			name, family.Ip, family.ExpireTime.Format(time.RFC3339))
	case family.LastError != "":
		return fmt.Sprintf("%s is not activated: %s", name, family.LastError)
	default:
		return ""
	}
}

// Releases every part that was built, newest first.
func (self *extenderRun) close() {
	for i := len(self.closers) - 1; 0 <= i; i -= 1 {
		self.closers[i]()
	}
	self.closers = nil
}

// Publishes the mesh address of one activated family (D2, G3). Only the tcp
// carrier carries the mesh, so a host whose tcp bind failed advertises nothing.
func (self *extenderRun) activated(ipVersion int, result *connect.ExtenderActivateResult) {
	if !slices.Contains(self.server.Carriers(), connect.ExtenderCarrierTcp) {
		return
	}
	ip, err := netip.ParseAddr(result.Ip)
	if err != nil {
		Err.Printf("extender activation carried no address: %s", err)
		return
	}
	listenAddrs, err := gossip.ExtenderListenAddrs([]netip.Addr{ip}, self.options.tcpPort)
	if err != nil {
		Err.Printf("extender mesh address: %s", err)
		return
	}
	if err := self.node.Listen(listenAddrs...); err != nil {
		Err.Printf("extender mesh listen: %s", err)
	}
}

// The identity key (B1): the file's seed, a new seed written to it, or an
// ephemeral one when there is nowhere to keep it.
func (self *extenderRun) identityKeySeed() ([]byte, error) {
	keyFile := strings.TrimSpace(self.options.keyFile)
	if keyFile == "" && strings.TrimSpace(self.options.stateDir) != "" {
		keyFile = filepath.Join(self.options.stateDir, extenderKeyFileName)
	}
	if keyFile == "" {
		Err.Printf("extender identity is ephemeral; pass --extender_key_file to keep it")
		return connect.NewExtenderKeySeed()
	}

	seedHex, err := os.ReadFile(keyFile)
	if err == nil {
		return connect.ParseExtenderKeySeedHex(string(seedHex))
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyFile, []byte(connect.ExtenderKeySeedHex(seed)), 0600); err != nil {
		return nil, err
	}
	return seed, nil
}

// The directory store under --state_dir, or nil for a memory directory (E1).
func (self *extenderRun) directoryStore() connect.ExtenderDirectoryStore {
	stateDir := strings.TrimSpace(self.options.stateDir)
	if stateDir == "" {
		return nil
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		Err.Printf("extender state dir: %s", err)
		return nil
	}
	return &extenderFileStore{path: filepath.Join(stateDir, extenderDirectoryFileName)}
}

// The hosts whose records this directory accepts (B2): the space host the
// operator signs with, and the api host itself, which is the space host when
// the api url names no service label.
func (self *extenderRun) networkHosts() []string {
	networkHosts := []string{self.networkHost}
	if self.apiHost != self.networkHost {
		networkHosts = append(networkHosts, self.apiHost)
	}
	return networkHosts
}

// The operator patterns this extender forwards to (A5): the api host and one
// wildcard level under it, plus every --allowed_host. The activation also
// reports the operator's own list, which the status shows rather than applies,
// so a host this extender was not configured for is visible instead of silently
// opened.
func (self *extenderRun) allowedHosts() []string {
	allowedHosts := []string{self.apiHost, "*." + self.apiHost}
	for _, allowedHost := range self.options.allowedHosts {
		if !slices.Contains(allowedHosts, allowedHost) {
			allowedHosts = append(allowedHosts, allowedHost)
		}
	}
	return allowedHosts
}

// The carrier ports (A1). tcp and udp share port 443 by default, which is one
// entry with both connect modes.
func (self *extenderRun) ports() map[int][]connect.ExtenderConnectMode {
	ports := map[int][]connect.ExtenderConnectMode{}
	for _, carrier := range []struct {
		port        int
		connectMode connect.ExtenderConnectMode
	}{
		{port: self.options.tcpPort, connectMode: connect.ExtenderConnectModeTcpTls},
		{port: self.options.udpPort, connectMode: connect.ExtenderConnectModeQuic},
		{port: self.options.dnsPort, connectMode: connect.ExtenderConnectModeDns},
	} {
		ports[carrier.port] = append(ports[carrier.port], carrier.connectMode)
	}
	return ports
}

// The network client of this space (E3), or nil when the api url names an ip
// literal: there is no service label to derive an extender dns name from, and
// nothing to resolve.
func (self *extenderRun) networkClientSettings() *connect.ExtenderNetworkClientSettings {
	extenderDnsName := connect.ExtenderServiceHostName(self.options.apiUrl, "extender")
	if extenderDnsName == "" {
		return nil
	}
	settings := connect.DefaultExtenderNetworkClientSettings()
	settings.ExtenderDnsName = extenderDnsName
	settings.ApiUrl = self.options.apiUrl
	// this node is a member of the mesh, which carries the live records, so
	// the feed is a one-shot sample (D5)
	settings.Subscribe = false
	if self.options.configureNetworkClient != nil {
		self.options.configureNetworkClient(settings)
	}
	return settings
}

// Reads the root keys from hello and installs them as the directory's trust
// anchor (B4).
func (self *extenderRun) refreshRootKeys(ctx context.Context) error {
	helloCtx, cancel := context.WithTimeout(ctx, extenderStartTimeout)
	defer cancel()
	request, err := connect.HelloRequestFromUrl(helloCtx, self.options.apiUrl, self.options.jwt)
	if err != nil {
		return err
	}
	bodyBytes, err := connect.HttpGetWithStrategyRaw(
		helloCtx, self.clientStrategy, request.URL.String(), self.options.jwt)
	if err != nil {
		return err
	}
	helloResult := &struct {
		ExtenderRootPublicKeys []string `json:"extender_root_public_keys"`
	}{}
	if err := json.Unmarshal(bodyBytes, helloResult); err != nil {
		return err
	}
	if len(helloResult.ExtenderRootPublicKeys) == 0 {
		return fmt.Errorf("the operator published no extender root keys")
	}
	keySet, err := connect.NewExtenderRootKeySetFromHex(helloResult.ExtenderRootPublicKeys...)
	if err != nil {
		return err
	}
	self.directory.SetRootKeys(keySet)
	return nil
}

// extenderFileStore persists the directory envelope in one file (E1). A file
// that is not there yet is an empty directory, not an error.
type extenderFileStore struct {
	path string
}

func (self *extenderFileStore) Load() ([]byte, error) {
	stateBytes, err := os.ReadFile(self.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return stateBytes, err
}

func (self *extenderFileStore) Save(stateBytes []byte) error {
	return os.WriteFile(self.path, stateBytes, 0600)
}
