//go:build linux && !android

package connect

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// The Linux half of the egress resolver. The substitution and the socket mark
// need a tunnel-providing daemon to exercise, but the piece that decides WHICH
// server gets substituted is a pure function over a resolv.conf(5) file, and
// getting it wrong is how a "fix" ends up substituting the very stub it exists
// to escape.

func writeResolvConf(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEgressResolvConfServers(t *testing.T) {
	// the systemd-resolved stub: this is what /etc/resolv.conf holds on the
	// machine the deadlock was measured on, and substituting it would send the
	// query straight back into the tunnel it is meant to escape
	stub := writeResolvConf(t, `# managed by systemd-resolved
nameserver 127.0.0.53
options edns0 trust-ad
search lan
`)
	if servers := egressResolvConfServers(stub, 2, 2); len(servers) != 0 {
		t.Fatalf("the resolved stub must never be substituted, got %v", servers)
	}

	// the uplink file: the physical link's own resolvers, plus the tun's
	// UpgradeMux mask address once the tunnel has pushed its `~.` link
	// resolver in front of them. The mask answers nothing off-tunnel.
	uplink := writeResolvConf(t, `# managed by systemd-resolved
nameserver `+DefaultDnsUpgradeMaskAddress+`
nameserver 192.168.12.1
nameserver 192.168.12.1
nameserver fe80::1a60:41ff:fe3b:cfd9%2
search lan
`)
	servers := egressResolvConfServers(uplink, 2, 2)
	want := []string{"192.168.12.1", "fe80::1a60:41ff:fe3b:cfd9%2"}
	if len(servers) != len(want) {
		t.Fatalf("got %v want %v", servers, want)
	}
	for i, server := range servers {
		if server != want[i] {
			t.Fatalf("got %v want %v", servers, want)
		}
	}

	// a family the host cannot carry is dropped rather than dialed: reaching a
	// usable server behind it costs a full dial timeout per candidate
	if servers := egressResolvConfServers(uplink, 2, 0); len(servers) != 1 || servers[0] != "192.168.12.1" {
		t.Fatalf("v6 must be dropped with no v6 carried, got %v", servers)
	}

	// a file that is not there is no servers, not an error
	if servers := egressResolvConfServers(filepath.Join(t.TempDir(), "absent"), 2, 2); servers != nil {
		t.Fatalf("a missing file must yield no servers, got %v", servers)
	}
}

func TestEgressResolverInertWithoutTheEgressMark(t *testing.T) {
	// the test process is not the tunnel daemon, so nothing has marked its
	// sockets and control dials must keep the platform resolver exactly as
	// they did before this file existed
	if egressSelfMarked() {
		t.Skip("this process's sockets carry the egress mark; not a plain host")
	}
	SetEgressInterfaceIndex(0, 0)
	// The resolver IS handed out unconditionally now — gating at handout froze
	// nil into the primary control dialer, which is built once
	// (newNormalDialTlsContext) and would then never pick the fix up. What must stay
	// inert off the daemon is the BEHAVIOUR: egressResolverDial takes its
	// unmarked branch and dials the server the Go resolver chose, unbound,
	// exactly as the platform resolver would.
	if egressResolver() == nil {
		t.Fatal("the egress resolver must be handed out so the per-dial gate can decide")
	}
	if egressAwareResolver(nil) != egressBoundResolver {
		t.Fatal("control dials must receive the egress-bound resolver")
	}
	// a caller-supplied resolver still wins
	custom := &net.Resolver{}
	if egressAwareResolver(custom) != custom {
		t.Fatal("a caller's own resolver must take precedence")
	}
	// the unmarked dial path must not substitute a server: it resolves through
	// whatever the host configured, which is the pre-fix behaviour
	if servers := egressDnsServers(0, 0); len(servers) == 0 {
		t.Fatal("egressDnsServers must always yield a fallback list")
	}
	if egressBound() {
		t.Fatal("egressBound must stay false with no forced interface and no egress mark")
	}
}
