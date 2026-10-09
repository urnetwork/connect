package connect

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// The keyed partitioning every directory channel releases records by
// (EXTENDER.md Q2, Q3), in the shape of Psiphon's classic discovery: a secret
// hmac places each record in one of about sqrt(n) partitions, places each
// vantage -- a resolver prefix, a feed client's prefix, an authenticated
// identity -- in one partition too, and the epoch picks a small rotating
// sample within it. A vantage therefore sees at most its partition however
// often it polls, and the sample size only sets how fast it gets there.
//
// A record's partition is keyed by its identity key, not its position, so it
// is stable while the partition count is: churn moves nothing that stays. The
// count is the power of two at or above ceil(sqrt(n)), so it changes only when
// the fleet quadruples or quarters, and every placement is dealt again when it
// does. Each channel has its own key space -- the same record is in unrelated
// partitions on dns, on the feed and on the gated tier -- so a partition that
// leaks on one channel says nothing about the others, which is what makes a
// canary's placement attributable (Q4).
//
// Everything here is a pure function of its arguments. The secret is the
// operator's for the dns sets and the gated tier, and a per-process one for
// the feed an extender serves; it is never on the wire.

// The hmac domain of every partition hash.
const ExtenderPartitionDomain = "ur-extender-partition-v1"

// The channels, each its own key space.
const (
	ExtenderChannelDns   = "dns"
	ExtenderChannelFeed  = "feed"
	ExtenderChannelGated = "gated"
)

// The epoch of the open channels (Q2): the dns sets and the feed sample are
// redrawn within a vantage's partition this often. Psiphon rotates hourly.
const ExtenderOpenEpochTimeout = time.Hour

// Addresses per open answer (Q2): what a dns set and a feed sample hand one
// vantage per epoch. Small on purpose; the partition is the bound and this is
// the pace.
const ExtenderOpenSampleCount = 3

// The epoch index of a time: the count of whole epochs since the unix epoch.
// A timeout <= 0 is one epoch forever.
func ExtenderEpoch(now time.Time, epochTimeout time.Duration) uint64 {
	if epochTimeout <= 0 {
		return 0
	}
	epochMs := epochTimeout.Milliseconds()
	if epochMs <= 0 {
		return 0
	}
	nowMs := now.UnixMilli()
	if nowMs < 0 {
		return 0
	}
	return uint64(nowMs / epochMs)
}

// The fewest records a partition is cut for. A fleet too small to give every
// partition this many is split less, down to one partition: a client of a
// fleet of five that saw one record would have no failover at all, and a
// fleet that small is enumerated by anyone who looks regardless.
const ExtenderPartitionMinSize = 4

// The partition count of a fleet of `recordCount`: the power of two at or
// above ceil(sqrt(n)), but no more than leaves ExtenderPartitionMinSize per
// partition, and at least 1.
func ExtenderPartitionCount(recordCount int) int {
	if recordCount < 2*ExtenderPartitionMinSize {
		return 1
	}
	root := int(math.Ceil(math.Sqrt(float64(recordCount))))
	count := 1
	for count < root {
		count *= 2
	}
	for ExtenderPartitionMinSize < count && recordCount < count*ExtenderPartitionMinSize {
		count /= 2
	}
	if recordCount < count*ExtenderPartitionMinSize {
		count /= 2
	}
	return max(count, 1)
}

// The hmac of the domain, the channel and each part, every part length
// prefixed so no two argument lists share bytes.
func extenderPartitionSum(secret []byte, channel string, parts ...[]byte) []byte {
	mac := hmac.New(sha256.New, secret)
	write := func(part []byte) {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(part)))
		mac.Write(length[:])
		mac.Write(part)
	}
	write([]byte(ExtenderPartitionDomain))
	write([]byte(channel))
	for _, part := range parts {
		write(part)
	}
	return mac.Sum(nil)
}

// The leading 64 bits of a sum, reduced to [0, n).
func extenderPartitionIndex(sum []byte, n int) int {
	if n <= 1 {
		return 0
	}
	return int(binary.BigEndian.Uint64(sum[0:8]) % uint64(n))
}

// The partition of one record on one channel.
func ExtenderRecordPartition(secret []byte, channel string, keyHex string, partitionCount int) int {
	return extenderPartitionIndex(
		extenderPartitionSum(secret, channel, []byte("record"), []byte(strings.ToLower(keyHex))),
		partitionCount,
	)
}

// The partition of one vantage on one channel. A nil vantage is a vantage of
// its own, so a channel that cannot tell its callers apart still serves one
// bounded partition.
func ExtenderVantagePartition(secret []byte, channel string, vantage []byte, partitionCount int) int {
	return extenderPartitionIndex(
		extenderPartitionSum(secret, channel, []byte("vantage"), vantage),
		partitionCount,
	)
}

