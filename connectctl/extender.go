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

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
	"github.com/urnetwork/connect/gossip"
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
	// command makes, and of every tcp carrier dial: the network client's, the
	// peer pinger's and the mesh's. The test maps its synthetic operator names
	// to loopback.
	dialContext connect.DialContextFunction
	// packetConnFactory, when set with dialContext, opens the udp endpoint of
	// every quic and dns carrier dial. The test refuses through it every
	// destination off the machine, such as the address an activation returns.
	packetConnFactory func(ctx context.Context) (net.PacketConn, error)
	// configureNetworkClient, when set, adjusts the network client settings
	// before it is built. The test installs an in-process resolver so the dns
	// bootstrap never leaves the machine.
	configureNetworkClient func(settings *connect.ExtenderNetworkClientSettings)
	// configureActivator, when set, adjusts the activation loop's settings before
	// it is built. A test pins when a post goes through it.
	configureActivator func(settings *connect.ExtenderActivatorSettings)
	// onStart, when set, receives the running extender once every part is up.
	// The test reads the live objects through it.
	onStart func(run *extenderRun)
	// admissionLogTimeout, when set, replaces extenderAdmissionLogTimeout as
	// how often the admission counts are read for the log. The test reads
	// them without waiting out the minute.
	admissionLogTimeout time.Duration

	// whether this extender serves the lag.dev measurement API and asks the
	// operator for its certificate (lag.dev/PLAN.md C4, M): the owner's
	// `measurement.enabled`, on unless --no_measurement. It also takes an
	// identity kept in a file (newMeasurementCert). The zero value is off,
	// which is what a test that builds the options itself gets.
	measurementEnabled bool
	// the zone of the measurement name (N1), empty for
	// connect.ExtenderMeasurementZone
	measurementZone string
	// configureLagLifecycle, when set, adjusts the measurement lifecycle's
	// settings before the server is built. The test shortens the certificate
	// poll through it.
	configureLagLifecycle func(settings *extender.ExtenderLagLifecycleSettings)

	// the tcp carrier camouflage (EXTENDER.md P, EXTENDER-UPDATE2 §3), off unless
	// the owner asks: --camouflage. It takes an identity kept in a file, since
	// its key derives from the identity and a client finds it in a signed record.
	camouflageEnabled bool
	// the initial splice posture (--camouflage_splice): off is Phase A, on is
	// Phase B once this extender's records have settled
	camouflageSplice bool
	// how often the borrowed names are verified again
	// (--camouflage_revalidate_interval); zero takes the role's default
	camouflageRevalidationInterval time.Duration
	// configureCamouflage, when set, adjusts the camouflage role's settings
	// before it is built. The test installs a clock and a fake borrowed site
	// through it.
	configureCamouflage func(settings *extender.ExtenderCamouflageRoleSettings)
	// configureServer, when set, sees the server's settings last, just before
	// the server is built. The test reads what the command passes through it.
	configureServer func(settings *extender.ExtenderSettings)

	// whether this extender serves the peer-to-peer webrtc carrier (EXTENDER.md
	// S, S8): --webrtc. Its offers arrive through a client under the jwt
	// connected to connectUrl, and a run whose tcp carrier does not bind
	// serves on it rather than exiting (extender_webrtc.go)
	webRtc bool
	// the platform url that client connects to, --connect_url; empty takes
	// the compiled default
	connectUrl string
	// webRtcSignaling, when set, replaces the client that connects to
	// connectUrl: it returns the manager the carrier's offers arrive on and
	// its close. The test answers on an in-process manager through it.
	webRtcSignaling func(ctx context.Context) (*connect.WebRtcManager, func(), error)

	// whether this extender posts what its tcp camouflage came to to the operator
	// (connect/EXTENDER-SPLICE-ROLLOUT.md): --camouflage_report, off by default.
	// This is the operator hosts' cohort control, which have no network space.
	camouflageReport bool
	// configureCamouflageReport, when set, adjusts the sender's settings. The test
	// takes the first look without waiting out its minutes.
	configureCamouflageReport func(settings *connect.ExtenderCamouflageReporterSettings)
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
	// the measurement API is on unless the owner turns it off (lag.dev/PLAN.md
	// C4, M10)
	options.measurementEnabled = true
	if noMeasurement, err := opts.Bool("--no_measurement"); err == nil && noMeasurement {
		options.measurementEnabled = false
	}
	if measurementZone, err := opts.String("--measurement_zone"); err == nil {
		options.measurementZone = strings.TrimSpace(measurementZone)
	}
	if camouflageReport, err := opts.Bool("--camouflage_report"); err == nil {
		options.camouflageReport = camouflageReport
	}
	if webRtc, err := opts.Bool("--webrtc"); err == nil {
		options.webRtc = webRtc
	}
	if connectUrl, err := opts.String("--connect_url"); err == nil {
		options.connectUrl = strings.TrimSpace(connectUrl)
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
	if err := options.readCamouflageOpts(opts); err != nil {
		return nil, err
	}
	return options, nil
}

