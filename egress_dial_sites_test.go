package connect

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// Every control dial in this package must reach the network through the
// egress-aware machinery (ConnectSettings.NetDialer / DialContext /
// resolveEgressUDPAddr), or its socket/name resolution falls into the tunnel
// this process provides on Windows (the connect-hang root cause). This test
// pins that statically: raw net dials, raw OS-resolver lookups, and the
// default http client/transport are forbidden in package-root non-test files,
// except at the sites audited below.
//
// If this test fails on new code, dial through ConnectSettings (NetDialer or
// DialContext) or resolveEgressUDPAddr instead — or, if the new site really
// is exempt (a connect()-only local-address probe, the egress machinery
// itself), add it to the allowlist WITH the reason.
var allowedRawDialSites = map[string]map[string]string{
	"ice_net.go": {
		"net.Dial": "connect()-only UDP local-address probe; no packet is sent",
	},
	"egress_dial.go": {
		"net.DefaultResolver": "the context-aware no-egress path inside resolveEgressUDPAddr itself, " +
			"resolving to a list so pickControlIPAddr can honor the family policy; and dialResolver, " +
			"the same fallback for the hostname dial race in ConnectSettings.DialContext",
	},
	"ice_resolve_net.go": {
		"net.DefaultResolver": "the lifecycle-aware fallback inside the per-peer egress resolver",
	},
}

var forbiddenNetCalls = map[string]bool{
	"Dial":            true,
	"DialTimeout":     true,
	"DialUDP":         true,
	"DialTCP":         true,
	"DialIP":          true,
	"ResolveUDPAddr":  true,
	"ResolveTCPAddr":  true,
	"ResolveIPAddr":   true,
	"LookupIP":        true,
	"LookupHost":      true,
	"LookupIPAddr":    true,
	"DefaultResolver": true,
}

var forbiddenHttpIdents = map[string]bool{
	"DefaultClient":    true,
	"DefaultTransport": true,
}

func TestControlDialSitesUseTheBoundDialer(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var violations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		allowed := allowedRawDialSites[name]
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			var call string
			switch {
			case pkg.Name == "net" && forbiddenNetCalls[sel.Sel.Name]:
				call = "net." + sel.Sel.Name
			case pkg.Name == "http" && forbiddenHttpIdents[sel.Sel.Name]:
				call = "http." + sel.Sel.Name
			default:
				return true
			}
			if _, ok := allowed[call]; ok {
				return true
			}
			pos := fset.Position(sel.Pos())
			violations = append(violations, fmt.Sprintf("%s:%d: %s bypasses the egress-aware dial path", pos.Filename, pos.Line, call))
			return true
		})
	}
	if 0 < len(violations) {
		t.Fatalf("raw dial/resolve sites outside the audited allowlist:\n%s", strings.Join(violations, "\n"))
	}
}

// Every socket this package makes for a dial or a datagram endpoint must reach
// the egress machinery (egressDialer, egressControl, applyEgress, egressNet),
// so a forced interface index on Windows and macOS and the egress mark on
// Linux (QD-800, ur tun under exits) land on it, or its entry here says why
// not. This pins the sites statically, as the allowlist above pins raw dials:
// a socket constructor in a package-root non-test file that its file's entry
// does not name fails. TestEgressMarkLandsAtEverySocketSite
// (egress_dial_sites_linux_test.go) proves the mark on Linux at each site an
// entry says reaches the machinery.
//
// If this test fails on new code, make the socket through ConnectSettings
// (NetDialer or DialContext) or call applyEgress on it before its first send,
// or, if it really is exempt, add it here WITH the reason.
var allowedSocketSites = map[string]map[string]string{
	"net.go": {
		"net.Dialer": "ConnectSettings.NetDialer, through egressDialer: every ConnectSettings dial, " +
			"the platform's TCP transports, api dials and provider egress's TCP and UDP among them",
	},
	"transport_family.go": {
		"net.ListenUDP": "openH3PacketConn's QUIC socket, through applyEgress before its first send",
	},
	"net_extender.go": {
		"net.ListenUDP": "openExtenderPacketConn: the alt api dial applies egress to it (net_http_alt.go); " +
			"the extender carrier's packet dial does not, so that socket is outside the machinery on every platform",
	},
	"egress_resolver_linux.go": {
		"net.Dialer": "egressResolverDial: the marked path through egressDialer; the unmarked one, " +
			"for a process no mark steers, keeps the system's own resolver",
		"unix.Socket": "egressReadFreshSocketMark's probe, made and closed, never connected or sent on",
	},
	"egress_resolver_windows.go": {
		"net.Dialer": "egressResolverDial: the bound path through egressDialer; the unbound one while no index is set",
	},
	"http_upgrade.go": {
		"net.Dialer": "the fallback for a caller whose websocket dialer names no dial; connect's own name ConnectSettings'",
	},
	"net_extender_seed_bootstrap.go": {
		"net.Dialer": "the default for a pass whose settings name no DialContext",
	},
	"ip_icmp_egress_unix.go": {
		"icmp.ListenPacket": "provider ICMP egress off Windows, outside the machinery; the Windows file pins its own",
	},
	"net_resilient_syscall_windows.go": {
		"windows.WSASocket": "a duplicate handle of a connected socket, which carries socket options and no I/O",
	},
}

