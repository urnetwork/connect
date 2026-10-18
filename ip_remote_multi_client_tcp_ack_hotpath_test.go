// Nonadvancing inner acknowledgements must not add a second linear claim scan.
// Structural work-order proof complements actual constructor/public delivery
// and the existing native recovery, generation, window, and ownership roots.
package connect

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// A runtime overlay does not redirect source reads. The private qualification
// binds this path to the exact compiled helper; ordinary runs read live source.
func tcpRecoveryAckObserverSource(t *testing.T) *ast.FuncDecl {
	t.Helper()
	path := os.Getenv("CONNECT_TEST_TCP_RECOVERY_SOURCE")
	expectedHash := os.Getenv("CONNECT_TEST_TCP_RECOVERY_SOURCE_SHA256")
	if (path == "") != (expectedHash == "") {
		t.Fatal("private recovery source binding requires both path and sha256")
	}
	if path == "" {
		path = "ip_remote_multi_client_tcp_recovery.go"
	}
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	actualHash := fmt.Sprintf("%x", sha256.Sum256(source))
	if expectedHash != "" && actualHash != expectedHash {
		t.Fatalf("recovery source binding mismatch: got %s want %s", actualHash, expectedHash)
	}
	t.Logf("recovery source: %s sha256=%s", path, actualHash)
	file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "observeTcpRecoveryAckWithLock" {
			return function
		}
	}
	t.Fatal("recovery observer not found")
	return nil
}

// Checks the dominating, side-effect-free return rather than a text mention.
// The scan has no observable result on nonreset claims, so a timing threshold
// cannot give the deterministic work bound this source-level root requires.
func tcpRecoveryAckGuardScanOrder(function *ast.FuncDecl) (int, int, error) {
	canonical := func(expression ast.Expr) string {
		var output bytes.Buffer
		if err := format.Node(&output, token.NewFileSet(), expression); err != nil {
			return ""
		}
		return output.String()
	}
	expected, err := parser.ParseExpr("!control.syn && recovery.client == client && recovery.seen && int32(control.ackSequenceNumber-recovery.ack) <= 0")
	if err != nil {
		return -1, -1, err
	}
	guard, scan, guardCount, scanCount := -1, -1, 0, 0
	for index, statement := range function.Body.List {
		if branch, ok := statement.(*ast.IfStmt); ok && branch.Init == nil && branch.Else == nil &&
			canonical(branch.Cond) == canonical(expected) && len(branch.Body.List) == 1 {
			if returned, ok := branch.Body.List[0].(*ast.ReturnStmt); ok && len(returned.Results) == 0 {
				guard, guardCount = index, guardCount+1
			}
		}
		ast.Inspect(statement, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if ok && selector.Sel.Name == "pendingCollapseResetWithLock" {
				scan, scanCount = index, scanCount+1
			}
			return true
		})
	}
	if guardCount != 1 || scanCount != 1 || guard >= scan {
		return guard, scan, fmt.Errorf("ordinary nonadvancing return must precede the claim scan: guard=%d count=%d scan=%d count=%d", guard, guardCount, scan, scanCount)
	}
	return guard, scan, nil
}

// V3 deterministically fails this ordering assertion. Moving the same guard
// behind the scan is a negative control, independent of machine speed or load.
func TestTcpRecoveryNonadvancingAckSkipsClaimScan(t *testing.T) {
	function := tcpRecoveryAckObserverSource(t)
	guard, scan, err := tcpRecoveryAckGuardScanOrder(function)
	if err != nil {
		t.Fatal(err)
	}
	late := make([]ast.Stmt, 0, len(function.Body.List))
	for index, statement := range function.Body.List {
		if index != guard {
			late = append(late, statement)
		}
		if index == scan {
			late = append(late, function.Body.List[guard])
		}
	}
	function.Body.List = late
	if _, _, err := tcpRecoveryAckGuardScanOrder(function); err == nil {
		t.Fatal("late-filter mutation retained the avoidable claim scan but passed")
	}
}

// Actual parsed singleton/batch returns still reach the public callback while
// 64 real current-provider raw claims remain queued. A duplicate or regressive
// pure/payload ACK must preserve every scalar of the available recovery lease.
func TestTcpRecoveryNonadvancingAckPublicDeliveryPreservesAuthority(t *testing.T) {
	for _, payloadBytes := range []int{0, tcpAckBenchPayloadBytes} {
		for _, batchSize := range []int{1, 8} {
			f := newTcpAckBenchFixture(t, payloadBytes, batchSize, 64)
			if _, accepted := f.send(true); !accepted {
				t.Fatal("public upload did not materialize")
			}
			frontier := f.sourceNext - 1
			f.receive(frontier)
			f.queueNativeClaims(64)
			f.update.stateLock.Lock()
			before := f.update.sequenceRecovery
			f.update.stateLock.Unlock()
			if !before.seen || before.ack != frontier || before.used || before.reserved {
				t.Fatal("public advancing ACK did not establish available authority")
			}
			observe := func(packet []byte) {
				if _, err := ParseIpPath(packet); err != nil {
					t.Error(err)
				}
				f.update.stateLock.Lock()
				after := f.update.sequenceRecovery
				f.update.stateLock.Unlock()
				if after != before {
					t.Errorf("payload=%d batch=%d: nonadvancing ACK changed recovery before public delivery", payloadBytes, batchSize)
				}
				f.delivered.Add(1)
			}
			f.parent.SetReceivePacketCallback(tcpAckBenchPacketCallback(ReceivePacketFunction(nil), observe))
			if batchSize != 1 {
				f.parent.SetReceivePacketsCallback(tcpAckBenchPacketsCallback(ReceivePacketsFunction(nil), observe))
			}
			for _, ack := range []uint32{frontier, frontier - 1, frontier} {
				f.receive(ack)
			}
			f.requireState(1 + 4*uint64(batchSize))
			f.close()
		}
	}
}

