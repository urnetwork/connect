// Accepted-proof timing is separate from a local offer's wait. These controls
// drive existing Transfer/source event seams with the existing packet clock;
// they do not claim a physical carrier, serializer, or live detector verdict.
package connect

import (
	"fmt"
	"testing"
	"time"
)

// A new admission receives its full healthy-connect budget. An earlier
// completion, queue-admission or dequeue receipt keeps its age when public
// return is delayed, while another admitted attempt keeps its own older age.
func TestMultiClientSynConnectClockStartsAtAcceptedProof(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		for _, requestedAck := range []bool{false, true} {
			for _, earlier := range []bool{false, true} {
				for _, boundary := range []string{"public-return", "terminal-before-return", "group-admitted-first", "group-dequeued-first"} {
					if !grouped && (boundary == "group-admitted-first" || boundary == "group-dequeued-first") {
						continue
					}
					label := fmt.Sprintf("%s/group=%t/requestedAck=%t/earlier=%t", boundary, grouped, requestedAck, earlier)
					channel := newConnectAdmissionTestChannel(t)
					clock := time.Now()
					original := clock
					bar := channel.settings.BlackholeConnectTimeout
					if bar <= 0 {
						t.Fatal("default connect detector must remain enabled")
					}
					channel.packetTransferNowForTest = func() time.Time { return clock }
					if earlier {
						channel.sendTransferForTest = func(completed AckFunction) (bool, error) {
							completed(nil)
							return true, nil
						}
						older := newConnectAdmissionTestOffer(t, grouped)
						if accepted, err := older.send(channel, requestedAck); !accepted || err != nil {
							t.Fatalf("%s: older admission=%t %v", label, accepted, err)
						}
						requireConnectAdmissionEvidence(t, label+" earlier", channel, true, original, nil)
					}

					offer := newConnectAdmissionTestOffer(t, grouped)
					var callback AckFunction
					firstProof := original.Add(2 * bar)
					wanted := firstProof
					if earlier {
						wanted = original
					}
					channel.sendTransferForTest = func(completed AckFunction) (bool, error) {
						callback = completed
						// More than one full health budget passes before this
						// offer has any accepted-proof event. No sleep is needed.
						clock = firstProof
						requireConnectAdmissionEvidence(t, label+" still local", channel, earlier, original, nil)
						switch boundary {
						case "terminal-before-return":
							completed(nil)
						case "group-admitted-first":
							offer.group.sendGroupAdmitted(nil)
						case "group-dequeued-first":
							offer.group.sendGroupDequeued(nil)
						}
						if boundary != "public-return" {
							stats := requireConnectAdmissionEvidence(t, label+" first proof", channel, true, wanted, nil)
							reason, held := blackholeReasonFromStats(clock, stats, 0, 0, bar, blackholeGates{
								transportDown: false, uplinkStale: false, unreliableSendTransport: false,
							})
							wantReason := blackholeNone
							if earlier {
								wantReason = blackholeNoReceiveSyn
							}
							if reason != wantReason || held != blackholeNone {
								t.Errorf("%s: at first proof verdict=%s/%s, want %s", label, reason, held, wantReason)
							}
							// A delayed public return or the other group source
							// notification cannot restart an already admitted age.
							clock = clock.Add(2 * bar)
							if boundary == "group-admitted-first" {
								offer.group.sendGroupDequeued(nil)
							} else if boundary == "group-dequeued-first" {
								offer.group.sendGroupAdmitted(nil)
							}
						}
						return true, nil
					}
					if accepted, err := offer.send(channel, requestedAck); !accepted || err != nil || callback == nil {
						t.Fatalf("%s: public admission=%t %v callback=%t", label, accepted, err, callback != nil)
					}
					stats := requireConnectAdmissionEvidence(t, label+" public return", channel, true, wanted, nil)
					reason, held := blackholeReasonFromStats(clock, stats, 0, 0, bar, blackholeGates{
						transportDown: false, uplinkStale: false, unreliableSendTransport: false,
					})
					wantReason := blackholeNone
					if earlier || boundary != "public-return" {
						wantReason = blackholeNoReceiveSyn
					}
					if reason != wantReason || held != blackholeNone {
						t.Errorf("%s: public-return verdict=%s/%s, want %s", label, reason, held, wantReason)
					}
					if boundary != "terminal-before-return" {
						callback(nil)
					}
					stats = requireConnectAdmissionEvidence(t, label+" complete", channel, true, wanted, nil)
					if stats.sendNackCount != 0 || stats.sendNackByteCount != 0 || !hasPeerSendAckInWindow(stats) {
						t.Errorf("%s: terminal TCP accounting pending=%d/%d peerACK=%t", label,
							stats.sendNackCount, stats.sendNackByteCount, hasPeerSendAckInWindow(stats))
					}
				}
			}
		}
	}
}
