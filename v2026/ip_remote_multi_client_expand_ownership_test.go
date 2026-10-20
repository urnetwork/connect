package connect

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// expandArgsCleanupViolations pins each direct retirement to its guarded,
// pre-ownership site. A call-count check alone permits moving a valid cleanup
// into an evaluated channel's decline path, revoking its still-needed auth.
func expandArgsCleanupViolations(t *testing.T, body string) []string {
	t.Helper()
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "expand.go", "package connect\n"+body+"\n}\n", 0)
	if err != nil {
		return []string{fmt.Sprintf("parse expand ownership: %v", err)}
	}
	function := file.Decls[0].(*ast.FuncDecl)
	var conditions []*ast.IfStmt
	var removals []*ast.CallExpr
	var construction token.Pos
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.IfStmt:
			conditions = append(conditions, node)
		case *ast.CallExpr:
			name := formatAstExpr(t, fileSet, node.Fun)
			if name == "newMultiClientChannel" {
				construction = node.Pos()
			}
			if strings.HasSuffix(name, ".RemoveClientArgs") {
				removals = append(removals, node)
			}
		}
		return true
	})
	if construction == token.NoPos {
		return []string{"expand has no channel ownership boundary"}
	}
	allowed := map[token.Pos]bool{}
	violations := []string{}
	for _, site := range []struct {
		name               string
		guard              string
		parentGuard        string
		argument           string
		beforeConstruction bool
	}{
		{
			name:               "late delivered args",
			guard:              "!args.deferredClientArgs",
			parentGuard:        "!expandCandidateWithinAcquisitionDeadline(candidateTime, requestEndTime)",
			argument:           "&args.MultiClientGeneratorClientArgs",
			beforeConstruction: true,
		},
		{
			name:               "canceled or expired fixed mint",
			guard:              "self.ctx.Err() != nil || !expandCandidateWithinAcquisitionDeadline(time.Now(), requestEndTime)",
			parentGuard:        "args.deferredClientArgs",
			argument:           "clientArgs",
			beforeConstruction: true,
		},
		{
			name:               "unowned constructor failure",
			guard:              "!setupFailed || !setupErr.argsOwned",
			parentGuard:        "err != nil",
			argument:           "&args.MultiClientGeneratorClientArgs",
			beforeConstruction: false,
		},
	} {
		matches := 0
		for _, guard := range conditions {
			if formatAstExpr(t, fileSet, guard.Cond) != site.guard || (guard.Pos() < construction) != site.beforeConstruction {
				continue
			}
			parentFound := false
			for _, parent := range conditions {
				if parent.Body.Pos() < guard.Pos() && guard.End() < parent.Body.End() &&
					formatAstExpr(t, fileSet, parent.Cond) == site.parentGuard &&
					(parent.Pos() < construction) == site.beforeConstruction {
					parentFound = true
				}
			}
			if !parentFound {
				continue
			}
			for _, statement := range guard.Body.List {
				expression, ok := statement.(*ast.ExprStmt)
				if !ok {
					continue
				}
				call, ok := expression.X.(*ast.CallExpr)
				if ok && formatAstExpr(t, fileSet, call.Fun) == "self.generator.RemoveClientArgs" &&
					len(call.Args) == 1 && formatAstExpr(t, fileSet, call.Args[0]) == site.argument {
					allowed[call.Pos()] = true
					matches++
				}
			}
		}
		if matches != 1 {
			violations = append(violations, fmt.Sprintf("expand has %d guarded %s cleanups, want 1", matches, site.name))
		}
	}
	for _, call := range removals {
		if !allowed[call.Pos()] {
			violations = append(violations, fmt.Sprintf("expand directly retires args outside a pre-ownership cleanup at %s", fileSet.Position(call.Pos())))
		}
	}
	return violations
}

// Moving a cleanup into decline keeps the old total count but violates the
// channel's ownership. The structural inventory must reject that regression.
func TestExpandArgsCleanupAnchorRejectsOwnedRetirement(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := functionBody(source, "func (self *multiClientWindow) expand(")
	if !ok {
		t.Fatal("could not find expand")
	}
	if violations := expandArgsCleanupViolations(t, body); len(violations) != 0 {
		t.Fatalf("healthy ownership control: %v", violations)
	}
	validCleanup := "\n\t\t\t\t\tself.generator.RemoveClientArgs(clientArgs)"
	decline := "\n\t\t\tclient.Cancel()\n\t\t\tself.monitor.AddProviderEventWithExtenderIps("
	if strings.Count(body, validCleanup) != 1 || strings.Count(body, decline) != 1 {
		t.Fatal("ownership mutation sites changed")
	}
	mutated := strings.Replace(body, validCleanup, "", 1)
	mutated = strings.Replace(mutated, decline, "\n\t\t\tself.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)"+decline, 1)
	if strings.Count(mutated, "RemoveClientArgs(") != strings.Count(body, "RemoveClientArgs(") {
		t.Fatal("counterexample changed the direct cleanup count")
	}
	violations := expandArgsCleanupViolations(t, mutated)
	if !strings.Contains(strings.Join(violations, "\n"), "outside a pre-ownership cleanup") {
		t.Fatalf("same-count owned retirement was not rejected: %v", violations)
	}
}

// The real evaluated replacement retains the old flow-carrying channel and
// retires only the declined channel, through its channel-owned cleanup.
func TestExpandReplacementDeclinePreservesChannelOwnedArgs(t *testing.T) {
	fixture := newMultiClientExpandLifecycleFixture(t)
	args := <-fixture.window.clientChannelArgs
	fixture.window.clientChannelArgs <- args
	oldCtx, cancelOld := context.WithCancel(fixture.window.ctx)
	defer cancelOld()
	oldClient := stallTestChannel()
	oldClient.ctx = oldCtx
	oldClient.cancel = cancelOld
	oldClient.args = &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: args.ClientId},
		Destination:                    args.Destination,
	}
	fixture.window.clients[args.ClientId] = oldClient
	fixture.window.flowCountFunc = func(client *multiClientChannel) int {
		if client == oldClient {
			return 1
		}
		return 0
	}
	expandDone := fixture.start()
	fixture.wait(t, "held replacement ping", fixture.pingResultEntered)
	fixture.releasePing()
	if added := fixture.result(t, expandDone); added != 0 {
		t.Fatalf("flow-carrying replacement admitted %d candidates", added)
	}
	fixture.wait(t, "declined channel cleanup", fixture.clientRemoved)
	fixture.assertNoDirectArgsRemoval(t)
	if oldClient.IsDone() || fixture.window.clients[args.ClientId] != oldClient || fixture.clientCount() != 1 {
		t.Fatal("replacement decline changed the live flow-carrying channel")
	}
}
