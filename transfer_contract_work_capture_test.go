// Real lifecycle and HTTPS boundaries exercise original capture and custody.
package connect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Explicit test preparation uses the same borrowed-descriptor birth inspector
// as the offline owner adapter. Runtime capture itself never creates a birth.
func prepareOriginalWorkOutboxTest(t *testing.T, directory string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	index, err := os.OpenFile(filepath.Join(directory, OriginalWorkOutboxIndexName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(index.Sync(), index.Close()); err != nil {
		t.Fatal(err)
	}
	root, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	raw, err := BuildFreshOriginalWorkOutboxCheckpoint(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceOriginalWorkOutboxAttribute(root, raw, true); err != nil {
		t.Fatal(err)
	}
	if err := root.Sync(); err != nil {
		t.Fatal(err)
	}
}

// Exact profile time is independent of scheduler speed in direct capture tests.
func wholeWorkCaptureFixture(t *testing.T, client *Client) (OriginalWorkCaptureSettings, protocol.OriginalWorkRequest, ed25519.PrivateKey) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{74}, 32))
	var approver [32]byte
	copy(approver[:], key[32:])
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	prepareOriginalWorkOutboxTest(t, directory)
	settings := OriginalWorkCaptureSettings{ApiUrl: "https://work.example", OutboxDirectory: directory, RequestPublicKey: approver, now: func() time.Time { return time.Unix(1100, 0) }}
	cut, err := client.ContractManager().OriginalWorkCut(t.Context(), 7, 101, [32]byte{75})
	if err != nil {
		t.Fatal(err)
	}
	request, err := protocol.SignOriginalWorkRequest(protocol.OriginalWorkRequest{RequestId: [16]byte{76}, DomainHash: cut.DomainHash, ClientId: cut.ClientId, Generation: cut.Generation, PublicKey: cut.PublicKey, Epoch: cut.Epoch, Kind: "start", Block: cut.Block, BlockHash: cut.BlockHash, IssuedAtUnix: 1000, ExpiresAtUnix: 1300}, key)
	if err != nil {
		t.Fatal(err)
	}
	settings.PublicKey = request.PublicKey
	return settings, request, key
}