// Reads the camouflage flags (EXTENDER.md P7). The options of Phase B and of the
// revalidation mean nothing without --camouflage, so a host that gives one alone
// is refused rather than left thinking it runs what it asked for.
func (self *extenderOptions) readCamouflageOpts(opts docopt.Opts) error {
	if camouflage, err := opts.Bool("--camouflage"); err == nil {
		self.camouflageEnabled = camouflage
	}
	if camouflageSplice, err := opts.Bool("--camouflage_splice"); err == nil {
		self.camouflageSplice = camouflageSplice
	}
	if self.camouflageSplice && !self.camouflageEnabled {
		return fmt.Errorf("--camouflage_splice needs --camouflage")
	}
	revalidationInterval, ok := opts["--camouflage_revalidate_interval"].(string)
	if !ok {
		return nil
	}
	if !self.camouflageEnabled {
		return fmt.Errorf("--camouflage_revalidate_interval needs --camouflage")
	}
	interval, err := time.ParseDuration(strings.TrimSpace(revalidationInterval))
	if err != nil {
		return fmt.Errorf("--camouflage_revalidate_interval must be a duration, such as 6h")
	}
	if interval < extender.ExtenderCamouflageMinRevalidationInterval {
		return fmt.Errorf(
			"--camouflage_revalidate_interval must be at least %s, since a fleet multiplies what it costs the borrowed sites",
			extender.ExtenderCamouflageMinRevalidationInterval,
		)
	}
	self.camouflageRevalidationInterval = interval
	return nil
}