// Calls that make a socket.
var socketConstructorCalls = map[string]bool{
	"net.ListenUDP":          true,
	"net.ListenPacket":       true,
	"net.ListenIP":           true,
	"net.ListenMulticastUDP": true,
	"icmp.ListenPacket":      true,
	"unix.Socket":            true,
	"syscall.Socket":         true,
	"windows.Socket":         true,
	"windows.WSASocket":      true,
}

// Types whose value makes sockets: a literal of one is a site.
var socketConstructorTypes = map[string]bool{
	"net.Dialer":       true,
	"net.ListenConfig": true,
}

func TestSocketSitesReachTheEgressMachinery(t *testing.T) {
	if _, err := os.Stat("egress.go"); err != nil {
		t.Skip("the package's source is not in the working directory, and this reads it: run it with go test")
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var violations []string
	found := map[string]map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		site := func(expr ast.Expr, kinds map[string]bool) string {
			sel, ok := expr.(*ast.SelectorExpr)
			if !ok {
				return ""
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return ""
			}
			if kind := pkg.Name + "." + sel.Sel.Name; kinds[kind] {
				return kind
			}
			return ""
		}
		ast.Inspect(file, func(n ast.Node) bool {
			var kind string
			switch node := n.(type) {
			case *ast.CallExpr:
				kind = site(node.Fun, socketConstructorCalls)
				if ident, ok := node.Fun.(*ast.Ident); ok && ident.Name == "new" && len(node.Args) == 1 {
					kind = site(node.Args[0], socketConstructorTypes)
				}
			case *ast.CompositeLit:
				kind = site(node.Type, socketConstructorTypes)
			}
			if kind == "" {
				return true
			}
			if _, ok := allowedSocketSites[name][kind]; ok {
				if found[name] == nil {
					found[name] = map[string]bool{}
				}
				found[name][kind] = true
				return true
			}
			pos := fset.Position(n.Pos())
			violations = append(violations, fmt.Sprintf("%s:%d: %s makes a socket the egress machinery never sees", pos.Filename, pos.Line, kind))
			return true
		})
	}
	// an entry for a site that went away names nothing; remove it with the site
	for name, kinds := range allowedSocketSites {
		for kind := range kinds {
			if !found[name][kind] {
				violations = append(violations, fmt.Sprintf("%s: the entry for %s names no site", name, kind))
			}
		}
	}
	slices.Sort(violations)
	if 0 < len(violations) {
		t.Fatalf("socket sites outside the audited allowlist:\n%s", strings.Join(violations, "\n"))
	}
}

// A RawConn that records whether the egress machinery reached into it.
type egressRecordingRawConn struct {
	controlCount int
}

var _ syscall.RawConn = (*egressRecordingRawConn)(nil)

func (self *egressRecordingRawConn) Control(f func(fd uintptr)) error {
	self.controlCount += 1
	return nil
}

func (self *egressRecordingRawConn) Read(f func(fd uintptr) bool) error {
	return errors.New("not a socket")
}

func (self *egressRecordingRawConn) Write(f func(fd uintptr) bool) error {
	return errors.New("not a socket")
}

// A conn whose socket the egress machinery would reach through SyscallConn.
type egressRecordingConn struct {
	raw              egressRecordingRawConn
	syscallConnCount int
}

func (self *egressRecordingConn) SyscallConn() (syscall.RawConn, error) {
	self.syscallConnCount += 1
	return &self.raw, nil
}

// C5's extension, the egress mark (QD-800), is off by default: with no index
// and no mark set, the egress control never reaches a socket and applyEgress
// never asks a conn for one, so every socket is made as it always was, on
// every platform.
func TestEgressMarkIsOffByDefault(t *testing.T) {
	AssertEqual(t, EgressMark(), uint32(0))
	index4, index6 := EgressInterfaceIndex()
	AssertEqual(t, index4, uint32(0))
	AssertEqual(t, index6, uint32(0))
	AssertEqual(t, egressConfigured(), false)

	raw := &egressRecordingRawConn{}
	AssertEqual(t, egressControl("udp4", "192.0.2.1:443", raw), nil)
	AssertEqual(t, raw.controlCount, 0)
	conn := &egressRecordingConn{}
	AssertEqual(t, applyEgress(conn), nil)
	AssertEqual(t, conn.syscallConnCount, 0)

	// the dialer the machinery builds still dials, its control a pass-through
	dialer := egressDialer(&net.Dialer{})
	AssertEqual(t, dialer.Control("udp4", "192.0.2.1:443", raw), nil)
	AssertEqual(t, raw.controlCount, 0)

	// a mark set and cleared leaves it off again
	SetEgressMark(0x7572)
	AssertEqual(t, egressConfigured(), true)
	SetEgressMark(0)
	AssertEqual(t, egressConfigured(), false)
	AssertEqual(t, egressControl("udp4", "192.0.2.1:443", raw), nil)
	AssertEqual(t, raw.controlCount, 0)
}