func TestWholeWorkOutboxRetainsFirstBoundaryAcrossRetryExpiryAndRestart(t *testing.T) {
	client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
	settings, request, _ := wholeWorkCaptureFixture(t, client)
	requestRaw, _ := request.Bytes()
	outbox, err := openOriginalWorkOutbox(settings.OutboxDirectory)
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.ContractManager().captureOriginalWork(t.Context(), settings, outbox, requestRaw)
	if err != nil {
		t.Fatal(err)
	}
	wholeWorkTestAdmit(t, client, NewId(), NewId())
	settings.now = func() time.Time { return time.Unix(1400, 0) }
	retry, err := client.ContractManager().captureOriginalWork(t.Context(), settings, outbox, requestRaw)
	if err != nil || !bytes.Equal(first, retry) {
		t.Fatal("expired retry recaptured newer work", err)
	}
	if err := outbox.close(); err != nil {
		t.Fatal(err)
	}
	other := newWholeWorkTestClient(t, NewNoContractClientOob(), nil, client.ClientId())
	if other.ContractManager().wholeWorkInventory.generation == client.ContractManager().wholeWorkInventory.generation {
		t.Fatal("restart reused its generation")
	}
	reopened, err := openOriginalWorkOutbox(settings.OutboxDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	restored, err := other.ContractManager().captureOriginalWork(t.Context(), settings, reopened, requestRaw)
	if err != nil || !bytes.Equal(first, restored) {
		t.Fatal("restart replaced its old original", err)
	}
	var submission protocol.OriginalWorkCutSubmission
	if json.Unmarshal(restored, &submission) != nil {
		t.Fatal("retained original could not decode")
	}
	cut, err := protocol.DecodeOriginalWorkCut(t.Context(), submission.Cut)
	if err != nil || cut.Generation != request.Generation || len(cut.Contracts) != 0 {
		t.Fatal("retained cut changed", cut, err)
	}
}

func TestWholeWorkOutboxRefusesChangedBoundaryAndPriorGenerationRecapture(t *testing.T) {
	client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
	settings, request, key := wholeWorkCaptureFixture(t, client)
	outbox, err := openOriginalWorkOutbox(settings.OutboxDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.close()
	raw, _ := request.Bytes()
	if _, err := client.ContractManager().captureOriginalWork(t.Context(), settings, outbox, raw); err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.RequestId[0]++
	changed.BlockHash[0]++
	changed, err = protocol.SignOriginalWorkRequest(changed, key)
	if err != nil {
		t.Fatal(err)
	}
	changedRaw, _ := changed.Bytes()
	if _, err := client.ContractManager().captureOriginalWork(t.Context(), settings, outbox, changedRaw); err == nil {
		t.Fatal("new request id reinterpreted retained window boundary")
	}
	other := newWholeWorkTestClient(t, NewNoContractClientOob(), nil, client.ClientId())
	request.Kind = "end"
	request.Block++
	request.BlockHash[0]++
	request, err = protocol.SignOriginalWorkRequest(request, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = request.Bytes()
	if _, err := other.ContractManager().captureOriginalWork(t.Context(), settings, outbox, raw); err == nil {
		t.Fatal("restart signed prior generation's missing end cut")
	}
}

func TestWholeWorkOutboxPrivateLeasePartialAndSymlinkRefusals(t *testing.T) {
	client := newWholeWorkTestClient(t, NewNoContractClientOob(), nil)
	settings, request, _ := wholeWorkCaptureFixture(t, client)
	outbox, err := openOriginalWorkOutbox(settings.OutboxDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := openOriginalWorkOutbox(settings.OutboxDirectory); err == nil {
		second.close()
		t.Fatal("concurrent owner acquired outbox")
	}
	name := originalWorkOutboxName(request)
	if err := os.WriteFile(filepath.Join(settings.OutboxDirectory, name), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.entries(t.Context()); err == nil {
		t.Fatal("partial original became retained complete evidence")
	}
	if err := outbox.close(); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "outbox")
	if err := os.Symlink(settings.OutboxDirectory, alias); err != nil {
		t.Fatal(err)
	}
	if other, err := openOriginalWorkOutbox(alias); err == nil {
		other.close()
		t.Fatal("symlink owns original outbox")
	}
	if err := os.Chmod(settings.OutboxDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	if other, err := openOriginalWorkOutbox(settings.OutboxDirectory); err == nil {
		other.close()
		t.Fatal("public outbox custody accepted")
	}
}

// The server discovers only the declared owner, then returns a separately
// signed request. Neither the server handler nor test chooses a completeness bit.
func TestWholeWorkActualLifecyclePollsRetainsAndDeliversSignedCut(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{77}, 32))
	var approver [32]byte
	copy(approver[:], key[32:])
	directory := t.TempDir()
	prepareOriginalWorkOutboxTest(t, directory)
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	got := make(chan protocol.OriginalWorkCutSubmission, 1)
	enrolled := make(chan protocol.OriginalWorkOwnerEnrollment, 1)
	cycleFinished := make(chan struct{}, 1)
	failures := make(chan error, 1)
	fail := func(err error) {
		select {
		case failures <- err:
		default:
		}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/provider-work/v1/owners":
			raw, err := io.ReadAll(io.LimitReader(request.Body, protocol.MaximumOriginalWorkOwnerBytes+1))
			identity, decodeErr := protocol.DecodeOriginalWorkOwnerEnrollment(request.Context(), raw)
			if err != nil || decodeErr != nil {
				fail(errors.Join(err, decodeErr))
				http.Error(writer, "invalid owner", http.StatusBadRequest)
				return
			}
			json.NewEncoder(writer).Encode(protocol.OriginalWorkOwnerReceipt{Schema: protocol.OriginalWorkOwnerReceiptSchema, OwnerHash: sha256.Sum256(raw)})
			select {
			case enrolled <- identity:
			default:
			}
		case "/provider-work/v1/requests":
			value := protocol.OriginalWorkRequest{RequestId: [16]byte{78}, Epoch: 9, Kind: "start", Block: 200, BlockHash: [32]byte{79}, IssuedAtUnix: 1000, ExpiresAtUnix: 1300}
			for _, field := range []struct {
				name string
				out  []byte
			}{{name: "domain", out: value.DomainHash[:]}, {name: "client", out: value.ClientId[:]}, {name: "generation", out: value.Generation[:]}, {name: "key", out: value.PublicKey[:]}} {
				raw, err := hex.DecodeString(request.URL.Query().Get(field.name))
				if err != nil || len(raw) != len(field.out) {
					fail(errors.New("actual owner query malformed"))
					http.Error(writer, "bad", 400)
					return
				}
				copy(field.out, raw)
			}
			value, err := protocol.SignOriginalWorkRequest(value, key)
			if err != nil {
				fail(err)
				http.Error(writer, "bad", 400)
				return
			}
			raw, _ := value.Bytes()
			json.NewEncoder(writer).Encode(protocol.OriginalWorkRequests{Schema: protocol.OriginalWorkRequestsSchema, Requests: [][]byte{raw}})
		case "/provider-work/v1/cuts":
			var submission protocol.OriginalWorkCutSubmission
			if err := json.NewDecoder(io.LimitReader(request.Body, protocol.MaximumOriginalWorkSubmissionBytes+1)).Decode(&submission); err != nil {
				fail(err)
				http.Error(writer, "bad", 400)
				return
			}
			receipt, err := protocol.VerifyOriginalWorkSubmission(request.Context(), submission, approver)
			if err != nil {
				fail(err)
				http.Error(writer, "bad", 400)
				return
			}
			json.NewEncoder(writer).Encode(receipt)
			select {
			case got <- submission:
			default:
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	capture := &OriginalWorkCaptureSettings{ApiUrl: server.URL, OutboxDirectory: directory, RequestPublicKey: approver, HttpClient: server.Client(), now: func() time.Time { return time.Unix(1100, 0) }, afterCycle: func() {
		select {
		case cycleFinished <- struct{}{}:
		default:
		}
	}}
	client := newWholeWorkTestClient(t, NewNoContractClientOob(), capture)
	var submission protocol.OriginalWorkCutSubmission
	select {
	case submission = <-got:
	case err := <-failures:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("actual SDK capture worker did not deliver")
	}
	select {
	case <-cycleFinished:
	case <-time.After(10 * time.Second):
		t.Fatal("actual capture cycle did not finish before custody assertion")
	}
	if other, err := openOriginalWorkOutbox(directory); err == nil {
		other.close()
		t.Fatal("live owner released custody between polling cycles")
	}
	identity, err := client.ContractManager().OriginalWorkIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case observed := <-enrolled:
		if observed != identity {
			t.Fatal("enrollment differs from real retained SDK identity")
		}
	default:
		t.Fatal("request polling preceded authentic generation enrollment")
	}
	join, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := client.CloseAndWait(join); err != nil {
		t.Fatal(err)
	}
	cut, err := protocol.DecodeOriginalWorkCut(t.Context(), submission.Cut)
	if err != nil || !cut.Complete || cut.ClientId != [16]byte(client.ClientId()) || len(cut.Contracts) != 0 {
		t.Fatal("actual worker did not deliver known-empty original", cut, err)
	}
	outbox, err := openOriginalWorkOutbox(directory)
	if err != nil {
		t.Fatal("worker retained lease after join", err)
	}
	defer outbox.close()
	entries, err := outbox.entries(t.Context())
	if err != nil || len(entries) != 1 {
		t.Fatal("durable original absent after delivery", entries, err)
	}
	raw, err := outbox.read(t.Context(), entries[0])
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(submission)
	if !bytes.Equal(raw, want) {
		t.Fatal("outbox differs from delivered original")
	}
}

// Cancel while the real HTTPS request is blocked; the exact owner must join.
func TestWholeWorkActualLifecycleCancellationJoinsPendingHttp(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		once.Do(func() { close(entered) })
		<-request.Context().Done()
	}))
	defer server.Close()
	directory := t.TempDir()
	prepareOriginalWorkOutboxTest(t, directory)
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	capture := &OriginalWorkCaptureSettings{ApiUrl: server.URL, OutboxDirectory: directory, RequestPublicKey: [32]byte{79}, httpClient: server.Client()}
	client := newWholeWorkTestClient(t, NewNoContractClientOob(), capture)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("actual request did not enter")
	}
	join, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := client.CloseAndWait(join); err != nil {
		t.Fatal("pending HTTPS capture escaped SDK owner", err)
	}
	outbox, err := openOriginalWorkOutbox(directory)
	if err != nil {
		t.Fatal("canceled worker retained outbox lease", err)
	}
	outbox.close()
}