// Reads one --nlayer-hop, an extender this one relays forwards to (A11):
//
//	[<carrier>://]<ip>[:<port>][?key=<hex>&camouflage_key=<hex>&secret_file=<path>&sni=<name>&tld=<tld>&fragment&reorder]
//
// The carrier is tcp, quic or dns, and tcp when omitted; the port defaults to
// the carrier's fixed port (A1). key pins the hop's identity leaf (B3), which
// is how a hop is known to be the extender it claims. camouflage_key is the
// camouflage key of the hop's signed record (P6), for the tcp carrier and only
// beside key, whose leaf check the camouflaged attempt needs: the dial is then
// the client's race, an authenticated hello fronting a name of this build's
// global borrow list and the legacy hello after it, which is what reaches a hop
// that splices a legacy hello away (Phase B). The secret that signs
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
	var camouflageKey []byte
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
		case "camouflage_key":
			if connectMode != connect.ExtenderConnectModeTcpTls {
				return nil, fmt.Errorf("camouflage_key applies to the tcp carrier only")
			}
			if camouflageKey, err = connect.ParseExtenderRealityPublicKeyHex(value); err != nil {
				return nil, fmt.Errorf("camouflage_key: %w", err)
			}
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
	if 0 < len(camouflageKey) {
		// checked once every parameter is read, since their order is the map's
		if len(hop.PublicKey) == 0 {
			return nil, fmt.Errorf("camouflage_key needs key, whose leaf check the camouflaged attempt relies on")
		}
		hop.RealityPublicKey = camouflageKey
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
	if 0 < len(hop.RealityPublicKey) {
		description += " camouflaged"
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
	// the lag.dev measurement key and certificate (lag.dev/PLAN.md C4), nil
	// when this extender serves no measurement API
	measurementCert *extender.ExtenderMeasurementCert
	// the tcp carrier camouflage (EXTENDER.md P), nil unless the owner asked
	camouflage *extender.ExtenderCamouflageRole
	// the peer-to-peer webrtc carrier and the manager its offers arrive on
	// (EXTENDER.md S8), nil unless the owner asked: --webrtc
	webRtcCarrier *connect.WebRtcExtenderCarrier
	webRtcManager *connect.WebRtcManager

	serveDone chan error
	// wakes the run loop when the carriers being served change
	carriersChange chan struct{}
	closers        []func()
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
		options:        options,
		apiHost:        apiHost,
		networkHost:    connect.ExtenderNetworkHostName(apiHost),
		serveDone:      make(chan error, 1),
		carriersChange: make(chan struct{}, 1),
	}
	success := false
	defer func() {
		if !success {
			run.close()
		}
	}()

	// before anything is created or bound, so a host that opted in and cannot
	// keep its key is told at once
	if options.camouflageEnabled && run.identityKeyFile() == "" {
		return nil, fmt.Errorf(
			"--camouflage needs a kept identity: pass --extender_key_file or --state_dir, since the camouflage key derives from the identity key and would change with it at every start",
		)
	}
	keySeed, err := run.identityKeySeed()
	if err != nil {
		if options.camouflageEnabled {
			return nil, fmt.Errorf("--camouflage needs a usable identity key in %s: %w", run.identityKeyFile(), err)
		}
		return nil, err
	}
	if run.publicKey, err = connect.ExtenderPublicKeyFromSeed(keySeed); err != nil {
		return nil, err
	}
	Out.Printf("extender public key: %s", hex.EncodeToString(run.publicKey))
	if run.camouflage, err = run.newCamouflageRole(keySeed); err != nil {
		return nil, err
	}
	run.measurementCert = run.newMeasurementCert()

	// direct only: an activation that crossed an extender would tell the
	// operator that extender's address, not this host's (C2)
	strategySettings := connect.DefaultClientStrategySettings()
	if options.dialContext != nil {
		strategySettings.ConnectSettings.DialContextSettings = &connect.DialContextSettings{
			DialContext:       options.dialContext,
			PacketConnFactory: options.packetConnFactory,
		}
	}
	// the operator's own api, whose authenticated Date corrects the clock an
	// NLayer hop's camouflaged hello seals with (EXTENDER-UPDATE2 §3.2)
	strategySettings.PlatformApiHosts = connect.PlatformApiHostNames(
		options.apiUrl,
		familyServiceUrl(options.apiUrl, 4),
		familyServiceUrl(options.apiUrl, 6),
	)
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
	if options.dialContext != nil {
		// the mesh dials its peers through the same seams as the strategy
		nodeSettings.ConnectSettings = connect.DefaultConnectSettings()
		nodeSettings.ConnectSettings.DialContextSettings = strategySettings.ConnectSettings.DialContextSettings
	}
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
	// a lost carrier comes back on its own; the run loop reports it and tells
	// the activation when the tcp carrier returns (G2, G3)
	serverSettings.CarriersChangeHandler = func() {
		select {
		case run.carriersChange <- struct{}{}:
		default:
		}
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
	// the lag.dev measurement API and the lifecycle behind it (lag.dev/PLAN.md
	// C4, M3, M10), or no API at all
	run.measurementSettings(serverSettings)
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
	// the tcp carrier camouflage, when the owner asked (EXTENDER.md P7)
	if run.camouflage != nil {
		run.camouflage.ConfigureServer(serverSettings)
	}
	// the peer-to-peer webrtc carrier, when the owner asked (EXTENDER.md S8):
	// the server lists it, so the activation offers it, and serves its
	// streams through the answerer installed below
	if options.webRtc {
		openSignaling := options.webRtcSignaling
		if openSignaling == nil {
			openSignaling = func(ctx context.Context) (*connect.WebRtcManager, func(), error) {
				return openExtenderWebRtcSignaling(ctx, options, run.clientStrategy)
			}
		}
		manager, closeSignaling, err := openSignaling(ctx)
		if err != nil {
			return nil, err
		}
		run.closers = append(run.closers, closeSignaling)
		run.webRtcManager = manager
		run.webRtcCarrier = connect.NewWebRtcExtenderCarrier(ctx, connect.DefaultWebRtcExtenderSettings(), nil)
		run.closers = append(run.closers, run.webRtcCarrier.Close)
		serverSettings.WebRtcCarrier = true
	}
	if options.configureServer != nil {
		options.configureServer(serverSettings)
	}
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
	if run.webRtcManager != nil {
		run.webRtcManager.SetExtenderCarrierAnswerer(run.webRtcCarrier.Answerer(run.server))
		// the carrier stops answering before the server it answers with goes
		run.closers = append(run.closers, func() {
			run.webRtcManager.SetExtenderCarrierAnswerer(nil)
		})
	}
	go func() {
		run.serveDone <- run.server.ListenAndServe()
	}()
	if run.camouflage != nil {
		// before the server goes down, whose splice dial the passes use
		run.closers = append(run.closers, run.camouflage.Close)
		run.camouflage.Start(ctx, run.server)
	}

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
	// A carrier whose first bind failed stays down (G2), and the operator accepts no
	// activation without tcp or webrtc (C2a), so this run could never post and would
	// never end. A role waits for the port and says so; this process exits for
	// whatever runs it to see (G4). With --webrtc the carrier is the required path
	// a host behind nat is accepted on, and the run goes on without the port.
	if !slices.Contains(run.server.Carriers(), connect.ExtenderCarrierTcp) {
		if !slices.Contains(run.server.Carriers(), connect.ExtenderCarrierWebRtc) {
			return nil, fmt.Errorf(
				"the extender tcp carrier did not bind: %w",
				run.server.ListenErrors()[connect.ExtenderCarrierTcp],
			)
		}
		Err.Printf(
			"extender tcp carrier did not bind, serving on the webrtc carrier: %s",
			run.server.ListenErrors()[connect.ExtenderCarrierTcp],
		)
	}

	activatorSettings := connect.DefaultExtenderActivatorSettings()
	if run.camouflage != nil {
		// the first post carries the key when the names verify in good time
		// (P5); the server opens tags under the same one (ConfigureServer)
		run.camouflage.WaitFirstVerification(ctx)
		activatorSettings.PublishedRealityPublicKey = run.camouflage.PublishedRealityPublicKey
	}
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
	if options.configureActivator != nil {
		options.configureActivator(activatorSettings)
	}
	if run.measurementCert != nil {
		// the activation carries the measurement certificate request, named by
		// the id its answers report (lag.dev/PLAN.md C4, N4)
		activatorSettings.MeasurementCsr = run.measurementCert.ActivationCsr
	} else if run.identityKeyFile() != "" {
		// a kept identity with no measurement certificate: the owner opted
		// out, or the measurement key cannot be used. Either way this
		// extender answers no measurement name, so every activation says so,
		// and a name it had is withdrawn (C4, C9) rather than renewed and
		// offered to lag.dev with nothing answering it. An ephemeral identity
		// has no name to withdraw.
		activatorSettings.MeasurementDisabled = func() bool {
			return true
		}
	}
	run.activator = connect.NewExtenderActivator(ctx, activatorSettings)
	if run.camouflage != nil {
		// a name that verifies after a post went out with no key is posted at once
		run.camouflage.SetReactivate(run.activator.Reactivate)
	}
	if run.measurementCert != nil {
		// the operator holding another key's request or certificate, or
		// withdrawing the certificate, asks for a pass at once, which carries
		// this key's request (C4, C9)
		run.measurementCert.SetReactivate(run.activator.Reactivate)
	}
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

	if options.camouflageReport {
		reporter := connect.NewExtenderCamouflageReporter(ctx, run.camouflageReporterSettings())
		reporter.SetSource(run.server)
		reporter.SetEnabled(true)
		// closes before the server it reads and the strategy it posts through
		run.closers = append(run.closers, reporter.Close)
	}

	success = true
	if options.onStart != nil {
		options.onStart(run)
	}
	return run, nil
}

// What the sender of the camouflage report posts with: this extender's own strategy
// and credential, which is the identity that activated it.
func (self *extenderRun) camouflageReporterSettings() *connect.ExtenderCamouflageReporterSettings {
	settings := connect.DefaultExtenderCamouflageReporterSettings()
	settings.ApiUrl = self.options.apiUrl
	settings.ByJwt = func() string { return self.options.jwt }
	settings.Post = func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
		return connect.HttpPostWithStrategyRaw(ctx, self.clientStrategy, requestUrl, requestBodyBytes, byJwt)
	}
	if self.options.configureCamouflageReport != nil {
		self.options.configureCamouflageReport(settings)
	}
	return settings
}