// An actual public new-SYN admission remains a generation barrier before its
// native source runs. Ordinary duplicates preserve state; an advancing old ACK
// must still be rejected before delivery while that valid claim is pending.
func TestTcpRecoveryAdvancingAckRetainsPublicPendingSynBarrier(t *testing.T) {
	f := newTcpAckBenchFixture(t, 0, 1, 1)
	if _, accepted := f.send(true); !accepted {
		t.Fatal("public upload did not materialize")
	}
	frontier := f.sourceNext - 2
	f.receive(frontier)
	f.update.stateLock.Lock()
	before := f.update.sequenceRecovery
	f.update.stateLock.Unlock()
	if !before.seen || before.ack != frontier || before.used || before.reserved {
		t.Fatal("public advancing ACK did not establish available authority")
	}
	f.channel.sendTransferForTest = nil
	newSequence := f.sourceNext + 100
	packet := MessagePoolCopy(tcpAckBenchPacket(f.egressPath, newSequence, 0, tcpFlagSyn, 0))
	if !f.parent.SendPacket(f.source, tcpAckBenchNetworkMode, packet, 0) {
		MessagePoolReturn(packet)
		t.Fatal("native new-SYN admission refused")
	}
	select {
	case <-f.nativeEntered:
	case <-f.ctx.Done():
		t.Fatal("native source did not reach its pre-Run barrier")
	}
	func() {
		f.update.stateLock.Lock()
		defer f.update.stateLock.Unlock()
		group := f.update.sequenceClaims
		if group == nil || group.collapseNext != nil || group.completionClient != f.channel ||
			group.collapseOrder() != 0 || group.completionFlags.Load()&groupCompletionTracked == 0 ||
			len(group.packets) != 1 || !group.packets[0].ipPath.Syn || group.packets[0].ipPath.SequenceNumber != newSequence ||
			!f.update.pendingCollapseResetWithLock() || f.update.sequenceRecovery != before {
			t.Fatal("public new-SYN did not retain the expected old-cohort reset barrier")
		}
	}()
	observe := func(packet []byte) {
		f.update.stateLock.Lock()
		after := f.update.sequenceRecovery
		f.update.stateLock.Unlock()
		if after != before {
			t.Error("pending new-SYN allowed old ACK authority before public delivery")
		}
		f.delivered.Add(1)
	}
	f.parent.SetReceivePacketCallback(tcpAckBenchPacketCallback(ReceivePacketFunction(nil), observe))
	f.receive(frontier)
	f.receive(frontier + 1)
	if f.delivered.Load() != 4 || len(f.nativeRoute) != 0 {
		t.Fatal("public delivery count or pre-Run route ownership changed")
	}
	f.close()
}

// Wraparound progress still takes full validation. An out-of-window or
// above-high-water ACK cannot open authority; removing only those barriers
// must let the same cumulative frontier advance.
func TestTcpRecoveryAdvancingAckKeepsValidationAfterFastReject(t *testing.T) {
	_, update, client := tcpRecoveryUnitFlow(t, ^uint32(0)-2)
	tcpRecoveryUnitAck(update, client, 0)
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	before := update.sequenceRecovery
	if !before.seen || before.ack != 0 || before.used || before.reserved {
		t.Fatal("wrapped frontier fixture did not establish available authority")
	}
	control := tcpControlObservation{valid: true, ack: true, sequenceNumber: 900, ackSequenceNumber: 1}
	control.sequenceNumber = before.peerSequence + before.peerWindow
	update.observeTcpRecoveryAckWithLock(client, control)
	if update.sequenceRecovery != before {
		t.Fatal("advancing ACK bypassed the advertised receive window")
	}
	control.sequenceNumber = 900
	control.ackSequenceNumber = update.sequenceNumber + 1
	update.observeTcpRecoveryAckWithLock(client, control)
	if update.sequenceRecovery != before {
		t.Fatal("advancing ACK bypassed materialized upload bounds")
	}
	control.ackSequenceNumber = 1
	update.observeTcpRecoveryAckWithLock(client, control)
	if update.sequenceRecovery.ack != 1 || update.sequenceRecovery.serial <= before.serial ||
		update.sequenceRecovery.used || update.sequenceRecovery.reserved {
		t.Fatal("valid wrapped advancement did not open fresh authority")
	}
	before = update.sequenceRecovery
	for _, ack := range []uint32{1, 0, ^uint32(0)} {
		control.ackSequenceNumber = ack
		update.observeTcpRecoveryAckWithLock(client, control)
		if update.sequenceRecovery != before {
			t.Fatal("wrapped duplicate/regression changed scalar authority")
		}
	}
}