// The records of a vantage's partition on one channel, in key order, with the
// partition and the count. `keyHexes` is the whole channel pool, in any order,
// repeats included. An empty partition hands the vantage the next one that is
// not, around the ring, so a small fleet never answers nothing: the vantage
// is still bound to one partition, just not the one its hash named.
func ExtenderPartitionMembers(
	secret []byte,
	channel string,
	vantage []byte,
	keyHexes []string,
) (members []string, partition int, partitionCount int) {
	pool := slices.Clone(keyHexes)
	for i, keyHex := range pool {
		pool[i] = strings.ToLower(keyHex)
	}
	slices.Sort(pool)
	pool = slices.Compact(pool)
	partitionCount = ExtenderPartitionCount(len(pool))
	partition = ExtenderVantagePartition(secret, channel, vantage, partitionCount)
	recordPartitions := make([]int, len(pool))
	for i, keyHex := range pool {
		recordPartitions[i] = ExtenderRecordPartition(secret, channel, keyHex, partitionCount)
	}
	for offset := range partitionCount {
		candidate := (partition + offset) % partitionCount
		members = []string{}
		for i, keyHex := range pool {
			if recordPartitions[i] == candidate {
				members = append(members, keyHex)
			}
		}
		if 0 < len(members) {
			return members, candidate, partitionCount
		}
	}
	return []string{}, partition, partitionCount
}

// The order one vantage draws a partition's records in during one epoch: by
// the hmac of the vantage, the epoch and the key, ascending, the key breaking
// a tie. A prefix of it is the epoch's sample; the next epoch deals the same
// partition in another order.
func ExtenderPartitionOrder(
	secret []byte,
	channel string,
	vantage []byte,
	epoch uint64,
	members []string,
) []string {
	var epochBytes [8]byte
	binary.BigEndian.PutUint64(epochBytes[:], epoch)
	type rank struct {
		keyHex string
		sum    []byte
	}
	ranks := make([]rank, 0, len(members))
	for _, keyHex := range members {
		keyHex = strings.ToLower(keyHex)
		ranks = append(ranks, rank{
			keyHex: keyHex,
			sum:    extenderPartitionSum(secret, channel, []byte("order"), vantage, epochBytes[:], []byte(keyHex)),
		})
	}
	slices.SortFunc(ranks, func(a rank, b rank) int {
		if c := slices.Compare(a.sum, b.sum); c != 0 {
			return c
		}
		return strings.Compare(a.keyHex, b.keyHex)
	})
	ordered := make([]string, 0, len(ranks))
	for _, r := range ranks {
		ordered = append(ordered, r.keyHex)
	}
	return ordered
}

// Up to `count` records of a vantage's partition for one epoch: the members
// (ExtenderPartitionMembers) in the epoch's order (ExtenderPartitionOrder),
// cut to `count`. What a channel with no eligibility of its own answers with.
func ExtenderPartitionSample(
	secret []byte,
	channel string,
	vantage []byte,
	epoch uint64,
	keyHexes []string,
	count int,
) []string {
	if count <= 0 {
		return []string{}
	}
	members, _, _ := ExtenderPartitionMembers(secret, channel, vantage, keyHexes)
	ordered := ExtenderPartitionOrder(secret, channel, vantage, epoch, members)
	if count < len(ordered) {
		ordered = ordered[:count]
	}
	return ordered
}

// The vantage of one address on an open channel: its /24 for v4 and its /48
// for v6, under a family byte, so every address of one subscriber's prefix is
// one vantage and a poller cannot change its partition by changing its last
// octets. An invalid address is the nil vantage.
func ExtenderVantageKey(ip netip.Addr) []byte {
	if !ip.IsValid() {
		return nil
	}
	ip = ip.Unmap()
	if ip.Is4() {
		ipBytes := ip.As4()
		return append([]byte{4}, ipBytes[0:3]...)
	}
	ipBytes := ip.As16()
	return append([]byte{6}, ipBytes[0:6]...)
}

// The vantage of a connection's remote address (ExtenderVantageKey), nil for
// an address that carries no ip, such as a pipe's.
func ExtenderVantageKeyOfAddr(addr net.Addr) []byte {
	if addr == nil {
		return nil
	}
	addrPort, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		ip, err := netip.ParseAddr(addr.String())
		if err != nil {
			return nil
		}
		return ExtenderVantageKey(ip)
	}
	return ExtenderVantageKey(addrPort.Addr())
}

// The vantage of a prefix as a request counter keys it (Q3): the family and
// the prefix of ExtenderVantageKey as text, which is what stands in for the
// requester's asn where none is known.
func ExtenderVantagePrefix(ip netip.Addr) string {
	if !ip.IsValid() {
		return ""
	}
	ip = ip.Unmap()
	if ip.Is4() {
		return netip.PrefixFrom(ip, 24).Masked().String()
	}
	return netip.PrefixFrom(ip, 48).Masked().String()
}