// Serves until the context is done, logging every activation status change
// and every change to the peer ping counts. One family's line is printed only
// when it changes, so a change to the other family does not repeat it.
func (self *extenderRun) run(ctx context.Context) error {
	ipVersionLines := map[int]string{}
	pingerLine := ""
	admissionLine := ""
	measurementLine := ""
	camouflageLine := ""
	admissionLogTimeout := extenderAdmissionLogTimeout
	if 0 < self.options.admissionLogTimeout {
		admissionLogTimeout = self.options.admissionLogTimeout
	}
	tcpServing := slices.Contains(self.server.Carriers(), connect.ExtenderCarrierTcp)
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
		// a nil channel never fires, which is a run with no measurement API
		var measurementChange chan struct{}
		if self.measurementCert != nil {
			_, measurementChange = self.measurementCert.ChangeMonitor().Get()
			if line := extenderMeasurementLine(self.measurementCert); line != measurementLine {
				measurementLine = line
				Out.Printf("%s", line)
			}
		}
		if self.camouflage != nil {
			line := extender.ExtenderCamouflageStatusLine(self.server.CamouflageStatus(), self.server.CamouflageStats())
			if line != camouflageLine {
				camouflageLine = line
				Out.Printf("extender camouflage: %s", line)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-self.serveDone:
			// the server ends only with its context or a failure to start, and
			// this process exists to serve it, so this is an exit with an error
			// and whatever runs connectctl restarts it (G4)
			if err == nil && ctx.Err() == nil {
				err = fmt.Errorf("the extender server ended")
			}
			return err
		case <-self.carriersChange:
			carriers := self.server.Carriers()
			Out.Printf("extender carriers: %s", strings.Join(carriers, ","))
			serving := slices.Contains(carriers, connect.ExtenderCarrierTcp)
			if serving && !tcpServing {
				self.activator.TcpCarrierReturned()
			}
			tcpServing = serving
			self.activator.CarriersChanged()
		case <-change:
		case <-pingerChange:
		case <-measurementChange:
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

// The measurement certificate as a log line (lag.dev/PLAN.md C4): the name it
// serves and until when, or that there is none to serve -- before the operator
// issues one, and after it expires or is withdrawn.
func extenderMeasurementLine(measurementCert *extender.ExtenderMeasurementCert) string {
	names, certificate := measurementCert.Certificate()
	if certificate == nil || certificate.Leaf == nil {
		return "extender measurement: no certificate"
	}
	return fmt.Sprintf(
		"extender measurement: serving %s until %s",
		strings.Join(names, ","),
		certificate.Leaf.NotAfter.Format(time.RFC3339),
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

// What one activated family changes (G3): the safe-flip gate reads the record,
// whatever the carriers (EXTENDER-UPDATE2 §3.2), and the mesh address is published
// (D2). Only the tcp carrier carries the mesh, so a host whose tcp bind failed
// advertises nothing.
func (self *extenderRun) activated(ipVersion int, result *connect.ExtenderActivateResult) {
	if self.camouflage != nil {
		self.camouflage.ObserveActivation(result, self.directory.RootKeys())
	}
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

// The file the identity key is kept in (B1): --extender_key_file, else the
// state dir's dot file, else "" for an ephemeral identity.
func (self *extenderRun) identityKeyFile() string {
	keyFile := strings.TrimSpace(self.options.keyFile)
	if keyFile == "" && strings.TrimSpace(self.options.stateDir) != "" {
		keyFile = filepath.Join(self.options.stateDir, extenderKeyFileName)
	}
	return keyFile
}

// The camouflage of this extender (EXTENDER.md P7), or nil when the owner did not
// ask for it. A seed it cannot derive its key from fails the start.
func (self *extenderRun) newCamouflageRole(keySeed []byte) (*extender.ExtenderCamouflageRole, error) {
	if !self.options.camouflageEnabled {
		return nil, nil
	}
	settings := extender.DefaultExtenderCamouflageRoleSettings()
	settings.Splice = self.options.camouflageSplice
	if 0 < self.options.camouflageRevalidationInterval {
		settings.RevalidationInterval = self.options.camouflageRevalidationInterval
	}
	if self.options.configureCamouflage != nil {
		self.options.configureCamouflage(settings)
	}
	camouflage, err := extender.NewExtenderCamouflageRole(keySeed, settings)
	if err != nil {
		return nil, err
	}
	Out.Printf(
		"extender camouflage: on, splice %t, revalidating borrowed names every %s",
		settings.Splice,
		settings.RevalidationInterval,
	)
	return camouflage, nil
}

// The measurement key and certificate (lag.dev/PLAN.md C4), or nil when this
// extender serves no measurement API: the owner turned it off, or the identity
// is ephemeral. The operator names the certificate by the extender's id, one
// per identity key, so an identity that is new at every start would ask for a
// new certificate each time; and the measurement key is kept beside the
// identity's file, which an ephemeral identity has none of. A key that cannot
// be read leaves measurement off rather than the extender down, and the
// activation then asks the operator to withdraw the name, as for an opt-out.
func (self *extenderRun) newMeasurementCert() *extender.ExtenderMeasurementCert {
	if !self.options.measurementEnabled {
		Out.Printf("extender measurement: off")
		return nil
	}
	keyFile := self.identityKeyFile()
	if keyFile == "" {
		Err.Printf("extender measurement: off, the identity is ephemeral; pass --extender_key_file or --state_dir to keep it")
		return nil
	}
	measurementKeyFile := extender.ExtenderMeasurementKeyPath(keyFile)
	measurementCert, err := extender.NewExtenderMeasurementCert(measurementKeyFile, self.options.measurementZone)
	if err != nil {
		Err.Printf(
			"extender measurement: off, the measurement key %s is not usable, so every activation asks the operator to withdraw this extender's measurement name: %s",
			measurementKeyFile,
			err,
		)
		return nil
	}
	return measurementCert
}

// The measurement settings of the server (lag.dev/PLAN.md C4, M3, M10). With a
// measurement certificate: the API's admission trusts the root keys the
// directory trusts for records, read at each ticket so a rotation applies at
// once, and the network hosts its records are for; and the lifecycle polls the
// certificate and posts reports under the activation credential. Without one,
// no API at all, so a hello for a measurement name takes the existing path.
func (self *extenderRun) measurementSettings(serverSettings *extender.ExtenderSettings) {
	if self.measurementCert == nil {
		serverSettings.Measurement.Enabled = false
		serverSettings.LagLifecycle = nil
		return
	}
	directory := self.directory
	serverSettings.Measurement.RootPublicKeys = func() []ed25519.PublicKey {
		return directory.RootKeys().PublicKeys()
	}
	serverSettings.Measurement.NetworkHosts = self.networkHosts()

	lagSettings := extender.DefaultExtenderLagLifecycleSettings()
	lagSettings.Certificate = self.measurementCert
	lagSettings.ApiUrl = self.options.apiUrl
	jwt := self.options.jwt
	lagSettings.ByJwt = func() string {
		return jwt
	}
	lagSettings.ClientStrategy = self.clientStrategy
	if self.options.configureLagLifecycle != nil {
		self.options.configureLagLifecycle(lagSettings)
	}
	serverSettings.LagLifecycle = lagSettings
}

// The identity key (B1): the file's seed, a new seed written to it, or an
// ephemeral one when there is nowhere to keep it.
func (self *extenderRun) identityKeySeed() ([]byte, error) {
	keyFile := self.identityKeyFile()
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
